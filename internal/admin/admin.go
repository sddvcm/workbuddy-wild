// Package admin 提供网页版账号管理面板：在浏览器里直接登录/添加/删除账号，
// 不必再进 NAS 终端跑 login.sh。
//
// 设计要点：
//   - 面板与 OpenAI 接口同端口同进程（默认 http://<NAS>:7863/admin/）
//   - 登录流程全程在服务端内存里推进（state 文件落在 auth_dir 旁边的 tmp），
//     前端只负责「拿链接 → 用户去登录 → 点一下轮询」三步
//   - 写操作（增删账号）用独立的 AdminToken 鉴权，默认复用 API_KEY；
//     若 API_KEY 为空则面板无鉴权（并会显著警告）
//   - 面板可以「重新加载账号」：把 auth 目录重新读一遍并同步进 pool，
//     免去重启容器
package admin

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/login"
	"github.com/rockswang/workbuddy-wild/internal/login_trae"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

//go:embed page.html
var pageHTML string

//go:embed app.js
var appJS string

//go:embed style.css
var styleCSS string

//go:embed favicon.svg
var faviconSVG string

// Config 面板依赖。
type Config struct {
	AuthDir string
	Region  string // workbuddy 账号按此区域过滤
	// Pools 按平台索引，用于列表与重新加载。
	Pools map[provider.Kind]*pool.Pool
	// Token 面板鉴权用；为空则不鉴权（仅限内网）。
	Token string
}

// Handler 面板路由。
type Handler struct {
	cfg   Config
	csrf  string // 进程级随机串，写操作必须带上，防跨站
	mu    sync.Mutex
	flows map[string]*flow // 进行中的登录流程，key = flowID
}

// flow 是一次进行中的登录流程。
type flow struct {
	Kind      string // "workbuddy" | "traework"
	StatePath string
	DeviceID  string
	CreatedAt time.Time
	AuthURL   string
}

func New(cfg Config) *Handler {
	if cfg.Pools == nil {
		cfg.Pools = map[provider.Kind]*pool.Pool{}
	}
	return &Handler{cfg: cfg, csrf: randHex(16), flows: map[string]*flow{}}
}

// Register 把面板挂到给定 mux 上。
// 面板路径固定 /admin/ 与 /admin/api/*，不影响 /v1/* 与 /status。
func (h *Handler) Register(mux *http.ServeMux) {
	// 注意：Go 1.22 ServeMux 对 "/admin/" 模式会自动加 301 重定向把 /admin → /admin/，
	// 因此这里不要再显式注册 "/admin"（会和内置重定向冲突，且我们的 handler 不会触发）。
	mux.HandleFunc("GET /admin/", h.page)
	mux.HandleFunc("GET /admin/app.js", h.appJS)
	mux.HandleFunc("GET /admin/style.css", h.styleCSS)
	mux.HandleFunc("GET /admin/favicon.svg", h.favicon)

	mux.HandleFunc("GET /admin/api/state", h.apiState)
	mux.HandleFunc("POST /admin/api/login/workbuddy/start", h.apiWbStart)
	mux.HandleFunc("POST /admin/api/login/workbuddy/poll", h.apiWbPoll)
	mux.HandleFunc("POST /admin/api/login/traework/start", h.apiTwStart)
	mux.HandleFunc("POST /admin/api/login/traework/poll", h.apiTwPoll)
	mux.HandleFunc("POST /admin/api/accounts/delete", h.apiDelete)
	mux.HandleFunc("POST /admin/api/accounts/reload", h.apiReload)
	mux.HandleFunc("POST /admin/api/accounts/checkin", h.apiCheckin)
}

// ---------- 鉴权 ----------

// guard 校验面板写操作。返回 false 表示已写响应、调用方应直接 return。
func (h *Handler) guard(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg.Token != "" {
		// 优先 X-Admin-Token，其次 ?token=，再其次 Bearer
		tok := r.Header.Get("X-Admin-Token")
		if tok == "" {
			tok = r.URL.Query().Get("token")
		}
		if tok == "" {
			tok = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if tok != h.cfg.Token {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "error": "未授权：请先填入正确的 API Key"})
			return false
		}
	}
	if r.Method == http.MethodPost {
		if r.Header.Get("X-CSRF") != h.csrf {
			writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "CSRF 校验失败，请刷新页面"})
			return false
		}
	}
	return true
}

// ---------- 页面资源（直接内嵌，零外部依赖）----------

