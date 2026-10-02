// Package server 暴露 OpenAI 兼容 HTTP 接口，按模型名前缀路由到不同上游。
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// Runtime 是一个平台的一组运行时资源：pool + upstream + 静态模型兜底。
type Runtime struct {
	Kind         provider.Kind
	Pool         *pool.Pool
	Upstream     provider.Upstream
	StaticModels []provider.ModelInfo

	mu       sync.RWMutex
	models   []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

// Config handler 依赖。
type Config struct {
	Runtimes map[provider.Kind]*Runtime
	APIKey   string // 空 = 不鉴权

	// 兼容旧调用方：只传 Pool/Upstream 时等价于只启用 workbuddy。
	Pool     *pool.Pool
	Upstream provider.Upstream

	MaxRotate    int
	HardCooldown time.Duration
	SoftCooldown time.Duration
	ErrThreshold int
	ErrCooldown  time.Duration
	RefreshSkew  time.Duration
}

// Handler 主路由。
type Handler struct {
	cfg Config
	mux *http.ServeMux

	apiMu sync.RWMutex // 保护 cfg.APIKey（面板可运行时修改）
}

func NewHandler(cfg Config) *Handler {
	if cfg.Runtimes == nil && cfg.Pool != nil && cfg.Upstream != nil {
		cfg.Runtimes = map[provider.Kind]*Runtime{
			provider.WorkBuddy: {Kind: provider.WorkBuddy, Pool: cfg.Pool, Upstream: cfg.Upstream, StaticModels: WorkBuddyStaticModels()},
		}
	}
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.HardCooldown <= 0 {
		cfg.HardCooldown = 12 * time.Hour
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 60 * time.Second
	}
	if cfg.ErrThreshold <= 0 {
		cfg.ErrThreshold = 3
	}
	if cfg.ErrCooldown <= 0 {
		cfg.ErrCooldown = 10 * time.Minute
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if key := h.currentAPIKey(); key != "" {
			authz := r.Header.Get("Authorization")
			if !strings.HasPrefix(authz, "Bearer ") || strings.TrimPrefix(authz, "Bearer ") != key {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

func (h *Handler) SetAPIKey(key string) { h.apiMu.Lock(); defer h.apiMu.Unlock(); h.cfg.APIKey = key }
func (h *Handler) currentAPIKey() string {
	h.apiMu.RLock()
	defer h.apiMu.RUnlock()
	return h.cfg.APIKey
}
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	accounts := map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		accounts[k.String()] = rt.Pool.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": accounts})
}

var workbuddyStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2", ContextWindow: 131072}, {ID: "glm-5.1", ContextWindow: 131072}, {ID: "glm-5v-turbo", ContextWindow: 131072},
	{ID: "kimi-k2.7", ContextWindow: 131072}, {ID: "minimax-m3", ContextWindow: 131072}, {ID: "hy3", ContextWindow: 131072},
	{ID: "hy3-preview", ContextWindow: 131072}, {ID: "hy3-preview-agent", ContextWindow: 131072},
	{ID: "deepseek-v4-pro", ContextWindow: 131072}, {ID: "deepseek-v4-flash", ContextWindow: 131072},
}

var traeworkStaticModels = []provider.ModelInfo{
	{ID: "glm-5.2"}, {ID: "glm-5-turbo"}, {ID: "glm-5"}, {ID: "DeepSeek-V4-Pro"}, {ID: "DeepSeek-V4-Flash"},
	{ID: "kimi-k2.6"}, {ID: "kimi-k2.7-code"}, {ID: "minimax-m3"}, {ID: "qwen3-coder"}, {ID: "Doubao-Seed-2.1-Pro"},
}

// dynamicModelsCache 保留给旧测试/旧单平台语义；实际多平台缓存放在 Runtime 内。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []provider.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": h.modelList()})
}

func (h *Handler) modelList() []map[string]any {
	out := []map[string]any{}
	for _, k := range h.runtimeKinds() {
		rt := h.cfg.Runtimes[k]
		if rt.Pool == nil || len(rt.Pool.List()) == 0 { // 只暴露已接入账号的平台
			continue
		}
		infos := h.fetchRuntimeModels(rt)
		if len(infos) == 0 {
			infos = rt.StaticModels
		}
		for _, mi := range infos {
			id := k.String() + "/" + mi.ID
			entry := map[string]any{"id": id, "object": "model", "created": 1753600000, "owned_by": k.String()}
			if mi.ContextWindow > 0 {
				entry["context_length"] = mi.ContextWindow
			}
			if mi.MaxTokens > 0 {
				entry["max_output_tokens"] = mi.MaxTokens
			}
			// 说明该 id 支持在名称后追加人类可读备注（如「（倍率0.11x）」），
			// 备注会在发给上游前自动剥离。客户端可选择展示给用户。
			entry["name"] = id
			entry["annotation_supported"] = true
			out = append(out, entry)
		}
	}
	return out
}

func (h *Handler) fetchRuntimeModels(rt *Runtime) []provider.ModelInfo {
	if rt.Kind == provider.WorkBuddy { // 兼容旧单平台缓存观察点
		dynamicModelsCache.RLock()
		if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
			out := dynamicModelsCache.ids
			dynamicModelsCache.RUnlock()
			return out
		}
		if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
			dynamicModelsCache.RUnlock()
			return nil
		}
		dynamicModelsCache.RUnlock()
	}
	rt.mu.RLock()
	if len(rt.models) > 0 && time.Since(rt.fetched) < dynamicModelsTTL {
		out := rt.models
		rt.mu.RUnlock()
		return out
	}
	if rt.Kind != provider.WorkBuddy && !rt.lastFail.IsZero() && time.Since(rt.lastFail) < modelsFetchFailCooldown {
		rt.mu.RUnlock()
		return nil
	}
	rt.mu.RUnlock()
	acct := rt.Pool.Pick()
	if acct == nil {
		return nil
	}
	infos, err := rt.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		now := time.Now()
		rt.mu.Lock()
		rt.lastFail = now
		rt.mu.Unlock()
		if rt.Kind == provider.WorkBuddy {
			dynamicModelsCache.Lock()
			dynamicModelsCache.lastFail = now
			dynamicModelsCache.Unlock()
		}
		return nil
	}
	now := time.Now()
	rt.mu.Lock()
	rt.models = infos
	rt.fetched = now
	rt.lastFail = time.Time{}
	rt.mu.Unlock()
	if rt.Kind == provider.WorkBuddy { // 兼容旧测试观察点
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids = infos
		dynamicModelsCache.fetched = now
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
	}
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var peek struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	_ = json.Unmarshal(body, &peek)
	rt, model, err := h.runtimeForModel(peek.Model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_model", err.Error())
		return
	}
	body, err = rewriteModel(body, model)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	tried := map[string]bool{}
	var lastErr error
	// rotate 标签用于"模型名错误"这种**换账号也没用**的场景提前退出，
	// 避免把整个账号池都白试一遍（每次白试都会污染一个账号的 errCount）。
