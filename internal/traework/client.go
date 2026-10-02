package traework

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"

	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

var sessionDeadMarkers = []string{"login", "token 失效", "token invalid", "session", "unauthorized", "401"}

// inBandErrorMarkers 是 traework **把错误塞进 HTTP 200 正文**时使用的文案片段。
//
// ★ 实测（2026-10-02）：给 traework 传一个上游不存在的模型 ID，
// 上游不返回 4xx，而是返回 200 + 一条形状完全正常的 SSE：
//
//	data: {"choices":[{"delta":{"content":"solo error code=4001 msg=We're sorry,
//	       the param is invalid. Please try with a valid param."},"finish_reason":"stop"}]}
//
// 若不识别，这行错误文本会被当成模型的回答显示给用户 —— 比报错更糟（看起来像答了）。
//
// 定性：这是**请求方参数错误**（模型 ID 不存在 / 参数非法），
// 换账号、重试都无用，因此归入 provider.ErrBadModel（不冷却账号）。
//
// 选词刻意保守，只取足够独特的片段，避免误伤正常回答里恰好出现的词。
var inBandErrorMarkers = []string{"solo error code=", "the param is invalid"}

// inBandPatterns 声明 traework 200 正文里可能出现的**各类**错误。
//
// ★ 两套错误形态必须分开判别（2026-10-02 实测）：
//
//	形态 A：文本型，塞在 content 里，有独特文案
//	  data: {"choices":[{"delta":{"content":"solo error code=4001 msg=..."}}]}
//	  → 靠文案片段识别（Contains），且**必须等正文收全**才能拿到完整消息。
//
//	形态 B：信封型，走 SSE 的 event:error，message 恒为空
//	  event:error
//	  data:{"code":1005,"message":"","data":null}
//	  → 靠**事件名**识别（Event）。若仍按文案匹配，message 为空时无从匹配，
//	    错误会被漏过并当成正常回答（曾实际发生：kimi-k3 的 code:1005 漏过）。
//
// 顺序有意义：形态 A 在前（有具体消息，优先返回更多人可读的文案）。
var inBandPatterns = []provider.InBandPattern{
	{
		Name:     "solo-text",
		Contains: inBandErrorMarkers,
		Kind:     provider.ErrBadModel,
	},
	{
		Name:  "sse-error-envelope",
		Event: "error",
		Kind:  provider.ErrUnknown, // 实际类别由 Classify 按内容细分
		Drop:  true,                // 信封本身即判据，无需等正文
	},
}

// inBandErrorLookup 供 provider.SniffInBandError 使用的判定函数。
func inBandErrorLookup(prefix string) (string, bool, bool) {
	lower := strings.ToLower(prefix)
	for _, p := range inBandPatterns {
		if p.Event != "" && provider.SSEEventName(prefix, p.Event) {
			// 信封类：消息可能为空，交给 Classify 按 code 细分。
			return "", true, p.Drop
		}
		for _, m := range p.Contains {
			if strings.Contains(lower, strings.ToLower(m)) {
				return provider.FirstSSEContent(prefix), true, false
			}
		}
	}
	return "", false, false
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) provider.ErrKind {
	lower := strings.ToLower(body)
	// 正文内错误（由 SniffInBandError 归一化成 400 + 原文）优先判定。
	for _, m := range inBandErrorMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return provider.ErrBadModel
		}
	}
	// traework 权益/套餐不足：实为 SSE event:error 的 code=1005，message 为空。
	// 归一化后 body 形如 {"code":1005,"message":"","data":null}，
	// 必须按 code 判定 —— 文案为空时任何关键词匹配都无效。
	if strings.Contains(body, `"code":1005`) || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return provider.ErrHardCredit
	}
	if status == http.StatusUnauthorized {
		for _, m := range sessionDeadMarkers {
			if strings.Contains(lower, strings.ToLower(m)) {
				return provider.ErrSessionDead
			}
		}
		return provider.ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return provider.ErrSoftRate
	}
	if status == http.StatusNotFound {
		return provider.ErrNotFound
	}
	if status >= 500 {
		return provider.ErrServer
	}
	// ★ 嗅探命中后统一归一化成 400，因此这里要把「确实是错误但还没细分」
	// 的 400 与「真正的账号无关请求方错误」区分开：
	//   - 有 event:error 信封特征 / 有具体业务 code → 请求方错误
	//     （模型不可用、参数非法等），换账号无用 → ErrBadModel
	//   - 其余 4xx → ErrClient（可能累计 errCount）
	// 注意 1005 已在上面被截走（权益不足 → ErrHardCredit）。
	if status == http.StatusBadRequest {
		if strings.Contains(body, `"code":`) || provider.SSEEventName(body, "error") {
			return provider.ErrBadModel
		}
	}
	if status >= 400 {
		return provider.ErrClient
	}
	return provider.ErrNone
}

// CheckinRateLimitCode TraeWork 签到业务码 9074。
//
// 文案是"当前参与用户太多，请稍后再试"。
//
// ⚠️ 语义经过两次修正，最终结论（2026-10-02）：**它是瞬时限流，可重试**。
//
//	v0.5.4        当作"高峰限流"，做指数退避重试 —— 方向对但实现差（最坏 60s）
//	v0.5.7 ~ 0.6.8 定性为"设备号未注册、重试无用" —— **错判**
//	v0.6.9        确认为**瞬时限流**：同一操作稍后重试即可成功
//
// 推翻旧结论的证据（2026-10-02）：
//
//	09:00 程序用随机设备号轮换重试 → 9074（唯一一次，程序随即放弃）
//	同日手工用 8 个随机设备号重试   → 全部 code=0，成功 +100
//
// 即**同一操作有时成功、有时 9074**，且重试就能过 —— 这是限流的特征，
// 不是"设备号非法"（后者应稳定复现，重试无用）。
//
// 因此换设备号重试时遇到 9074 应当**继续换号再试**，绝不能就此放弃。
const CheckinRateLimitCode = 9074

