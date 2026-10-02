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
//
// 命中时返回 (错误消息, 是否命中, 是否可立即返回)。
// 第三个返回值 drop 为 true 表示「分类已确定、无需等待正文收全」——
// 用于信封类错误（如 event:error + message 为空），此时消息可能为空，
// 提前返回可避免白等一个 wait 周期。
type InBandErrorLookup func(prefix string) (msg string, hit bool, drop bool)

// InBandPattern 描述一类「HTTP 200 但正文里夹着错误」的形态。
//
// 上游（实测 traework）在请求非法/权益不足时**不返回 4xx**，而是返回
// 200 + 形状正常的 SSE，把错误塞进 data 里。这类响应若不识别，
// 错误文本会被当作模型的回答透传给客户端（比报错更糟）。
//
// 不同错误的**判别方式不同**，因此这里把「怎么认」和「算哪类错」分开：
//   - Contains：正文里出现该片段即命中（用于有独特文案的错误）；
//   - Event：SSE 事件名（如 "error"）命中即命中（用于 message 可能为空的
//     信封类错误，只能靠事件名判别 —— 例如 traework 的 code:1005）；
//   - Drop 为 true 时命中后**立刻**返回，不必等正文收全（信封类错误
//     的消息可能在后续事件里，但分类只依赖事件名，早返回可减少延迟）。
type InBandPattern struct {
	// Name 仅用于日志/测试可读性。
	Name string
	// Contains 命中片段（小写比较）。与 Event 二者其一即可。
	Contains []string
	// Event 命中即命中的 SSE 事件名（精确匹配，大小写不敏感）。
	Event string
	// Kind 命中后应归入的错误类别。
	Kind ErrKind
	// Drop 命中后立即返回，不等正文收全。
	Drop bool
}

// sseEventName 判断 raw 中是否出现了 `event:<name>` 事件行。
// 要求整行就是 `event:<name>`（允许前后空白），避免匹配到正文里的巧合文本。
func sseEventName(raw, name string) bool {
	needle := "event:" + strings.ToLower(name)
	for _, ln := range strings.Split(raw, "\n") {
		if strings.ToLower(strings.TrimSpace(ln)) == needle {
			return true
		}
	}
	return false
}

// SSEEventName 是 sseEventName 的导出包装，供 traework 等上游包复用。
func SSEEventName(raw, name string) bool { return sseEventName(raw, name) }

// minSniffBeforeRelease 是"提前放行"所需的最小预读字节数。
//
// 背景：上游可能在若干条正常 output **之后**才发 event:error。
// 若看到第一条 content 就立刻 break，后到的错误帧会被漏掉。
// 因此要求至少读满这么多字节才允许提前放行 —— 起首的
// progress_notice / metadata / timing_cost 等事件会把这部分额度自然吃掉，
// 从而在几乎不增加延迟的前提下，给错误帧留出被读到的机会。
//
// 取值权衡：太小则漏判（错误帧后到），太大则正常请求首字节延迟增加
// （最多多等一次 Read）。8KB 覆盖实测的上游前置事件序列。
const minSniffBeforeRelease = 8 << 10

// SniffInBandError 预读流的开头，用于识别**HTTP 200 但正文里夹着错误**的上游形态。
//
// 背景：部分上游（实测 traework 的 llm_utils_chat）在请求参数/模型非法、
// 或账号权益不足时**不返回 4xx**，而是返回 200 + 一条正常形状的 SSE，
// 把错误塞进正文：
//
//	data: {"choices":[{"delta":{"content":"solo error code=4001 msg=...invalid"}}]}
//
//	event:error
//	data:{"code":1005,"message":"","data":null}
//
// 若不识别，这些内容会被当作模型的回答原样透传给客户端。
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
				if msg, hit, drop := lookup(string(buf)); hit {
					pending = true
					// 信封类错误（如 event:error + message 为空）没有可等待的
					// 正文，立刻返回，避免白等一个 wait 周期。
					if drop {
						_ = rc.Close()
						return nil, msg, true
					}
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
			//
			// ★ 安全前提（2026-10-02 修正）：上游可能在正常 output 之后**才**发
			// event:error（实测 traework 存在这种时序）。因此放行前必须确认
			// 「已读字节里不含错误信封」—— 否则提前 break 会漏掉后到的错误帧，
			// 让错误被当作正常回答透传。
			//
			// 同时要求事件完整（以空行结尾），否则跨 chunk 的错误文本会被
			// 误判成正常内容。
			if !pending && len(buf) >= minSniffBeforeRelease {
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
