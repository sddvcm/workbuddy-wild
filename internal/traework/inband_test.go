package traework

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// 上游把错误塞进 200 正文时的真实形态（2026-10-02 抓取）。
const realInBandError = "data: {\"choices\":[{\"delta\":{\"content\":\"solo error code=4001 msg=We're sorry, the param is invalid. Please try with a valid param.\"},\"finish_reason\":\"stop\",\"index\":0}],\"created\":1790928286,\"id\":\"chatcmpl-1\",\"model\":\"\",\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"

// ★ D2 回归：信封型错误（event:error + message 为空）的真实形态（2026-10-02 抓取）。
//
// 历史实现只按「错误文案」匹配，而这类帧的 message 恒为空、正文里根本没有
// 可匹配的文案，导致 kimi-k3 的 code:1005 被**漏过**并当作正常回答透传。
const realEnvelopeError = "event:error\ndata:{\"code\":1005,\"message\":\"\",\"data\":null}\n\n"

func TestClassifyInBandErrorIsBadModel(t *testing.T) {
	// 上游真实文案（大小写混合）必须被判为请求方参数错误，而不是账号故障。
	for _, body := range []string{
		"solo error code=4001 msg=We're sorry, the param is invalid. Please try with a valid param.",
		"SOLO ERROR CODE=4001",
		"The Param Is Invalid.",
	} {
		if k := Classify(http.StatusBadRequest, body); k != provider.ErrBadModel {
			t.Fatalf("body=%q 分类=%v，期望 ErrBadModel", body, k)
		}
	}
	// 不能误伤：普通 400 仍应是 ErrClient（会累计 errCount）
	if k := Classify(http.StatusBadRequest, `{"msg":"bad request"}`); k != provider.ErrClient {
		t.Fatalf("普通 400 应判为 ErrClient，实际 %v", k)
	}
}

// ★ D2 回归：信封型错误必须被识别，且 1005 归入权益不足（长冷却）。
//
// 关键点：这条路径**不能**依赖 message 文案（它是空的），
// 只能靠 event:error 事件名识别，再由 Classify 按 code 细分。
func TestEnvelopeErrorIsCaught(t *testing.T) {
	// 1) Classify 直接按归一化后的 body 细分：1005 → 权益不足
	if k := Classify(http.StatusBadRequest, `{"code":1005,"message":"","data":null}`); k != provider.ErrHardCredit {
		t.Fatalf("code:1005 应判为 ErrHardCredit，实际 %v", k)
	}

	// 2) 端到端：200 + 信封 → ChatStream 归一化成 400，且分类正确
	c, srv := newChatTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, realEnvelopeError)
	})
	defer srv.Close()

	rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"traework/kimi-k3"}`))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if rc != nil {
		t.Fatalf("命中信封错误时不应返回可读流")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d，期望 400", status)
	}
	// 上层据此长冷却该账号（权益不足），而不是把它当正常回答。
	if k := Classify(status, string(body)); k != provider.ErrHardCredit {
		t.Fatalf("归一化后分类=%v，期望 ErrHardCredit（body=%q）", k, string(body))
	}
}

// 信封型错误也必须是**非流式聚合**路径上的错误，不能变成"模型的回答"。
func TestEnvelopeErrorFailsAggregate(t *testing.T) {
	_, err := Aggregate(strings.NewReader(realEnvelopeError))
	if err == nil {
		t.Fatal("信封型错误应让 Aggregate 返回 error，而不是产出空回答")
	}
	var se *SOLOStreamError
	if !errors.As(err, &se) || se.Code != 1005 {
		t.Fatalf("应返回 code=1005 的 SOLOStreamError，实际 %v", err)
	}
	if k := se.Kind(); k != provider.ErrHardCredit {
		t.Fatalf("Kind=%v，期望 ErrHardCredit", k)
	}
}

// 流内错误码到类别的映射必须与「换账号有没有用」对齐。
func TestSOLOStreamErrorKindMapping(t *testing.T) {
	cases := []struct {
		code int64
		want provider.ErrKind
	}{
		{1005, provider.ErrHardCredit}, // 权益/套餐不足
		{1001, provider.ErrBadModel},   // 模型不可用 → 换账号无用
		{4001, provider.ErrBadModel},   // 参数非法
		{400, provider.ErrBadModel},    // 参数非法
		{9999, provider.ErrClient},     // 未知 → 保守按账号问题
	}
	for _, c := range cases {
		got := (&SOLOStreamError{Code: c.code, Msg: "x"}).Kind()
		if got != c.want {
			t.Fatalf("code=%d 分类=%v，期望 %v", c.code, got, c.want)
		}
	}
}

func newChatTestClient(handler http.HandlerFunc) (*Client, *httptest.Server) {
	srv := httptest.NewServer(handler)
	c := New()
	c.HTTP = srv.Client()
	c.AgentHost = srv.URL
	return c, srv
}

// 核心回归：HTTP 200 + 正文内错误 → ChatStream 必须归一化成 400，
// 而不是把错误文本当作正常流交给上层（否则客户端会显示成"模型的回答"）。
func TestChatStreamInBandErrorBecomes400(t *testing.T) {
	c, srv := newChatTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, realInBandError)
	})
	defer srv.Close()

	rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"traework/nope"}`))
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if rc != nil {
		t.Fatalf("命中正文内错误时不应返回可读流")
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status=%d，期望 400", status)
	}
	if !strings.Contains(string(body), "solo error") {
		t.Fatalf("错误正文应保留原文，实际 %q", string(body))
	}
	// 上层据此判定：不冷却账号、立即终止轮换
	if k := Classify(status, string(body)); k != provider.ErrBadModel {
		t.Fatalf("归一化后分类=%v，期望 ErrBadModel", k)
	}
}