// ErrCheckinRateLimited 签到业务码 9074。
//
// v0.5.4 曾把它当"高峰限流"做指数退避重试；v0.5.7 误改为"设备未注册、
// 重试无用"；v0.6.9 依据实测重新定性为**瞬时/偶发限流**。
//
// IsRateLimited() 仍返回 false —— 语义是"**不交由调度器安排延迟重试**"。
// 原因：本错误只在**轮换设备号的过程中**出现，此时已在请求内同步重试
// （最多 maxRotateAttempts 次），比交给调度器"稍后再试"更及时有效。
// 调度器的延迟重试队列只用于跨小时的补签场景，不适合这种秒级的偶发拒绝。
type ErrCheckinRateLimited struct {
	Attempts int
	Msg      string
}

func (e *ErrCheckinRateLimited) Error() string {
	return fmt.Sprintf("checkin 9074 (transient throttle): %s", e.Msg)
}

// IsRateLimited 返回 false：9074 不在调度器层安排延迟重试。
//
// 语义澄清（v0.6.9）：9074 **是**可重试的瞬时限流，但重试动作已在
// claimWithRotatedDevice 内部同步完成（换号最多 8 次）。此处返回 false
// 只是为了**不重复安排**调度器那套"稍后自动重试"，并非表示"重试无用"。
func (e *ErrCheckinRateLimited) IsRateLimited() bool { return false }

// IsCheckinRateLimited 报告错误是否为签到 9074（设备未注册）。
func IsCheckinRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var target *ErrCheckinRateLimited
	return errors.As(err, &target)
}

// Client Trae SOLO 上游 HTTP 客户端。
type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client
	AgentHost  string
	UgHost     string
	OAuthHost  string
	ClientID   string
}

func New() *Client {
	tr := &http.Transport{MaxIdleConns: 100, MaxIdleConnsPerHost: 20, IdleConnTimeout: 90 * time.Second, ResponseHeaderTimeout: 120 * time.Second}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr},
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }

func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &provider.Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}

func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	oldRefresh := a.RefreshToken
	log.Printf("traework refresh start uid=%s", a.UID)
	if strings.TrimSpace(a.RefreshToken) == "" {
		err := fmt.Errorf("no refreshToken")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ClientID": c.ClientID, "RefreshToken": a.RefreshToken, "ClientSecret": "-", "UserID": ""}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	OAuthHeaders(req)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("exchange parse: %w", err)
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	if resp.Result.Token == "" {
		err := fmt.Errorf("refresh_failed: no token in response — re-login required")
		log.Printf("traework refresh failed uid=%s err=%v", a.UID, err)
		return err
	}
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		d := time.Duration(resp.Result.TokenExpireDuration)
		if resp.Result.TokenExpireDuration > 1e9 { // 上游通常是毫秒
			d *= time.Millisecond
		} else {
			d *= time.Second
		}
		a.ExpiresAt = time.Now().Add(d).Unix()
	}
	log.Printf("traework refresh success uid=%s refresh_rotated=%t expires_at=%d", a.UID, a.RefreshToken != oldRefresh, a.ExpiresAt)
	return nil
}

func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}

func (c *Client) ChatStream(a *auth.Auth, body []byte) (rc io.ReadCloser, status int, respBody []byte, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(PrepareBody(body)))
	if err != nil {
		return nil, 0, nil, err
	}
	SOLOHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("traework chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("traework chat_stream uid=%s: upstream %d %s body=%s", a.UID, resp.StatusCode, kind, truncate(string(raw), 200))
		return nil, resp.StatusCode, raw, nil
	}
	// HTTP 200 也可能是错误（见 inBandPatterns）：预读开头若干字节做嗅探，
	// 命中则归一化成 400 交给上层 Classify；未命中则原样放行，流内容不丢。
	sniffed, msg, hit := provider.SniffInBandError(resp.Body, inBandErrorLookup, 64<<10, 8*time.Second)
	if hit {
		// 信封类错误的 message 恒为空，兜底补上分类名，避免上层拿到空字符串。
		if strings.TrimSpace(msg) == "" {
			msg = inBandEnvelopeFallback
		}
		log.Printf("traework chat_stream uid=%s: upstream 200 in-band error: %s", a.UID, truncate(msg, 200))
		return nil, http.StatusBadRequest, []byte(msg), nil
	}
	return sniffed, resp.StatusCode, nil, nil
}

// inBandEnvelopeFallback 是信封类错误（message 为空）的兜底文案。
// 必须**同时**带上分类关键信息，因为上层 Classify 是按 body 关键词
// 细分错误类别的（1005 → 长冷却 / 其余 → 账号无关的请求方错误）。
const inBandEnvelopeFallback = `{"code":1005,"message":"solo error: insufficient plan/credit (event:error)","data":null}`

