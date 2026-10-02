package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// replayCloser 把「已预读的字节 + 剩余流」拼回一个 ReadCloser，
// 对调用方完全透明（Close 时关闭底层流）。
type replayCloser struct {
	io.Reader
	c io.Closer
}

func (r *replayCloser) Close() error { return r.c.Close() }

// InBandErrorLookup 判断一段流前缀是否命中「正文内错误」。
// 返回 (命中的错误消息, 是否命中)。
type InBandErrorLookup func(prefix string) (string, bool)

// SniffInBandError 预读流的开头，用于识别**HTTP 200 但正文里夹着错误**的上游形态。
//
// 背景：部分上游（实测 traework 的 llm_utils_chat）在请求参数/模型非法时
// 不返回 4xx，而是返回 200 + 一条正常形状的 SSE，把错误文本塞进 content：
//
//	data: {"choices":[{"delta":{"content":"solo error code=4001 msg=...the param is invalid"}}]}
//
// 若不识别，这行错误会被当作模型的回答原样透传给客户端。
//
// 行为保证：
//   - 命中 → 关闭流并返回 (nil, 消息, true)，调用方应按错误处理；
//   - 未命中 → 返回可正常读取的 ReadCloser，已预读的字节会被先回放，
//     流内容一字节不丢（因此对正常请求无副作用，只增加首字节前的等待）；
//   - 最多预读 limit 字节 / 最多等待 wait 时长，任一超限即放行，
//     避免拖慢首字节交付很慢的模型。
func SniffInBandError(rc io.ReadCloser, lookup InBandErrorLookup, limit int, wait time.Duration) (io.ReadCloser, string, bool) {
	if limit <= 0 {
		limit = 64 << 10
	}
	if wait <= 0 {
		wait = 8 * time.Second
	}

	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 2048)
	deadline := time.Now().Add(wait)
	pending := false // 已命中错误标记，等待正文收全

	for len(buf) < limit && time.Now().Before(deadline) {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)

			if !pending {
				if _, hit := lookup(string(buf)); hit {
					pending = true
				}
			}
			if pending {
				// 标记是**边收边匹配**的，此刻 JSON 可能还是半截
				// （错误文本会跨 chunk）。必须等到能解析出完整的
				// content 再返回，否则给出的错误消息是截断的。
				if msg, ok := firstSSEContentStrict(string(buf)); ok {
					_ = rc.Close()
					return nil, msg, true
				}
			}

			// 确认是正常流后提前放行，避免无谓等待。
			// 要求事件完整（以空行结尾），否则跨 chunk 的错误文本会被
			// 误判成正常内容。
			if !pending {
				if idx := bytes.LastIndex(buf, []byte("\n\n")); idx >= 0 {
					head := buf[:idx]
					if bytes.Contains(head, []byte(`"content"`)) ||
						bytes.Contains(head, []byte(`"reasoning_content"`)) ||
						bytes.Contains(head, []byte(`"finish_reason"`)) {
						break
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if pending {
		// 流中断也没能收全 —— 仍按错误处理，消息退回原文片段。
		_ = rc.Close()
		return nil, FirstSSEContent(string(buf)), true
	}
	return &replayCloser{Reader: io.MultiReader(bytes.NewReader(buf), rc), c: rc}, "", false
}

// FirstSSEContent 从 SSE 文本里取出第一个非空的 content / reasoning_content，
// 取不到时回退为原文片段（截断到 300 字符）。
func FirstSSEContent(raw string) string {
	if s, ok := firstSSEContentStrict(raw); ok {
		return s
	}
	return TruncateText(strings.TrimSpace(raw), 300)
}

// firstSSEContentStrict 只在能**完整解析**出某个事件的内容时返回 ok=true。
// 用于区分「已拿到完整的错误文本」与「只是碰巧匹配到了半截标记」。
func firstSSEContentStrict(raw string) (string, bool) {
	for _, seg := range strings.Split(raw, "data:") {
		s := strings.TrimSpace(seg)
		if s == "" || s == "[DONE]" {
			continue
		}
		var o struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(s), &o) != nil || len(o.Choices) == 0 {
			continue
		}
		c := o.Choices[0]
		if txt := strings.TrimSpace(c.Delta.Content); txt != "" {
			return txt, true
		}
		if txt := strings.TrimSpace(c.Delta.ReasoningContent); txt != "" {
			return txt, true
		}
		if txt := strings.TrimSpace(c.Message.Content); txt != "" {
			return txt, true
		}
	}
	return "", false
}

// TruncateText 按字节截断并保留可读前缀。
func TruncateText(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