func (h *Handler) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 用固定字符串占位符替换（别用子串会被误伤的短名字）：
	// 之前用 "__CSRF__" 做占位符，会把 JS 里的变量名 window.__CSRF__ 一起换掉。
	html := strings.ReplaceAll(pageHTML, "__WB2A_CSRF_TOKEN__", h.csrf)
	html = strings.ReplaceAll(html, "__WB2A_AUTH_REQUIRED__", boolJS(h.cfg.Token != ""))
	_, _ = w.Write([]byte(html))
}

func (h *Handler) appJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	_, _ = w.Write([]byte(appJS))
}

func (h *Handler) styleCSS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	_, _ = w.Write([]byte(styleCSS))
}

func (h *Handler) favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(faviconSVG))
}

func boolJS(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// ---------- API ----------

func (h *Handler) apiState(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	accounts := map[string]any{}
	for kind, p := range h.cfg.Pools {
		if p == nil {
			continue
		}
		accounts[kind.String()] = p.List()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"accounts":  accounts,
		"auth_dir":  h.cfg.AuthDir,
		"region":    h.cfg.Region,
		"auth_req":  h.cfg.Token != "",
		"server_tz": time.Now().Format("2006-01-02 15:04:05 -07:00"),
	})
}

// apiWbStart 发起 WorkBuddy 登录：拿到授权 URL。
func (h *Handler) apiWbStart(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	client := login.NewClient()
	id := randHex(8)
	sp := h.statePath("workbuddy", id)
	url, err := login.Start(client, sp)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// 尽量把中间跳转展开，让用户直接打开最终页
	if resolved, rerr := login.ResolveAuthURL(client, url); rerr == nil && resolved != "" {
		url = resolved
	}
	h.mu.Lock()
	h.flows[id] = &flow{Kind: "workbuddy", StatePath: sp, CreatedAt: time.Now(), AuthURL: url}
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flow_id": id, "url": url})
}

// apiWbPoll 轮询 WorkBuddy 登录结果；成功即落盘账号文件。
func (h *Handler) apiWbPoll(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	var req struct{ FlowID string `json:"flow_id"` }
	_ = json.NewDecoder(r.Body).Decode(&req)
	f := h.getFlow(req.FlowID, "workbuddy")
	if f == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "流程已失效，请重新获取授权链接"})
		return
	}
	client := login.NewClient()
	res, err := login.Poll(client, f.StatePath)
	if err != nil {
		if err == login.ErrPending {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pending": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	fp, err := login.SaveAuth(h.cfg.AuthDir, res)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "写入账号失败: " + err.Error()})
		return
	}
	h.dropFlow(req.FlowID)
	h.reloadPools()
	log.Printf("[admin] WorkBuddy 账号已添加: uid=%s nickname=%s -> %s", res.UID, res.Nickname, fp)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "done": true, "uid": res.UID, "nickname": res.Nickname, "file": fp,
	})
}

// apiTwStart 发起 TraeWork 登录：需要设备号。
func (h *Handler) apiTwStart(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	var req struct {
		DeviceID string `json:"device_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	client := login_trae.NewClient()
	id := randHex(8)
	sp := h.statePath("traework", id)
	url, err := login_trae.StartLocal(client, sp, req.DeviceID, "")
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	h.mu.Lock()
	h.flows[id] = &flow{Kind: "traework", StatePath: sp, DeviceID: req.DeviceID, CreatedAt: time.Now(), AuthURL: url}
	h.mu.Unlock()
	warn := ""
	if strings.TrimSpace(req.DeviceID) == "" {
		warn = "未填设备号，已随机生成；签到可能因设备校验失败（9074）。建议填入 Trae 客户端真实 16 位设备号。"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "flow_id": id, "url": url, "warning": warn})
}

// apiTwPoll 用用户粘贴的回调 URL 完成 TraeWork 登录。
func (h *Handler) apiTwPoll(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	var req struct {
		FlowID      string `json:"flow_id"`
		CallbackURL string `json:"callback_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f := h.getFlow(req.FlowID, "traework")
	if f == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "流程已失效，请重新获取授权链接"})
		return
	}
	if strings.TrimSpace(req.CallbackURL) == "" {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "请粘贴浏览器地址栏里的完整回调 URL"})
		return
	}
	client := login_trae.NewClient()
	res, err := login_trae.PollCallback(client, f.StatePath, req.CallbackURL)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	fp, err := login_trae.SaveAuth(h.cfg.AuthDir, res)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "写入账号失败: " + err.Error()})
		return
	}
	h.dropFlow(req.FlowID)
	h.reloadPools()
	log.Printf("[admin] TraeWork 账号已添加: uid=%s nickname=%s -> %s", res.UID, res.Nickname, fp)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "done": true, "uid": res.UID, "nickname": res.Nickname, "file": fp,
	})
}