func (c *Client) FetchModels(a *auth.Auth) ([]provider.ModelInfo, error) {
	// traework 上游 llm_utils_chat 强制 stream=true（见 PrepareBody），
	// 所有模型均为流式模式；非流式请求由本地 Aggregate() 缓冲 SSE 后聚合。
	// mode_type=nil 返回全部配置，按 config_name 去重避免流式/非流式重复。
	body := map[string]any{"function": Function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList []struct {
			ConfigName    string `json:"config_name"`
			DisplayConfig struct {
				DisplayName string `json:"display_name"`
			} `json:"display_config"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	// 按 config_name 去重：上游可能为同一模型返回流式/非流式两条配置。
	seen := make(map[string]bool, len(resp.ConfigInfoList))
	out := make([]provider.ModelInfo, 0, len(resp.ConfigInfoList))
	for _, cfg := range resp.ConfigInfoList {
		name := strings.TrimSpace(cfg.ConfigName)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, provider.ModelInfo{ID: name, Name: cfg.DisplayConfig.DisplayName})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list")
	}
	return out, nil
}

func (c *Client) CheckinStatusLegacyRemoved() {}

// CheckinClaim 领取签到额度。
//
// 修订（2026-09-30，单变量实测确认）：
//
//	v0.5.4–v0.5.5 把 9074 当"高峰限流"，做 4 次指数退避重试（8s→16s→32s）。
//	**方向错误**：9074 的真正含义是"设备号未被服务端认作注册设备"，
//	重试再多次也不会成功（实测连续重试恒返 9074）。
//
//	现在的做法：**不重试**，一次调用直接判定。
//	  9074     → 返回 *ErrCheckinRateLimited（文案说明要换真实设备号）
//	  0 / 9095 → 成功（9095 表示今日已签，由后置 status 验证兜底）
//
// 这样单账号签到耗时从最坏 ~60s 降到 ~1s，且不再给出"稍后自动重试"的空头承诺。
func (c *Client) CheckinClaim(a *auth.Auth) error {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte(CheckinClaimBody)))
	if err != nil {
		log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
		return err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
		return err
	}
	code, msg, success, err := parseCheckinResponse(data)
	if err != nil {
		log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
		return err
	}

	if code == CheckinRateLimitCode {
		// 9074 = 瞬时/偶发限流（**不是**"设备号未注册"）。
		//
		// ⚠️ v0.5.7~v0.6.8 曾把它定性为"设备号未注册、重试无用"并直接放弃，
		// 那是错判。2026-10-02 实测推翻：同一操作有时成功、有时 9074，
		// 且重试就能通过（详见 CheckinRateLimitCode 注释）。
		//
		// 因此这里**不再直接放弃**，而是同样走"换设备号重试"路径 ——
		// claimWithRotatedDevice 内部会对 9074 继续换号重试。
		log.Printf("traework checkin claim 9074 uid=%s device=%s（瞬时限流，尝试换号重试）",
			a.UID, shortDevice(a.DeviceID))
		if ok, altAmount := c.claimWithRotatedDevice(a); ok {
			log.Printf("traework checkin claim 9074-rotated-success uid=%s（换号重试后入账）", a.UID)
			_ = altAmount
			return nil
		}
		log.Printf("traework checkin claim throttle-exhausted uid=%s device=%s（多次换号仍被限流）",
			a.UID, shortDevice(a.DeviceID))
		return &ErrCheckinRateLimited{Attempts: 1, Msg: msg}
	}
	if code != 0 {
		// 9095 = 该账号在当前设备号上今日已领（或该设备号今日已被用过）。
		//
		// ⚠️ 实测（2026-10-01）这里存在一个可利用的规律：
		//   同一 token 下，把 X-Device-Id 换成**任意 16 位数字**再 claim，
		//   若该账号今天确实还没领到额度，就会真正入账（额度 +100）。
		//   原设备号被拒时换号成功 —— 已验证额度从 4600 变为 4700。
		//
		// 因此 9095 不再直接判定失败，而是**自动轮换设备号重试**。
		// 这样能救回"设备号被上游标记为今日已签、但账号当天尚未领到"的账号
		// （实测账号 2222575719809915 正是这种状态，用户反馈"一点也没增长"）。
		if code == CheckinAlreadyClaimedCode {
			log.Printf("traework checkin claim 9095 uid=%s device=%s（尝试轮换设备号重试）",
				a.UID, shortDevice(a.DeviceID))
			if ok, altAmount := c.claimWithRotatedDevice(a); ok {
				log.Printf("traework checkin claim 9095-rotated-success uid=%s（换设备号后成功）", a.UID)
				_ = altAmount
				return nil
			}
			log.Printf("traework checkin claim already-claimed uid=%s device=%s（轮换后仍被拒，判定今日已领）",
				a.UID, shortDevice(a.DeviceID))
			return &ErrCheckinAlreadyClaimed{Msg: msg, Device: shortDevice(a.DeviceID)}
		}
		err := fmt.Errorf("checkin claim code=%d msg=%s", code, msg)
		log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
		return err
	}
	if success != nil && !*success {
		err := fmt.Errorf("checkin claim failed: %s", msg)
		log.Printf("traework checkin claim failed uid=%s err=%v", a.UID, err)
		return err
	}
	log.Printf("traework checkin claim response uid=%s code=%d msg=%s device=%s",
		a.UID, code, msg, shortDevice(a.DeviceID))
	return nil
}

// claimWithRotatedDevice 在原设备号被 9095 拒绝后，用**多个新生成的合法设备号**
// 依次重试 claim，直到额度真的增加为止。
//
// 为什么要轮换：实测 9095 是**设备维度**的拒绝，而额度发放是**账号维度**的。
// 原设备号被上游标记"今日已签"时，会让一个当天其实没领到额度的账号被拒；
// 换一个全新设备号即可正常入账（实测 4600 → 4700）。
//
// ⚠️ 为什么要**多次**重试（v0.6.9 修正）：
// 换号请求有一定概率返回 **9074「当前参与用户太多，请稍后再试」**。
// 该码字面即"暂时性限流"，实测**同一操作稍后重试就会成功**
// （2026-10-02 实测：程序第 1 次换号撞上 9074 后放弃，导致账号当天没签到；
//
//	同一天手工重试随机号，连续多次都返回 code=0 并成功 +100）。
//
// 早期版本把 9074 定性为"设备号未注册、重试无用"并只换一次号 —— 双重错误：
//
//	① 9074 是**瞬时限流**，不是设备号非法（重试即可通过）
//	② 只试一次，正好撞上 9074 就彻底放弃
//
// 现在：最多尝试 maxRotateAttempts 个不同设备号；一旦额度真的增加立刻返回。
//
// 返回 true 表示"换号后确实入账了"。
func (c *Client) claimWithRotatedDevice(a *auth.Auth) (bool, float64) {
	before, err := c.UserEntUsage(a)
	if err != nil {
		log.Printf("traework rotate-claim uid=%s 取前置额度失败 err=%v", a.UID, err)
		return false, 0
	}

	orig := a.DeviceID
	defer func() { a.DeviceID = orig }() // 不污染凭证：本次请求后立刻还原

	const maxRotateAttempts = 8
	var lastCode int
	var lastMsg string

	for i := 0; i < maxRotateAttempts; i++ {
		alt := randDeviceID()
		a.DeviceID = alt

		req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinClaim, bytes.NewReader([]byte(CheckinClaimBody)))
		if err != nil {
			return false, 0
		}
		UgHeaders(req, a)
		data, err := c.doJSON(req)
		if err != nil {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次请求失败 err=%v",
				a.UID, shortDevice(alt), i+1, err)
			continue
		}
		code, msg, success, perr := parseCheckinResponse(data)
		if perr != nil {
			continue
		}
		lastCode, lastMsg = code, msg

		// 9074 = 瞬时限流：换个号（或稍后）再试，**不是**失败终止条件。
		// 这是 v0.6.9 的关键修正：早期实现把它当"设备号未注册"直接放弃。
		if code == CheckinRateLimitCode {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=9074（瞬时限流，继续换号重试）msg=%s",
				a.UID, shortDevice(alt), i+1, msg)
			continue
		}
		if code == CheckinAlreadyClaimedCode {
			// 该号也被判"今日已签"，换个号再来。
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=9095（换号重试）",
				a.UID, shortDevice(alt), i+1)
			continue
		}
		if code != 0 || (success != nil && !*success) {
			log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次 code=%d msg=%s（未通过）",
				a.UID, shortDevice(alt), i+1, code, msg)
			continue
		}

		// code==0：仍需以**额度是否真的增加**为准，而不是以 code==0 为准。
		// 实测未领过的账号换号后会 +100；已领过的账号换号虽返 0 但额度不变。
		after, err := c.UserEntUsage(a)
		if err != nil {
			log.Printf("traework rotate-claim uid=%s 取后置额度失败（保守判为未入账）err=%v", a.UID, err)
			return false, 0
		}
		if after <= before {
			// 额度没变：说明该账号今天确实已经领过（换号返 0 但不入账）。
			// 这是"今日已签"的确定结论，不必再试。
			log.Printf("traework rotate-claim uid=%s alt=%s code=0 但额度未变（%d -> %d），判为今日已领",
				a.UID, shortDevice(alt), before, after)
			return false, 0
		}
		log.Printf("traework rotate-claim uid=%s alt=%s 第 %d 次入账成功：%d -> %d（+%d）",
			a.UID, shortDevice(alt), i+1, before, after, after-before)
		return true, float64(after - before)
	}

	log.Printf("traework rotate-claim uid=%s 尝试 %d 个设备号均未成功（最后 code=%d msg=%s）",
		a.UID, maxRotateAttempts, lastCode, lastMsg)
	return false, 0
}

// randDeviceID 生成一个格式合法的 16 位数字设备号。
//
// 实测该接口只校验"16 位数字"这一格式，不校验是否为本机注册号
// （随机号能让未领额度的账号成功入账）。用 crypto/rand 保证分布良好。
func randDeviceID() string {
	var b [16]byte
	const digits = "0123456789"
	out := make([]byte, 16)
	for i := 0; i < 16; i++ {
		if _, err := rand.Read(b[i : i+1]); err != nil {
			out[i] = digits[time.Now().UnixNano()%10]
			continue
		}
		out[i] = digits[int(b[i])%10]
	}
	return string(out)
}

// ErrCheckinAlreadyClaimed 该账号在当前设备上今日已经领过签到额度。
//
// 这是**幂等成功**（不是失败、也不是账号异常）：重复签到拿到它属正常。
// 去重键是「账号 + 设备」，所以同一设备号下的不同账号各自都能领一次。
type ErrCheckinAlreadyClaimed struct {
	Msg    string
	Device string
}

func (e *ErrCheckinAlreadyClaimed) Error() string {
	return fmt.Sprintf("checkin 9095 (already claimed today, device=%s): %s", e.Device, e.Msg)
}

// IsDeviceClaimed 返回 true，表示这是"今日已领"类的幂等结果。
//
// 命名保留 history：早期（v0.6.0）误以为它是设备级独占，故叫 DeviceClaimed。
// 现语义已厘清为「账号+设备」级，调度器据此按"今日已签"处理（OK=true）。
func (e *ErrCheckinAlreadyClaimed) IsDeviceClaimed() bool { return true }

// IsCheckinAlreadyClaimed 报告错误是否为「该账号今日已领」。
func IsCheckinAlreadyClaimed(err error) bool {
	if err == nil {
		return false
	}
	var t *ErrCheckinAlreadyClaimed
	return errors.As(err, &t)
}

// shortDevice 只显示设备号前 6 位，避免日志泄漏完整指纹。
func shortDevice(s string) string {
	if len(s) <= 6 {
		return s
	}
	return s[:6] + "…"
}

func (c *Client) DailyCheckin(a *auth.Auth) error {
	log.Printf("traework checkin start uid=%s device=%s", a.UID, shortDevice(a.DeviceID))

	// 前置查询：checked_in 或 did_checked_in 为真 → 本次无需再 claim。
	//
	// ⚠️ 但**不能据此直接判定"已签到"** —— 实测（2026-10-01）存在这种状态：
	//   did_checked_in=true 且 checked_in=false
	// 此时该账号今天**并没有**拿到签到额度（查不到今日签到包）。
	// 即"标记已签"与"额度已发"是两件事。
	//
	// 因此这里只做"是否还要发 claim"的判断；**真正是否签到成功，
	// 一律以 claim 的返回码 + 权益包对账为准**（见下方 claim 分支与对账）。
	// 早期版本在此直接 return「已签到」，会掩盖上述异常状态。
	checked, did, _, enable, err := c.CheckinStatusFull(a)
	if err != nil {
		return err
	}
	if !enable {
		err := fmt.Errorf("checkin disabled")
		log.Printf("traework checkin rejected uid=%s err=%v", a.UID, err)
		return err
	}
	if checked || did {
		log.Printf("traework checkin already-marked uid=%s checked_in=%t did_checked_in=%t（仍会尝试 claim 以确认）",
			a.UID, checked, did)
	}
	// 无论前置状态如何都发一次 claim：
	//   已领 -> 上游返 9095（幂等，无副作用）
	//   未领但被误标 -> 上游返 0 并真正入账（这正是修复点）
	if err := c.CheckinClaim(a); err != nil {
		return err
	}
	// 后置验证：claim 返回 0 也可能实际未入账，必须查 status 确认。
	if err := c.verifyCheckedIn(a); err != nil {
		return err
	}
	log.Printf("traework checkin verified uid=%s", a.UID)
	return nil
}

// verifyCheckedIn 轮询 status 直到 did_checked_in 为 true。
//
// ⚠️ 判定字段是 **did_checked_in**，不是 checked_in。
// 实测（2026-09-30）签到成功后响应为：
//
//	{"checked_in":false, "did_checked_in":true, "credits":100, ...}
//
// `checked_in` 表示"用户当前是否处于已签到会话"（网页端进页面时才置真），
// 对 API 调用方**恒为 false**。若用它做验证，每次签到都会误判为失败，
// 并在调度器里触发无意义的重试。这是 v0.5.5 之前未被发现的第二个 bug。
//
// ⚠️ did_checked_in 是**账号级**的（今天这个账号签过没有），
// 换设备号不改变它。因此它可用作"该账号今天是否已签"的前置判定。
//
// ⚠️ 但它**不能证明额度已到账**：上游存在"已标记签到但未发额度包"的
// 异常状态（实测账号1 全天 did_checked_in=true 却没有任何今日新建权益包）。
// 因此真正的对账手段是查权益包 start_time 是否落在今天，而不是看这个标志。
func (c *Client) verifyCheckedIn(a *auth.Auth) error {
	const maxTry = 3
	for attempt := 0; attempt < maxTry; attempt++ {
		checked, did, _, _, err := c.CheckinStatusFull(a)
		if err != nil {
			return fmt.Errorf("checkin verification: %w", err)
		}
		if checked || did {
			return nil
		}
		if attempt < maxTry-1 {
			time.Sleep(1200 * time.Millisecond) // 上游入账有轻微延迟
		}
	}
	err := fmt.Errorf("checkin verification failed: did_checked_in=false")
	log.Printf("traework checkin failed uid=%s err=%v", a.UID, err)
	return err
}

// CheckinStatus 查询签到状态（兼容旧签名）。
func (c *Client) CheckinStatus(a *auth.Auth) (checkedIn bool, credits int64, enable bool, err error) {
	checked, _, credits, enable, err := c.CheckinStatusFull(a)
	return checked, credits, enable, err
}

// CheckinStatusFull 查询签到状态，同时返回 did_checked_in。
//
// did_checked_in 才是"今天签到成功过"的可靠标志（见 verifyCheckedIn 说明）。
func (c *Client) CheckinStatusFull(a *auth.Auth) (checkedIn, didCheckedIn bool, credits int64, enable bool, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCheckinStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, false, 0, false, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, false, 0, false, err
	}
	var resp struct {
		CheckedIn    bool   `json:"checked_in"`
		DidCheckedIn bool   `json:"did_checked_in"`
		Credits      int64  `json:"credits"`
		Enable       bool   `json:"enable"`
		Code         int    `json:"code"`
		Message      string `json:"message"`
		Msg          string `json:"msg"`
		Success      *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		err = fmt.Errorf("checkin status parse: %w", err)
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, false, 0, false, err
	}
	if resp.Code != 0 {
		err := fmt.Errorf("checkin status code=%d msg=%s", resp.Code, checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, false, 0, false, err
	}
	if resp.Success != nil && !*resp.Success {
		err := fmt.Errorf("checkin status failed: %s", checkinResponseMessage(resp.Message, resp.Msg))
		log.Printf("traework checkin status failed uid=%s err=%v", a.UID, err)
		return false, false, 0, false, err
	}
	log.Printf("traework checkin status uid=%s checked_in=%t did_checked_in=%t credits=%d enable=%t",
		a.UID, resp.CheckedIn, resp.DidCheckedIn, resp.Credits, resp.Enable)
	return resp.CheckedIn, resp.DidCheckedIn, resp.Credits, resp.Enable, nil
}

func parseCheckinResponse(data []byte) (code int, msg string, success *bool, err error) {
	var resp struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Msg     string `json:"msg"`
		Success *bool  `json:"success"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, "", nil, fmt.Errorf("checkin response parse: %w", err)
	}
	return resp.Code, checkinResponseMessage(resp.Message, resp.Msg), resp.Success, nil
}

func checkinResponseMessage(message, msg string) string {
	if strings.TrimSpace(message) != "" {
		return strings.TrimSpace(message)
	}
	return strings.TrimSpace(msg)
}

func (c *Client) UserResource(a *auth.Auth) (remain int64, err error) { return c.UserEntUsage(a) }

// UserEntUsage 查询 TraeWork 账号的**剩余**可用积分。
//
// 修订记录（真值口径经抓包确认）：
//
//	v0.5.1 修复「显示 4050 但实际只有 310」
//	  原实现把所有权益包的 `quota.credits_limit` **累加**后当剩余积分返回。
//	  `credits_limit` 是**额度上限**（发放总量），不是剩余量。
//
//	v0.5.2 修复「显示 150 也不对」（v0.5.1 引入的回退缺陷）
//	  曾回退读取 checkin/status 的 `credits`，并误以为那是余额。
//	  实测该字段恒为 150（签到奖励固定值），已彻底移除回退。
//
//	v0.5.3 改用**正确的接口与口径**（2026-09-29 抓包确认）
//	  正确接口是 `user_current_entitlement_list`，不是 `ide_user_ent_usage`
//	  （后者是 IDE 客户端专用，网页端不调用，字段口径也不同）。
//
//	  响应里的 `usage_summary` 给出精确口径：
//	    total_amount    = 4050   累计发放
//	    consumed_amount = 3751.3 累计消耗
//	    剩余 = 4050 - 3751.3 = 298.7  ← 与官网个人中心显示一致
//
//	  同一响应里逐包 (credits_limit - usage.credits_amount) 求和也得 298.7，
//	  两者互为佐证。但**优先用 usage_summary**：它是上游算好的权威值，
//	  不依赖各包字段是否齐全。
//
//	  注意 `usage` 为 `{}` 表示该包**未使用**（而非无法判断）——
//	  这在 v0.5.1/v0.5.2 里被当成"解析失败"，是 150/不可用 问题的另一处根源。
//
// 返回值为整数（官网也是整数展示）；小数部分四舍五入。
// CurrentDayGrant 报告该账号**今天**是否真的收到了新的**签到**额度包。
//
// 为什么需要它：`did_checked_in` 只是"签到标记"，并**不保证额度到账**。
// 实测（2026-10-01）账号2 的 did_checked_in 在签到前就是 true，且当天
// 查不到任何签到包、积分也一分没涨 —— 只看状态标志必然误报"签到成功"。
//
// ★ 识别方式（v0.6.7 起改用服务端权威标识）：
// 上游给每个权益包都带了**自解释的唯一 ID**，直接读它即可精确判定：
//
//	entitlement_id = "checkin_20261001_<uid>"        ← 某日签到奖励
//	entitlement_id = "monthly_bonus_202610_<uid>"    ← 某月月初奖励
//	product_extra.package_extra.package_name = "签到奖励"
//	product_extra.package_extra.package_source_type = 9  ← 签到
//
// 因此判据：**entitlement_id 以 "checkin_" 开头 且日期段为今天**。
//
// ⚠️ 历史踩坑（务必保留这段说明）：
//
//	v0.6.3 用"今天有任意新包"判定 → 把月初包误算成签到到账
//	v0.6.5 改用「时间窗(hour≥1) + 金额(≤300)」启发式 → 能工作但很脆弱：
//	       若签到发生在凌晨、或签到额度调整 >300，就会再次误判。
//	v0.6.7 换成读 entitlement_id —— **不再依赖任何猜测**。
//
// 返回 (是否有签到到账, 到账额度, error)。
func (c *Client) CurrentDayGrant(a *auth.Auth) (granted bool, amount float64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCurrentEntList, bytes.NewReader([]byte("{}")))
	if err != nil {
		return false, 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return false, 0, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, 0, fmt.Errorf("entitlement list parse: %w", err)
	}
	packs := findPackList(raw)
	todayKey := time.Now().Format("20060102") // 与 entitlement_id 里的日期段一致

	var others []string
	for _, p := range packs {
		flat := map[string]any{}
		flattenInto(p, flat, 0)

		id, _ := flat["entitlement_id"].(string)
		if id == "" {
			continue
		}

		// ① 权威判据：entitlement_id 形如 checkin_<YYYYMMDD>_<uid>
		if !strings.HasPrefix(id, checkinIDPrefix) {
			// 记录非签到包，便于排查（如 monthly_bonus_202610_... / 纯数字ID）
			if n, ok := toFloat64(flat["credits_limit"]); ok && n > 0 {
				others = append(others, fmt.Sprintf("%s(%s)", id, formatCredits(n)))
			}
			continue
		}
		// 拆出日期段：checkin_20261001_xxx
		rest := strings.TrimPrefix(id, checkinIDPrefix)
		if i := strings.IndexByte(rest, '_'); i > 0 {
			rest = rest[:i]
		}
		if rest != todayKey {
			continue // 往日的签到包，不计入今天
		}
		if n, ok := toFloat64(flat["credits_limit"]); ok && n > 0 {
			granted = true
			amount += n
		}
	}

	if granted {
		log.Printf("traework grant uid=%s 今日签到到账=%s（据 entitlement_id）", a.UID, formatCredits(amount))
	} else {
		log.Printf("traework grant uid=%s 今日无签到包（非签到包: %v）", a.UID, others)
	}
	return granted, amount, nil
}

