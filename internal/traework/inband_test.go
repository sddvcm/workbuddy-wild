package traework

import (
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