rotate:
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := rt.Pool.PickExcluding(tried)
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		// 标记"正在调用"：记录本次调用账号与时间戳，供面板高亮显示。
		rt.Pool.NotifyUsed(acct.UID)
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			log.Printf("refresh start platform=%s uid=%s reason=request", rt.Kind, acct.UID)
			if err := rt.Upstream.RefreshToken(acct); err != nil {
				log.Printf("refresh failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
				lastErr = err
				var ue *provider.Error
				if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
					rt.Pool.Disable(acct.UID, "refresh session dead")
				} else {
					rt.Pool.Cooldown(acct.UID, pool.CoolErr, h.cfg.ErrCooldown, "refresh: "+err.Error())
				}
				continue
			}
			if err := acct.SaveAtomic(); err != nil {
				log.Printf("refresh save failed platform=%s uid=%s err=%v", rt.Kind, acct.UID, err)
			}
			log.Printf("refresh success platform=%s uid=%s expires_at=%d", rt.Kind, acct.UID, acct.ExpiresAt)
		}
		rc, status, respBody, terr := rt.Upstream.ChatStream(acct, body)
		if terr != nil {
			lastErr = terr
			rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			continue
		}
		if status >= 400 {
			kind := rt.Upstream.Classify(status, string(respBody))
			switch kind {
			case provider.ErrHardCredit:
				rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "余额/权益不足")
			case provider.ErrSoftRate:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "429 rate limit")
			case provider.ErrSessionDead:
				rt.Pool.Disable(acct.UID, "session dead")
			case provider.ErrNotFound:
				rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "upstream 404")
			case provider.ErrBadModel:
				// 模型 ID 不存在 —— 这是**请求方参数错误**，换任何账号结果都一样。
				// 因此：不计 errCount、不冷却账号，并立刻终止轮换。
				// （否则一次写错模型名就会把整个账号池连续冷却 10 分钟。）
				lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
				break rotate
			default:
				rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
			}
			lastErr = &provider.Error{Kind: kind, Status: status, Msg: string(respBody)}
			continue
		}
		defer rc.Close()
		rt.Pool.NoteSuccess(acct.UID)
		if peek.Stream {
			// 流式路径：上游可能在流**中途**才发错误（超出嗅探预读窗口的
			// 场景）。StreamErrors 让实现方把这类错误回调出来，
			// 以便按类别冷却账号 —— 否则错误只以 SSE 事件形式下发给
			// 客户端，账号状态却仍被记为"成功"。
			if se, ok := rt.Upstream.(provider.StreamErrorReporter); ok {
				_ = se.StreamWithError(w, rc, func(kind provider.ErrKind, msg string) {
					switch kind {
					case provider.ErrHardCredit:
						rt.Pool.Cooldown(acct.UID, pool.CoolHard, h.cfg.HardCooldown, "流内错误: "+msg)
					case provider.ErrSoftRate, provider.ErrNotFound:
						rt.Pool.Cooldown(acct.UID, pool.CoolSoft, h.cfg.SoftCooldown, "流内错误: "+msg)
					case provider.ErrSessionDead:
						rt.Pool.Disable(acct.UID, "流内 session dead")
					case provider.ErrBadModel:
						// 请求方错误：与账号无关，不冷却、不记错
						log.Printf("stream in-band bad-model uid=%s: %s", acct.UID, msg)
					default:
						rt.Pool.NoteError(acct.UID, h.cfg.ErrThreshold, h.cfg.ErrCooldown)
					}
				})
				return
			}
			_ = rt.Upstream.Stream(w, rc)
			return
		}
		resp, err := rt.Upstream.Aggregate(rc)
		if err != nil {
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	msg := "all accounts unavailable (cooling/disabled)"
	if lastErr != nil {
		msg += ": " + lastErr.Error()
	}
	// 请求方参数错误（模型 ID 不存在 / 参数非法）：以 400 明确返回并指路，
	// 不要伪装成"账号不可用"（会让人误以为是账号问题，且会冤枉冷却整个账号池）。
	if lastErr != nil {
		var ue *provider.Error
		if errors.As(lastErr, &ue) && ue.Kind == provider.ErrBadModel {
			writeOpenAIError(w, http.StatusBadRequest, "model_not_available", fmt.Sprintf(
				"model %q is not available upstream (http %d). 上游按「模型 ID」精确匹配且区分大小写，"+
					"请填 /v1/models 返回的 id（而不是客户端展示的名字）；若确认 id 正确，则说明该请求参数不被上游接受。"+
					"上游原始响应: %s",
				peek.Model, ue.Status, ue.Msg))
			return
		}
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, "no_healthy_account", msg)
}