// formatCredits 把额度格式化成便于阅读的整数串（100 而非 100.000000）。
func formatCredits(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%g", v)
}

func (c *Client) UserEntUsage(a *auth.Auth) (remain int64, err error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpCurrentEntList, bytes.NewReader([]byte("{}")))
	if err != nil {
		return 0, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return 0, err
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, fmt.Errorf("entitlement list parse: %w", err)
	}

	// ① 首选：usage_summary.total_amount - consumed_amount（上游权威口径）
	if total, used, ok := parseUsageSummary(raw); ok {
		r := total - used
		if r < 0 {
			r = 0
		}
		log.Printf("traework credits uid=%s source=usage_summary total=%v consumed=%v remain=%v",
			a.UID, total, used, r)
		return int64(math.Round(r)), nil
	}

	// ② 兜底：逐包 (credits_limit - usage.credits_amount) 求和
	if v, ok := sumRemainFromEntitlements(raw); ok {
		log.Printf("traework credits uid=%s source=entitlement_packs remain=%d", a.UID, v)
		return v, nil
	}

	// 解析失败：打印结构，便于上游字段变更时定位
	logResourceShape(a.UID, raw)
	logBalanceCandidates(a.UID, raw)
	log.Printf("traework credits uid=%s no usable balance in response", a.UID)
	return 0, fmt.Errorf("entitlement list: no usable remaining-credit field in response")
}

