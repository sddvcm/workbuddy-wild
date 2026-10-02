package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/upstream"
)

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:      "https://fake.example",
		BillingBaseCN:   "https://fake.example",
		ChatBaseGlobal:  "https://fake.example",
		BillingBaseGlob: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"workbuddy/glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, HardCooldown: time.Hour, SoftCooldown: time.Minute, ErrThreshold: 3, ErrCooldown: 10 * time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestModelsEndpoint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) < 5 {
		t.Errorf("models count=%d", len(data))
	}
	found := false
	for _, m := range data {
		if m.(map[string]any)["id"] == "workbuddy/glm-5.2" {
			found = true
		}
	}
	if !found {
		t.Error("workbuddy/glm-5.2 missing")
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["workbuddy/dyn-model-a"] || !ids["workbuddy/glm-9.9"] {
		t.Errorf("dynamic ids missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "workbuddy/dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
		case "workbuddy/glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

func TestModelsDynamicFallsBackToStatic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游 500
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	// 回退静态表（≥5 个）
	if len(data) < 5 {
		t.Errorf("static fallback failed: %d", len(data))
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 其余走静态 fallback（仍返回 200）。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if calls != 1 {
		t.Errorf("want 1 fetch, got %d", calls)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if calls != 2 {
		t.Errorf("want 2 fetch after cooldown, got %d", calls)
	}
}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
}

func TestHealthz(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("code=%d", rec.Code)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// ---- 模型名备注（annotation）支持 ----
//
// 需求：允许在模型名后追加人类可读的备注，便于在客户端里区分同名模型，
// 例如 workbuddy/deepseek-v4-flash（倍率0.11x）。
// 备注必须在下发上游前被剥离，且不影响前缀路由。

func TestStripModelAnnotation(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// 需求里举的例子
		{"deepseek-v4-flash（倍率0.11x）", "deepseek-v4-flash"},
		// 各种括号（全角/半角/方/引号）
		{"deepseek-v4-flash(rate 0.11x)", "deepseek-v4-flash"},
		{"glm-5.2【便宜】", "glm-5.2"},
		{"glm-5.2[cheap]", "glm-5.2"},
		{"glm-5.2「备用」", "glm-5.2"},
		// 多段备注一次剥净（取第一个起始符之前）
		{"glm-5.2（倍率0.11x）(备用)", "glm-5.2"},
		// 备注在中间也剥（从第一个起始符切开）
		{"kimi-k2.7（a）extra", "kimi-k2.7"},
		// 无备注：原样返回
		{"deepseek-v4-flash", "deepseek-v4-flash"},
		// 前后空白应清理
		{"  glm-5.2  ", "glm-5.2"},
		// 只有备注（没写模型名）：原样返回，交由上层报错，不能变空串
		{"（倍率0.11x）", "（倍率0.11x）"},
		// 模型名本身含连字符/点/数字，不受影响
		{"Doubao-Seed-2.1-Pro（贵）", "Doubao-Seed-2.1-Pro"},
		{"kimi-k2.7-code（code专用）", "kimi-k2.7-code"},
	}
	for _, c := range cases {
		if got := stripModelAnnotation(c.in); got != c.want {
			t.Errorf("stripModelAnnotation(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// 端到端：带备注的模型名必须能正确路由，且**发给上游的 model 字段不含备注**。
func TestChatCompletionsStripsModelAnnotation(t *testing.T) {
	var gotModel string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		// 上游收到的请求体里应当是剥离备注后的干净模型名
		return 200, sseOK, true
	})
	// 用自定义 Transport 截获请求体里的 model
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		gotModel, _ = obj["model"].(string)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}

	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/deepseek-v4-flash（倍率0.11x）","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotModel != "deepseek-v4-flash" {
		t.Fatalf("发给上游的 model = %q，备注未被剥离（want deepseek-v4-flash）", gotModel)
	}
}

// 各种括号形式都要能被端到端剥离。
func TestChatCompletionsAnnotationForms(t *testing.T) {
	cases := []struct{ in, want string }{
		{"workbuddy/glm-5.2【便宜】", "glm-5.2"},
		{"workbuddy/glm-5.2(cheap)", "glm-5.2"},
		{"workbuddy/kimi-k2.7「备用」", "kimi-k2.7"},
		{"traework/glm-5.2（倍率低）", "glm-5.2"},
	}
	for _, c := range cases {
		var gotModel string
		up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
		up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			var obj map[string]any
			_ = json.Unmarshal(b, &obj)
			gotModel, _ = obj["model"].(string)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})}
		kind := provider.WorkBuddy
		if strings.HasPrefix(c.in, "traework/") {
			kind = provider.TraeWork
		}
		h := NewHandler(Config{
			Runtimes: map[provider.Kind]*Runtime{
				kind: {Kind: kind, Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+c.in+`","messages":[{"role":"user","content":"hi"}]}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if gotModel != c.want {
			t.Errorf("%s -> 上游 model=%q, want %q (code=%d)", c.in, gotModel, c.want, rec.Code)
		}
	}
}

// 回归：不带备注的模型名照常工作（不能因为新增注释逻辑而破坏原有路径）。
func TestChatCompletionsPlainModelStillWorks(t *testing.T) {
	var gotModel string
	up := newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true })
	up.HTTP = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(b, &obj)
		gotModel, _ = obj["model"].(string)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(sseOK)),
		}, nil
	})}

	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if gotModel != "glm-5.2" {
		t.Fatalf("model=%q, want glm-5.2", gotModel)
	}
}

// ---- 模型 ID 不存在：不得拖累账号池 ----
//
// 回归背景（真实事故）：上游按「模型 ID」精确匹配且区分大小写。
// 用户在 models.json 里填了上游的**显示名**（如 Deepseek-V4.1-Flash），
// 而上游真实 ID 是 deepseek-v4.1-flash，于是每次请求都返回
//
//	400 {"code":11102,"msg":"model [Deepseek-V4.1-Flash] service info not found"}
//
// 旧实现把 11102 当"其他 4xx"，走 default 分支 -> NoteError。
// 每次请求轮换 3 个账号、各记 1 次错误，第 3 轮就把 5 个 workbuddy 账号
// 全部冷却 10 分钟（面板显示"冷却中（consecutive errors 至 …）"）。
//
// 正确行为：这是**请求方参数错误**，换任何账号都一样 ->
//  1. 不计 errCount、不冷却账号
//  2. 立即停止轮换（只试 1 次）
//  3. 以 400 + model_not_available 明确告知，而不是 503 "账号不可用"
func TestBadModelDoesNotCooldownAccounts(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11102,"msg":"model [Deepseek-V4.1-Flash] service info not found"}`, false
	})
	a1 := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	a2 := &auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999}
	a3 := &auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999}
	p := testPoolWith(a1, a2, a3)
	// ErrThreshold=1：若仍会计数，第一次就能把账号冷却，问题立刻暴露。
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 3, ErrThreshold: 1, ErrCooldown: time.Hour})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/Deepseek-V4.1-Flash","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("模型不存在应返回 400，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_available") {
		t.Errorf("错误码应为 model_not_available，实际 body=%s", rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("应只试 1 次即停止（换号无用），实际调用上游 %d 次", calls)
	}
	for _, au := range []*auth.Auth{a1, a2, a3} {
		st, ok := p.Status(au.UID)
		if !ok {
			t.Fatalf("账号 %s 丢失", au.UID)
		}
		if st.Cooling {
			t.Errorf("账号 %s 被错误冷却（reason=%q until=%v）—— 模型名错误与账号无关",
				au.UID, st.Reason, st.Until)
		}
		if st.Disabled {
			t.Errorf("账号 %s 被错误禁用", au.UID)
		}
		if st.ErrCount != 0 {
			t.Errorf("账号 %s errCount=%d，应为 0", au.UID, st.ErrCount)
		}
	}
}

// 对照测试：**普通** 4xx（不是模型不存在）仍必须走 errCount -> 冷却。
// 防止上面的修复把账号保护机制一并改坏（"别的账号的错"仍要隔离）。
func TestGenericClientErrorStillCooldownsAccounts(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":9999,"msg":"some unrelated bad request"}`, false
	})
	a1 := &auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}
	p := testPoolWith(a1)
	h := NewHandler(Config{Pool: p, Upstream: up, MaxRotate: 1, ErrThreshold: 1, ErrCooldown: time.Hour})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"workbuddy/glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Errorf("普通 4xx 达到阈值后应冷却账号，实际 cooling=%v reason=%q", st.Cooling, st.Reason)
	}
}