// modelAnnotations 是允许出现在模型名之后的**备注包裹符**（成对使用）。
//
// 用途：让用户能在客户端里给模型写人类可读的备注，便于区分同名模型，例如
//
//	workbuddy/deepseek-v4-flash（倍率0.11x）
//	workbuddy/deepseek-v4-flash (rate 0.11x)
//	traework/glm-5.2【便宜】
//
// 备注内容随意填，程序在**调用上游前**会把它剥掉，
// 因此不影响任何实际请求；模型列表里展示的是带备注的名字 +
// 标准 id 字段（客户端可自行选择显示哪个）。
var modelAnnotations = [][2]string{
	{"（", "）"}, // 全角圆括号（中文输入法默认）
	{"(", ")"}, // 半角圆括号
	{"【", "】"}, // 全角方括号
	{"[", "]"}, // 半角方括号
	{"「", "」"}, // 全角引号
}

// stripModelAnnotation 去掉模型名末尾的备注部分。
//
// 规则：从**第一个**出现的注释起始符切起，只保留它之前的部分。
// 这样「（倍率0.11x）」「(x)」「【便宜】(备用)」都能一次剥净。
//
// 若剥离后为空（例如用户只写了「（备注）」而没写模型名），
// 则原样返回，交由上层报「模型名不合法」，避免静默变成空串。
func stripModelAnnotation(model string) string {
	s := strings.TrimSpace(model)
	cut := -1
	for _, pair := range modelAnnotations {
		if i := strings.Index(s, pair[0]); i >= 0 {
			if cut < 0 || i < cut {
				cut = i
			}
		}
	}
	if cut <= 0 {
		// cut < 0：没有备注；cut == 0：整串都是备注，保留原样让上层报错
		return s
	}
	return strings.TrimSpace(s[:cut])
}

func (h *Handler) runtimeForModel(model string) (*Runtime, string, error) {
	raw := strings.TrimSpace(model)
	parts := strings.SplitN(raw, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, "", fmt.Errorf("model must use explicit prefix: workbuddy/<model> or traework/<model>")
	}
	kind := provider.Kind(parts[0])
	rt := h.cfg.Runtimes[kind]
	if rt == nil || rt.Pool == nil || rt.Upstream == nil {
		return nil, "", fmt.Errorf("provider %q is not configured", kind)
	}
	if len(rt.Pool.List()) == 0 {
		return nil, "", fmt.Errorf("provider %q has no account", kind)
	}
	// 剥掉人类可读的备注（如「（倍率0.11x）」）——备注仅供辨认，不发给上游。
	// 例如 workbuddy/deepseek-v4-flash（倍率0.11x） → deepseek-v4-flash
	name := stripModelAnnotation(parts[1])
	if name == "" {
		return nil, "", fmt.Errorf("model name is empty after removing annotation: %q", raw)
	}
	return rt, name, nil
}

func rewriteModel(body []byte, model string) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, err
	}
	obj["model"] = model
	return json.Marshal(obj)
}

func (h *Handler) runtimeKinds() []provider.Kind {
	ks := make([]provider.Kind, 0, len(h.cfg.Runtimes))
	for k := range h.cfg.Runtimes {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool { return ks[i] < ks[j] })
	return ks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "api_error", "code": code}})
}

func WorkBuddyStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, workbuddyStaticModels...)
}
func TraeWorkStaticModels() []provider.ModelInfo {
	return append([]provider.ModelInfo{}, traeworkStaticModels...)
}