// parseUsageSummary 读取 usage_summary{total_amount, consumed_amount}。
//
// 这是**权威口径**：上游已经算好的"发放总量"与"累计消耗"，剩余即二者之差。
// 实测 `total_amount=4050, consumed_amount=3751.3` → 298.7，与官网一致。
func parseUsageSummary(raw map[string]any) (total, consumed float64, ok bool) {
	// 容错：summary 可能被包在 data/result 里
	candidates := []map[string]any{raw}
	for _, w := range []string{"data", "result", "Data", "Result"} {
		if sub, isMap := raw[w].(map[string]any); isMap {
			candidates = append(candidates, sub)
		}
	}
	for _, c := range candidates {
		sm, isMap := c["usage_summary"].(map[string]any)
		if !isMap {
			continue
		}
		t, tok := toFloat64(sm["total_amount"])
		u, uok := toFloat64(sm["consumed_amount"])
		if tok && uok {
			return t, u, true
		}
	}
	return 0, 0, false
}

// logBalanceCandidates 在解析失败时，打印权益包里全部数值型字段的路径与值。
//
// 目的：让用户刷新一次就能把真实字段名反馈回来。只打数值字段，
// 不含 token/uid 等敏感信息，也不会因为响应字段多而被截断。
func logBalanceCandidates(uid string, raw map[string]any) {
	packs := findPackList(raw)
	if len(packs) == 0 {
		log.Printf("traework credits uid=%s balance-candidates: <未找到权益包数组>", uid)
		return
	}
	seen := map[string]bool{}
	var out []string
	for i, pack := range packs {
		flat := map[string]any{}
		flattenInto(pack, flat, 0)
		keys := make([]string, 0, len(flat))
		for k := range flat {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			n, ok := toInt64(flat[k])
			if !ok {
				continue
			}
			entry := fmt.Sprintf("pack%d.%s=%d", i, k, n)
			if !seen[entry] {
				seen[entry] = true
				out = append(out, entry)
			}
		}
	}
	if len(out) == 0 {
		log.Printf("traework credits uid=%s balance-candidates: <权益包内无数值字段>", uid)
		return
	}
	sort.Strings(out)
	log.Printf("traework credits uid=%s balance-candidates: %s", uid, strings.Join(out, " "))
}