// apiDelete 删除账号：移走 auth 文件 + 从 pool 摘除。
func (h *Handler) apiDelete(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	var req struct {
		Kind string `json:"kind"`
		UID  string `json:"uid"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	kind := provider.Kind(req.Kind)
	p := h.cfg.Pools[kind]
	if p == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "未知平台: " + req.Kind})
		return
	}
	// 先从 pool 摘除
	p.Remove(req.UID)

	// 再删磁盘文件。文件名格式固定为 <prefix><uid>.json，直接按名匹配即可。
	prefix := kind.String() + "-"
	if kind == provider.TraeWork {
		prefix = "trae-"
	}
	removed := ""
	candidates := []string{
		filepath.Join(h.cfg.AuthDir, prefix+req.UID+".json"),
		filepath.Join(h.cfg.AuthDir, prefix+req.UID+".json.tmp"),
	}
	for _, fp := range candidates {
		if err := os.Remove(fp); err == nil {
			removed = fp
		}
	}
	// 兜底：文件名含 uid 的也清掉（防止手工改名残留）
	if removed == "" {
		entries, _ := os.ReadDir(h.cfg.AuthDir)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if !strings.HasPrefix(e.Name(), prefix) && !strings.HasPrefix(e.Name(), "workbuddy-") && !strings.HasPrefix(e.Name(), "trae-") {
				continue
			}
			if strings.Contains(e.Name(), req.UID) {
				fp := filepath.Join(h.cfg.AuthDir, e.Name())
				if err := os.Remove(fp); err == nil {
					removed = fp
				}
				break
			}
		}
	}
	log.Printf("[admin] 删除账号 kind=%s uid=%s file=%s", req.Kind, req.UID, removed)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": removed})
}

// apiReload 重新读取 auth 目录并同步进 pool（不用重启容器）。
func (h *Handler) apiReload(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	n, err := h.reloadPools()
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "workbuddy": n[0], "traework": n[1]})
}

// apiCheckin 手动触发一次签到。
func (h *Handler) apiCheckin(w http.ResponseWriter, r *http.Request) {
	if !h.guard(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"note":  "签到由容器内定时器按 09:00 / 21:00 自动执行；手动签到请查看日志 docker logs workbuddy-wild",
	})
}

// ---------- 内部工具 ----------

func (h *Handler) statePath(kind, id string) string {
	dir := filepath.Join(filepath.Dir(h.cfg.AuthDir), "login_state")
	_ = os.MkdirAll(dir, 0o700)
	return filepath.Join(dir, kind+"-"+id+".json")
}

func (h *Handler) getFlow(id, kind string) *flow {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.flows[id]
	if f == nil || f.Kind != kind {
		return nil
	}
	// 超过 30 分钟的流程视为过期
	if time.Since(f.CreatedAt) > 30*time.Minute {
		delete(h.flows, id)
		return nil
	}
	return f
}

func (h *Handler) dropFlow(id string) {
	h.mu.Lock()
	delete(h.flows, id)
	h.mu.Unlock()
}

// reloadPools 重新读取 auth 目录，并把结果同步进各 platform 的 pool。
// 返回 [workbuddy 数量, traework 数量]。
func (h *Handler) reloadPools() ([2]int, error) {
	var out [2]int
	wb, err := auth.LoadWorkBuddyDir(h.cfg.AuthDir, h.cfg.Region)
	if err != nil {
		return out, fmt.Errorf("加载 workbuddy 账号失败: %w", err)
	}
	tr, err := auth.LoadTraeDir(h.cfg.AuthDir)
	if err != nil {
		return out, fmt.Errorf("加载 traework 账号失败: %w", err)
	}
	if p := h.cfg.Pools[provider.WorkBuddy]; p != nil {
		p.SyncToDir(wb)
	}
	if p := h.cfg.Pools[provider.TraeWork]; p != nil {
		p.SyncToDir(tr)
	}
	out[0], out[1] = len(wb), len(tr)
	return out, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