// 正常模型不受影响：流内容必须完整（含预读回放部分）。
func TestChatStreamNormalStreamIsIntact(t *testing.T) {
	const normal = "data: {\"choices\":[{\"delta\":{\"content\":\"po\"},\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ng\"},\"index\":0}],\"object\":\"chat.completion.chunk\"}\n\ndata: [DONE]\n\n"
	c, srv := newChatTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, normal)
	})
	defer srv.Close()

	rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"traework/glm-5"}`))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if status != http.StatusOK || body != nil {
		t.Fatalf("status=%d body=%q，期望 200 且无错误体", status, string(body))
	}
	if rc == nil {
		t.Fatal("正常流不应为 nil")
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = rc.Close()
	if string(got) != normal {
		t.Fatalf("流内容被破坏:\n got=%q\nwant=%q", string(got), normal)
	}
}

// 4xx 路径保持原行为（不经过嗅探）。
func TestChatStream4xxUnchanged(t *testing.T) {
	c, srv := newChatTestClient(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"code":401,"msg":"token invalid"}`)
	})
	defer srv.Close()

	rc, status, body, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if rc != nil || status != http.StatusUnauthorized {
		t.Fatalf("rc=%v status=%d", rc, status)
	}
	if k := Classify(status, string(body)); k != provider.ErrSessionDead {
		t.Fatalf("分类=%v，期望 ErrSessionDead", k)
	}
}

// --- 流式层：上游 event:error 必须以 OpenAI 标准错误事件下发 ---

// recWriter 是最小的 http.ResponseWriter + Flusher 记录器。
type recWriter struct {
	hdr    http.Header
	body   []byte
	status int
}

func (r *recWriter) Header() http.Header         { return r.hdr }
func (r *recWriter) WriteHeader(s int)           { r.status = s }
func (r *recWriter) Write(p []byte) (int, error) { r.body = append(r.body, p...); return len(p), nil }
func (r *recWriter) Flush()                      {}
func (r *recWriter) String() string              { return string(r.body) }

// ★ 核心回归：错误帧**不能**伪装成 delta.content。
//
// 旧实现输出 delta.content="solo error code=1005 msg=" + finish_reason=stop，
// 客户端会把它渲染成一段"模型的回答"，用户看到像答了、实则是报错。
// 现在必须是标准错误事件：`event: error` + `data: {"error":{...}}`。
func TestStreamErrorEventIsNotFakeContent(t *testing.T) {
	const upstream = "event:output\ndata:{\"response\":\"po\"}\n\n" + realEnvelopeError
	w := &recWriter{hdr: http.Header{}}

	var got *SOLOStreamError
	if err := StreamWithError(w, strings.NewReader(upstream), func(e *SOLOStreamError) { got = e }); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := w.String()

	// 1) 必须有标准错误事件
	if !strings.Contains(out, "event: error") {
		t.Fatalf("缺少 event: error 错误事件:\n%s", out)
	}
	if !strings.Contains(out, `"error"`) || !strings.Contains(out, `"code":"1005"`) {
		t.Fatalf("错误事件缺少标准 error 结构:\n%s", out)
	}
	// 2) 错误**不得**出现在任何 delta.content 里
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"content"`) &&
			strings.Contains(line, "1005") {
			t.Fatalf("错误仍被塞进 delta.content:/n%s", line)
		}
	}
	// 3) 仍要有 [DONE] 收尾，避免客户端挂起
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatalf("缺少 [DONE]:\n%s", out)
	}
	// 4) onErr 回调必须带上原始错误码，供上层冷却/日志
	if got == nil || got.Code != 1005 {
		t.Fatalf("onErr 回调异常: %+v", got)
	}
	if k := got.Kind(); k != provider.ErrHardCredit {
		t.Fatalf("回调分类=%v，期望 ErrHardCredit", k)
	}
}

// 晚到的 event:error（正常 output 之后）也必须被识别，且分类正确。
func TestStreamErrorEventArrivesLate(t *testing.T) {
	const upstream = "event:output\ndata:{\"response\":\"po\"}\n\n" +
		"event:output\ndata:{\"response\":\"ng\"}\n\n" + realEnvelopeError
	w := &recWriter{hdr: http.Header{}}
	if err := Stream(w, strings.NewReader(upstream)); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	out := w.String()
	if !strings.Contains(out, "event: error") {
		t.Fatalf("晚到的错误帧未被识别:\n%s", out)
	}
	// 正常内容仍应正常下发
	if !strings.Contains(out, `"content":"po"`) {
		t.Fatalf("正常内容丢失:\n%s", out)
	}
}