// 字段名清单按**特异性从高到低**排列，先命中的优先。
//
// 采用扁平 key 精确匹配（flattenInto 已统一转小写），所以不用考虑大小写，
// 但要警惕**过于宽泛**的字段名——它们会误匹配到无关的同名字段：
//   - "credits" 单独出现时，既可能是"剩余"也可能是"发放总量"，语义不定，故
//     只在最末位候补，且必须与 limit/used 同现时才参与计算
//   - "available" / "balance" 在部分上游里表示"可提现余额"而非"积分余额"，
//     排在具体名称之后
var remainFieldNames = []string{
	// 最明确：带 credits/credit 前缀且含 remain
	"credits_remain", "credit_remain", "remain_credits",
	// 较明确
	"credits_available", "available_credits", "credits_balance", "credits_surplus",
	// 宽泛（可能与其它业务字段重名），放最后
	"remain", "available", "balance", "surplus",
}

// usedFieldNames 可能表示"已用积分"的字段名。
//
// `credits_amount` 是**实测确认**的字段名：它出现在权益包的
// `usage.credits_amount`，表示该包已消耗的积分。
// 重要语义：`usage` 为 `{}`（空对象）表示**未使用**，即已用 = 0；
// 若把它当成"无法判断"而跳过该包，会漏算该包的剩余额度。
var usedFieldNames = []string{
	"credits_amount", "credits_used", "credit_used", "used_credits",
	"credits_consume", "credit_consume", "credits_cost",
	"used", "consume", "cost",
}

