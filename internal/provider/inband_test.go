package provider

import (
	"io"
	"strings"
	"testing"
	"time"
)

// sseReader 把字符串切成长度 pieces 的碎片返回，用于模拟跨 chunk 的 SSE。
func sseReader(s string, pieces int) io.ReadCloser {
	if pieces <= 1 {
		return io.NopCloser(strings.NewReader(s))
	}
	size := (len(s) + pieces - 1) / pieces
	var chunks []string
	for i := 0; i < len(s); i += size {
		end := i + size
		if end > len(s) {
			end = len(s)
		}
		chunks = append(chunks, s[i:end])
	}
	readers := make([]io.Reader, 0, len(chunks))
	for _, c := range chunks {
		readers = append(readers, strings.NewReader(c))
	}
	return io.NopCloser(io.MultiReader(readers...))
}

const soloErrorSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"solo error code=4001 msg=We're sorry, the param is invalid. Please try with a valid param.\"},\"finish_reason\":\"stop\",\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"

const normalSSE = "data: {\"choices\":[{\"delta\":{\"content\":\"pong\"},\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"

func lookup(prefix string) (string, bool, bool) {
	lower := strings.ToLower(prefix)
	for _, m := range []string{"solo error code=", "the param is invalid"} {
		if strings.Contains(lower, m) {
			return FirstSSEContent(prefix), true, false
		}
	}
	return "", false, false
}

// 命中：错误文案出现在正文里 → 必须被识别出来，并给出可读消息。
func TestSniffInBandErrorHits(t *testing.T) {
	msg, hit := func() (string, bool) {
		rc, m, h := SniffInBandError(sseReader(soloErrorSSE, 1), lookup, 64<<10, 2*time.Second)
		if rc != nil {
			t.Fatalf("hit 时应返回 nil reader")
		}
		return m, h
	}()
	if !hit {
		t.Fatal("未识别出正文内错误")
	}
	if !strings.Contains(msg, "solo error") {
		t.Fatalf("消息应包含原文，实际 %q", msg)
	}
}

// 关键回归：错误文本被拆到多个 chunk 时仍要识别出来
// （若只看"是否出现过 content 字段"就提前放行，会漏判）。
func TestSniffInBandErrorAcrossChunks(t *testing.T) {
	for _, pieces := range []int{2, 3, 7, 20, 60} {
		rc, msg, hit := SniffInBandError(sseReader(soloErrorSSE, pieces), lookup, 64<<10, 2*time.Second)
		if !hit {
			t.Fatalf("碎片数 %d 时未识别出正文内错误", pieces)
		}
		if rc != nil {
			t.Fatalf("碎片数 %d 命中时应返回 nil reader", pieces)
		}
		if !strings.Contains(msg, "param is invalid") {
			t.Fatalf("碎片数 %d 消息不含原文: %q", pieces, msg)
		}
	}
}

// 未命中：正常流必须**一字节不丢**地放行（MultiReader 回放预读部分）。
func TestSniffInBandErrorPassThroughIsLossless(t *testing.T) {
	for _, pieces := range []int{1, 3, 50} {
		rc, msg, hit := SniffInBandError(sseReader(normalSSE, pieces), lookup, 64<<10, 2*time.Second)
		if hit {
			t.Fatalf("碎片数 %d 被误判为错误: %s", pieces, msg)
		}
		got, err := io.ReadAll(rc)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(got) != normalSSE {
			t.Fatalf("碎片数 %d 流内容被破坏:\n got=%q\nwant=%q", pieces, string(got), normalSSE)
		}
		if err := rc.Close(); err != nil {
			t.Fatalf("close: %v", err)
		}
	}
}

// 正常回答里恰好出现 "error" 这类普通词不应被误判。
func TestSniffInBandErrorNoFalsePositiveOnOrdinaryText(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"An error occurred in the demo code sample.\"},\"index\":0}]}\n\ndata: [DONE]\n\n"
	rc, msg, hit := SniffInBandError(sseReader(sse, 2), lookup, 64<<10, 2*time.Second)
	if hit {
		t.Fatalf("普通文本被误判: %s", msg)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != sse {
		t.Fatalf("内容被破坏: %q", string(got))
	}
}

func TestFirstSSEContent(t *testing.T) {
	if got := FirstSSEContent(soloErrorSSE); !strings.HasPrefix(got, "solo error code=4001") {
		t.Fatalf("提取错误: %q", got)
	}
	if got := FirstSSEContent("not sse at all"); got != "not sse at all" {
		t.Fatalf("应回退为原文: %q", got)
	}
}