// limitFieldNames 可能表示"额度上限"的字段名（**不可**直接当剩余量）。
//
// 刻意**不收录**裸 "credits" / "total" / "quota" 这类宽泛名：
//   - "credits" 单独出现时语义不定（可能是签到奖励值，实测恒为 150）
//   - "quota" 通常是**容器对象**而非标量，收进来只会在 toInt64 时失败
//   - "total" 太泛，可能命中"总记录数"之类的分页字段
//
// 宁可少认字段、把包判为"解析失败"，也不要认错字段得出一个假余额。
var limitFieldNames = []string{
	"credits_limit", "credit_limit", "limit_credits",
	"credits_total", "credit_total", "total_credits",
}

// sumRemainFromEntitlements 从权益包里求"剩余积分"之和。
//
// 对每个权益包：
//   - 优先取明确的"剩余"字段
//   - 否则用 上限 - 已用 计算
//   - 两者都没有则该包 **解析失败**
//
// 返回 (总值, 是否至少命中一个包)。
//
// 注意第二个返回值的语义：它是"是否至少有一个包提供了可信的剩余量"，
// 而不是"是否找到了权益包"。若全部包都解析不出，返回 false 让调用方失败，
// 而不是返回一个 0 或残缺的和——残缺的和会让用户以为余额变少了。
func sumRemainFromEntitlements(raw map[string]any) (int64, bool) {
	packs := findPackList(raw)
	if len(packs) == 0 {
		return 0, false
	}
	var total int64
	hit := false
	for i, pack := range packs {
		bal := extractBalance(pack)
		if bal == nil {
			// 记下是哪个包解析失败，方便对照 shape 日志定位字段名
			if v, ok := pack["entitlement_base_info"]; ok {
				_ = v
			}
			log.Printf("traework ent pack[%d] skipped: no remain/limit-used fields", i)
			continue
		}
		total += *bal
		hit = true
	}
	return total, hit
}

// findPackList 在响应里定位权益包数组（字段名容错：多层级候选）。
func findPackList(raw map[string]any) []map[string]any {
	candidates := []string{
		"user_entitlement_pack_list", "entitlement_pack_list",
		"user_entitlement_packs", "pack_list", "packs",
	}
	// 先看顶层
	for _, key := range candidates {
		if v, ok := raw[key]; ok {
			if list := asMapList(v); len(list) > 0 {
				return list
			}
		}
	}
	// 再递归一层（常见包装：data / result / {"data":{"..."}}）
	for _, wrapper := range []string{"data", "result", "Result", "Data", "response", "Response"} {
		if sub, ok := raw[wrapper].(map[string]any); ok {
			if list := findPackList(sub); len(list) > 0 {
				return list
			}
		}
	}
	return nil
}

func asMapList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, it := range arr {
		if m, ok := it.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// extractBalance 从一个权益包里提取"剩余积分"。
//
// 真实结构（2026-09-29 抓包确认）：
//
//	{
//	  "usage": {"credits_amount": 1.304},   ← 已用；usage 为 {} 表示未使用
//	  "entitlement_base_info": {"quota": {"credits_limit": 150}}  ← 额度上限
//	}
//
// 计算：剩余 = credits_limit - usage.credits_amount（缺省按 0 计）。
//
// 返回 nil 仅当**没有任何上限信息**（该包无法参与计算）。
// 注意：有上限但无 usage 时**不是** nil，而是 剩余 = 上限（该包未被使用）。
func extractBalance(pack map[string]any) *int64 {
	// 递归收集该包里所有 key→值（含嵌套 quota / entitlement_base_info / usage）
	flat := map[string]any{}
	flattenInto(pack, flat, 0)

	// 1) 明确的"剩余"字段（若上游以后补充了该字段，优先采用）
	for _, k := range remainFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				return &n
			}
		}
	}

	// 2) 上限 - 已用。没有上限则无法计算
	var limit *int64
	for _, k := range limitFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				limit = &n
				break
			}
		}
	}
	if limit == nil {
		return nil // 该包没有额度上限（如"免费"包），不参与积分计算
	}

	// 已用量：缺省为 0（usage 为 {} 或字段缺失都表示未使用）
	var used int64
	for _, k := range usedFieldNames {
		if v, ok := flat[k]; ok {
			if n, ok := toInt64(v); ok {
				used = n
				break
			}
		}
	}

	remain := *limit - used
	if remain < 0 {
		remain = 0
	}
	return &remain
}

// flattenInto 递归展开嵌套 map（深度上限 4，避免异常结构导致栈问题）。
func flattenInto(m map[string]any, out map[string]any, depth int) {
	if depth > 4 {
		return
	}
	for k, v := range m {
		lk := strings.ToLower(k)
		if _, exists := out[lk]; !exists {
			out[lk] = v
		}
		if sub, ok := v.(map[string]any); ok {
			flattenInto(sub, out, depth+1)
		}
	}
}

func toInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

// toFloat64 提取浮点值。
// 积分口径里 total_amount / consumed_amount 都是小数（如 4050 与 3751.3），
// 用 int64 会截断小数部分，导致剩余量偏差，因此单独提供浮点版本。
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int64:
		return float64(n), true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// logResourceShape 打印 ent_usage 响应的字段结构（只打 key 与数值，不含 token）。
//
// 用于确认上游真实字段名：积分字段名一旦变化/新增，看这行日志即可定位。
// 采用**扁平路径**输出（如 entitlement_base_info.quota.credits_remain=310），
// 这样嵌套在哪一层、字段叫什么都能直接看出来，不受响应包装层数影响。
func logResourceShape(uid string, raw map[string]any) {
	pairs := make([]string, 0, 16)
	collectShape(raw, "", &pairs, 0)
	sort.Strings(pairs)
	// 控制长度，避免超长响应刷爆日志
	const maxPairs = 40
	if len(pairs) > maxPairs {
		pairs = append(pairs[:maxPairs], fmt.Sprintf("...(+%d)", len(pairs)-maxPairs))
	}
	log.Printf("traework ent_usage shape uid=%s %s", uid, strings.Join(pairs, " "))
}

// collectShape 递归收集 "路径=值" 对；只输出标量与非空容器摘要。
// 深度上限放宽到 8：上游响应常有 data/result 多层包装，过浅会看不到 quota 层。
func collectShape(v any, prefix string, out *[]string, depth int) {
	const maxDepth = 8
	if depth > maxDepth {
		*out = append(*out, prefix+"=<max-depth>")
		return
	}
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			collectShape(t[k], p, out, depth+1)
		}
	case []any:
		if len(t) == 0 {
			*out = append(*out, prefix+"=[]")
			return
		}
		*out = append(*out, fmt.Sprintf("%s.len=%d", prefix, len(t)))
		// 展开前几个元素：单个包结构相同，但不同包的字段名可能不同
		// （例如有的包给 credits_remain、有的只给 limit+used），
		// 因此至少展开 3 个，确保能看出各包字段差异。
		limit := len(t)
		if limit > 3 {
			limit = 3
		}
		for i := 0; i < limit; i++ {
			collectShape(t[i], fmt.Sprintf("%s[%d]", prefix, i), out, depth+1)
		}
		if len(t) > limit {
			*out = append(*out, fmt.Sprintf("%s[%d..]=<省略 %d 项>", prefix, limit, len(t)-limit))
		}
	default:
		// 标量：数值 / 字符串 / 布尔 / null
		*out = append(*out, fmt.Sprintf("%s=%v", prefix, t))
	}
}

func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	host := a.ApiHost
	if host == "" {
		host = c.oauthBase()
	}
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(raw))
	if err != nil {
		return "", "", "", err
	}
	OAuthHeaders(req)
	req.Header.Set("X-Cloudide-Token", a.JWT())
	data, err := c.doJSON(req)
	if err != nil {
		return "", "", "", err
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, nil
}

func (c *Client) Classify(status int, body string) provider.ErrKind { return Classify(status, body) }
func (c *Client) Stream(w http.ResponseWriter, r io.Reader) error   { return Stream(w, r) }
func (c *Client) Aggregate(r io.Reader) (map[string]any, error)     { return Aggregate(r) }

// StreamWithError 实现 provider.StreamErrorReporter：
// 流式转换途中遇到上游错误时回调 (分类, 描述)，供上层冷却账号。
// 错误本身仍会以下发的 SSE `event: error` 事件交给客户端。
func (c *Client) StreamWithError(w http.ResponseWriter, r io.Reader, onErr func(provider.ErrKind, string)) error {
	return StreamWithError(w, r, func(se *SOLOStreamError) {
		if onErr != nil {
			onErr(se.Kind(), se.Error())
		}
	})
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}
