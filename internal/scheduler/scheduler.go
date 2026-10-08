// Package scheduler 定时任务：每日签到 + token keepalive。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
// 签到时间支持分钟精度运行中更新（SetCheckinMinutes），由 GUI 面板写入 config.json 后调用。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
)

// Config 调度器依赖。
type Config struct {
	Pool           *pool.Pool
	Upstream       provider.Upstream
	Name           string // 日志中的平台名
	CheckinHours   []int  // 旧配置兼容，整点小时
	CheckinMinutes []int  // 当天分钟数，优先于 CheckinHours
	KeepaliveHours []int  // 默认 [22]
}

// 限流重试参数。
//
// 背景：TraeWork 的签到 claim 接口在高峰会返回 9074（"当前参与用户太多"）。
// 单次调用内虽然已有指数退避重试（CheckinClaim 内 4 次 / 约 58 秒），
// 但高峰可能持续更久。原实现在耗尽后**只是标记 Retryable 而无人消费**，
// 用户看到的"稍后自动重试"实际要等到下一个定时点（可能间隔数小时）。
//
// 现在：耗尽后由调度器安排**独立的延迟重试**，不依赖下一次定时签到。
const (
	// rateLimitRetryDelay 限流耗尽后的首次延迟重试间隔。
	// 取 10 分钟：足够跨过一波瞬时高峰，又不会让用户等太久。
	rateLimitRetryDelay = 10 * time.Minute

	// rateLimitRetryMax 单个账号每天最多安排多少次限流延迟重试。
	// 防止上游长时间异常时无限重试刷屏。
	rateLimitRetryMax = 6
)

// retryState 单个账号的限流重试状态。
type retryState struct {
	count    int       // 已安排的延迟重试次数（当天）
	lastDay  int       // 归属日期（一年中的第几天），跨天重置
	nextAt   time.Time // 下次重试时刻
	pending  bool      // 是否有待执行的重试
}

// Scheduler 调度器。
type Scheduler struct {
	mu        sync.Mutex                 // 保护 cfg 中的小时配置
	cfg       Config
	wake      chan struct{}              // 配置变更唤醒 Run 循环重算下次触发
	onCheckin func(CheckinResult)        // 结果观察器，供 GUI 接收自动签到结果
	onRefresh func(string, bool, string) // token 刷新结果观察器

	retryMu sync.Mutex            // 保护 retries
	retries map[string]*retryState // uid → 限流重试状态
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinMinutes) == 0 {
		if len(cfg.CheckinHours) > 0 {
			cfg.CheckinMinutes = make([]int, 0, len(cfg.CheckinHours))
			for _, h := range cfg.CheckinHours {
				if h >= 0 && h <= 23 {
					cfg.CheckinMinutes = append(cfg.CheckinMinutes, h*60)
				}
			}
		} else {
			cfg.CheckinMinutes = []int{9 * 60, 21 * 60}
		}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	return &Scheduler{cfg: cfg, wake: make(chan struct{}, 1), retries: map[string]*retryState{}}
}

// markRetryable 记录某账号需要延迟重试，并返回安排的时刻。
//
// 仅在限流耗尽时调用。返回 (下次重试时刻, 是否还能重试)：
//   - 跨天自动重置计数
//   - 超过 rateLimitRetryMax 后不再安排（返回 ok=false）
func (s *Scheduler) markRetryable(uid string, now time.Time) (time.Time, bool) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	st := s.retries[uid]
	day := now.YearDay()
	if st == nil || st.lastDay != day {
		st = &retryState{lastDay: day}
		s.retries[uid] = st
	}
	if st.count >= rateLimitRetryMax {
		return time.Time{}, false
	}
	st.count++
	// 递增退避：10min, 20min, 30min, ...（线性增长，避免像指数那样很快跨过小时级）
	st.nextAt = now.Add(rateLimitRetryDelay * time.Duration(st.count))
	st.pending = true
	return st.nextAt, true
}

// dueRetries 取出所有已到期的重试账号，并清除其 pending 标记。
func (s *Scheduler) dueRetries(now time.Time) []string {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	var out []string
	for uid, st := range s.retries {
		if st.pending && !now.Before(st.nextAt) {
			st.pending = false
			out = append(out, uid)
		}
	}
	return out
}

// nextRetryAt 返回最近一次待执行重试的时刻（无则返回零值）。
func (s *Scheduler) nextRetryAt() time.Time {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	var best time.Time
	for _, st := range s.retries {
		if !st.pending {
			continue
		}
		if best.IsZero() || st.nextAt.Before(best) {
			best = st.nextAt
		}
	}
	return best
}

// clearRetry 账号签到成功后清除其重试状态。
func (s *Scheduler) clearRetry(uid string) {
	s.retryMu.Lock()
	defer s.retryMu.Unlock()
	delete(s.retries, uid)
}

// schedule 返回当前签到分钟/保活小时配置的副本。
func (s *Scheduler) schedule() (checkinMinutes, keepaliveHours []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int{}, s.cfg.CheckinMinutes...), append([]int{}, s.cfg.KeepaliveHours...)
}

// CheckinHours 保留旧 API，返回整点签到小时。
func (s *Scheduler) CheckinHours() []int {
	minutes, _ := s.schedule()
	out := make([]int, 0, len(minutes))
	for _, m := range minutes {
		if m%60 == 0 {
			out = append(out, m/60)
		}
	}
	return out
}

// CheckinMinutes 当前签到时间，单位为当天分钟数（0..1439）。
func (s *Scheduler) CheckinMinutes() []int {
	minutes, _ := s.schedule()
	return minutes
}

// CheckinTimes 当前签到时间，格式为 HH:MM。
func (s *Scheduler) CheckinTimes() []string {
	minutes := s.CheckinMinutes()
	out := make([]string, 0, len(minutes))
	for _, m := range minutes {
		out = append(out, fmt.Sprintf("%02d:%02d", m/60, m%60))
	}
	return out
}

// KeepaliveHours 当前保活时间（副本）。
func (s *Scheduler) KeepaliveHours() []int {
	_, kh := s.schedule()
	return kh
}

// SetCheckinMinutes 运行中更新签到分钟并唤醒调度循环。
func (s *Scheduler) SetCheckinMinutes(minutes []int) {
	clean := make([]int, 0, len(minutes))
	seen := map[int]bool{}
	for _, m := range minutes {
		if m >= 0 && m < 24*60 && !seen[m] {
			seen[m] = true
			clean = append(clean, m)
		}
	}
	sort.Ints(clean)
	s.mu.Lock()
	s.cfg.CheckinMinutes = clean
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// SetCheckinHours 保留旧 API，将整点小时转换为当天分钟数。
func (s *Scheduler) SetCheckinHours(hours []int) {
	minutes := make([]int, 0, len(hours))
	for _, h := range hours {
		minutes = append(minutes, h*60)
	}
	s.SetCheckinMinutes(minutes)
}

// SetCheckinObserver 设置签到结果观察器；用于把定时任务结果推送到 GUI。
func (s *Scheduler) SetCheckinObserver(fn func(CheckinResult)) {
	s.mu.Lock()
	s.onCheckin = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyCheckin(r CheckinResult) {
	s.mu.Lock()
	fn := s.onCheckin
	s.mu.Unlock()
	if fn != nil {
		fn(r)
	}
}

// SetRefreshObserver 设置 token 刷新结果观察器。
func (s *Scheduler) SetRefreshObserver(fn func(uid string, ok bool, msg string)) {
	s.mu.Lock()
	s.onRefresh = fn
	s.mu.Unlock()
}

func (s *Scheduler) notifyRefresh(uid string, ok bool, msg string) {
	s.mu.Lock()
	fn := s.onRefresh
	s.mu.Unlock()
	if fn != nil {
		fn(uid, ok, msg)
	}
}

// NextFire 返回最近的一次触发时间（签到 + 保活合并）。
func (s *Scheduler) NextFire() time.Time {
	ch, kh := s.schedule()
	all := append(append([]int{}, ch...), hoursToMinutes(kh)...)
	return nextFireMinutes(time.Now(), all)
}

// nextFire 保留旧测试/API语义：hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	return nextFireMinutes(now, hoursToMinutes(hours))
}

func hoursToMinutes(hours []int) []int {
	out := make([]int, 0, len(hours))
	for _, h := range hours {
		out = append(out, h*60)
	}
	return out
}

// nextFireMinutes 返回 now 之后最近的触发时间；输入为当天分钟数。
func nextFireMinutes(now time.Time, minutes []int) time.Time {
	var earliest time.Time
	for _, m := range minutes {
		if m < 0 || m >= 24*60 {
			continue
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), m/60, m%60, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		ch, kh := s.schedule()
		all := append(append([]int{}, ch...), hoursToMinutes(kh)...)
		next := nextFireMinutes(time.Now(), all)
		// 若有待执行的限流重试且早于下次定时点，则以重试时刻为准唤醒。
		if ra := s.nextRetryAt(); !ra.IsZero() && ra.Before(next) {
			next = ra
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop() // 配置变更，重算
		case <-timer.C:
			now := time.Now()
			minute := now.Hour()*60 + now.Minute()
			if containsMinute(ch, minute) {
				s.RunCheckinNow()
			}
			if contains(kh, now.Hour()) {
				s.RunKeepaliveNow()
			}
			// 到期重试：只签这些账号，不影响其它账号
			s.runDueRetries(now)
		}
	}
}

// runDueRetries 对已到期的限流账号单独重试签到。
func (s *Scheduler) runDueRetries(now time.Time) {
	uids := s.dueRetries(now)
	if len(uids) == 0 {
		return
	}
	name := s.name()
	log.Printf("checkin retry start platform=%s accounts=%d uids=%v", name, len(uids), uids)
	for _, uid := range uids {
		if s.cfg.Pool.AuthByUID(uid) == nil {
			s.clearRetry(uid) // 账号已被删除
			continue
		}
		r := s.checkinOne(uid)
		if r.OK {
			s.clearRetry(uid) // 成功，不再重试
		} else if r.Retryable {
			if at, ok := s.markRetryable(uid, now); ok {
				log.Printf("checkin retry scheduled platform=%s uid=%s at=%s",
					name, uid, at.Format("15:04:05"))
			} else {
				log.Printf("checkin retry exhausted-for-today platform=%s uid=%s",
					name, uid)
			}
		}
	}
}

func containsMinute(minutes []int, minute int) bool {
	for _, v := range minutes {
		if v == minute {
			return true
		}
	}
	return false
}

func contains(hours []int, h int) bool {
	for _, v := range hours {
		if v == h {
			return true
		}
	}
	return false
}

// CheckinResult 单账号签到结果（GUI 面板展示）。
type CheckinResult struct {
	UID string `json:"uid"`
	OK  bool   `json:"ok"`
	Msg string `json:"msg"`
	// Retryable 表示失败原因是上游瞬时状态，不是账号本身的问题。
	Retryable bool `json:"retryable,omitempty"`
	// AlreadyChecked 表示**今天已经签过了**，本次没有新增积分。
	//
	// 这是"幂等的成功"，不是"本次签到成功"。前端必须据此区分文案：
	// 若显示成绿色的"签到成功"，用户会期待积分上涨，发现没涨就会以为程序坏了。
	AlreadyChecked bool `json:"already_checked,omitempty"`
	// GrantMissing 表示"签到标记已完成，但今日没有查到新增额度包"。
	//
	// 这是上游的一种异常状态：账号被标记为已签，却没真正发放积分
	// （实测 2026-09-30 见过）。此时**不能**告诉用户"积分已到账"。
	GrantMissing bool  `json:"grant_missing,omitempty"`
	Remain       int64 `json:"remain"`
	HasRemain    bool  `json:"has_remain"`
}

// currentDayGrant 由上游客户端实现的"今日是否真的到账了新额度包"探测。
// 用接口探测而非直接 import 具体平台包，避免 scheduler 与各上游耦合。
type currentDayGrant interface {
	CurrentDayGrant(*auth.Auth) (bool, float64, error)
}

// currentDayGrant 查询该账号今天有没有新建权益包（真正到账的凭证）。
// 上游不支持时返回 (false, 0, nil)，调用方应忽略该信息。
func (s *Scheduler) currentDayGrant(a *auth.Auth) (bool, float64, error) {
	g, ok := s.cfg.Upstream.(currentDayGrant)
	if !ok {
		return false, 0, nil
	}
	return g.CurrentDayGrant(a)
}

// RunCheckinNow 立即对所有账号执行签到 + 余额刷新 + 解冻。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
func (s *Scheduler) RunCheckinNow() {
	name := s.name()
	log.Printf("checkin batch start platform=%s accounts=%d", name, len(s.cfg.Pool.List()))
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("checkin skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		r := s.checkinOne(st.UID)
		log.Printf("checkin result platform=%s uid=%s ok=%t msg=%s remain=%d has_remain=%t", name, st.UID, r.OK, r.Msg, r.Remain, r.HasRemain)
	}
	log.Printf("checkin batch done platform=%s", name)
}

// CheckinAccount 单个账号立即签到（禁用跳过），返回该账号结果。
func (s *Scheduler) CheckinAccount(uid string) (CheckinResult, error) {
	st, ok := s.cfg.Pool.Status(uid)
	if !ok {
		return CheckinResult{}, fmt.Errorf("unknown account %s", uid)
	}
	if st.Disabled {
		return CheckinResult{}, fmt.Errorf("account %s disabled", uid)
	}
	return s.checkinOne(uid), nil
}

// checkinOne 单账号签到 + 余额刷新 + 解冻 + 记录签到状态。
func (s *Scheduler) checkinOne(uid string) CheckinResult {
	name := s.name()
	log.Printf("checkin start platform=%s uid=%s", name, uid)
	a := s.cfg.Pool.AuthByUID(uid)
	if a == nil || a.RefreshToken == "" {
		r := CheckinResult{UID: uid, Msg: "no refresh token"}
		s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
		s.notifyCheckin(r)
		return r
	}
	// 签到前保证 access token 有效；否则仅依赖晚间 keepalive 时，早上的签到可能拿过期 token。
	if a.NeedsRefresh(2 * time.Hour) {
		if err := s.refreshForCheckin(a, uid); err != nil {
			r := CheckinResult{UID: uid, Msg: "refresh: " + shortErr(err)}
			s.cfg.Pool.RecordCheckin(uid, false, r.Msg)
			s.notifyCheckin(r)
			return r
		}
	}
	r := CheckinResult{UID: uid}
	checkinErr := s.cfg.Upstream.DailyCheckin(a)
	// status 接口本身就是令牌有效性验证；若返回 session dead，刷新一次后重试整套签到。
	if checkinErr != nil && isSessionDead(checkinErr) {
		log.Printf("checkin token invalid platform=%s uid=%s, refreshing and retrying", name, uid)
		if err := s.refreshForCheckin(a, uid); err != nil {
			checkinErr = fmt.Errorf("token invalid; refresh failed: %w", err)
		} else {
			checkinErr = s.cfg.Upstream.DailyCheckin(a)
		}
	}
	if checkinErr != nil {
		log.Printf("checkin failed platform=%s uid=%s err=%v", name, uid, checkinErr)
		r.Msg = shortErr(checkinErr)
		// 判定顺序：先认"今日已领"类幂等结果，再看真正的失败。
		//
		// 9095（ErrCheckinAlreadyClaimed）与"已签到"文本都表示**该账号今天已经领过**，
		// 属幂等成功（OK=true），不是失败。
		//
		// ⚠️ 去重粒度是**账号级**（实测矩阵：同一账号换任何设备号结果都一样，
		// 不同账号各自独立）。所以同一设备号下多账号**本来就能各签一次**，
		// 不需要给每个账号单独配设备号（v0.6.0 误判为设备级、v0.6.1 误判为
		// 账号+设备级，v0.6.3 最终确认为账号级）。
		if isDeviceClaimed(checkinErr) || isAlready(checkinErr) {
			// 上游认为该账号今天已签（9095）。但"已签标记"**不等于**额度到账，
			// 下面用权益包对账，据实给出提示。
			//
			// 实测（2026-10-01）存在这种状态：did_checked_in=true 且当天
			// 查不到签到包、积分也一分没涨。此时若说"积分已到账"就是谎报。
			r.OK = true
			r.AlreadyChecked = true
			r.Msg = "今日已签到（本次无新增积分）"

			g, amt, gerr := s.currentDayGrant(a)
			switch {
			case gerr != nil:
				// 对账失败（网络/字段变更）：给中性文案，不谎报也不断言失败
				r.Msg = "今日已签到（未能确认到账情况）"
				log.Printf("checkin grant-check-failed platform=%s uid=%s err=%v", name, uid, gerr)
			case g:
				r.Msg = fmt.Sprintf("今日已签到（今日已到账 %.0f 积分）", amt)
				log.Printf("checkin grant-ok platform=%s uid=%s 今日签到到账=%v", name, uid, amt)
			default:
				// 标记已签，但今天确实没查到签到包 -> 如实告知未到账
				r.GrantMissing = true
				r.Msg = "今日已签到，但未查到新增积分（今日额度未增加）"
				log.Printf("checkin grant-missing platform=%s uid=%s（标记已签但今日签到包不存在）", name, uid)
			}
		} else if isCheckinThrottled(checkinErr) {
			// 上游 9074：**瞬时限流**（v0.6.9 依据实测定性；v0.5.7 曾误判为
			// "设备未注册、重试无用"，该结论已作废）。
			//
			// 请求内已完成同步换号重试（claimWithRotatedDevice，最多 8 次），
			// 仍失败才走到这里 —— 说明这一波高峰还没过去。
			//
			// 因此：交给调度器的**跨分钟延迟重试队列**（runDueRetries），
			// 而不是让用户干等到下一个定时点。
			//
			// ⚠️ 这里必须用 isCheckinThrottled 而非 isRateLimited：
			// *ErrCheckinRateLimited 的 IsRateLimited() 恒返回 false
			//（语义是"不交给 isRateLimited 那套判定"），所以旧代码
			// `else if isRateLimited(...)` 是**永不成立的死分支**，
			// 9074 会一直掉到下面的兜底文案，用户看到的是英文原文
			// `checkin 9074 (transient throttle): ...`。
			r.Retryable = true
			r.Msg = "签到被拒：当前签到人数过多，将在稍后自动重试"
			log.Printf("checkin throttled platform=%s uid=%s attempts=%d（已安排延迟重试）",
				name, uid, checkinAttempts(checkinErr))
		}
	} else {
		r.OK = true
		r.Msg = "ok"
		s.clearRetry(uid) // 签到成功，清除待重试状态
	}
	// 无论签到成败都查余额（已签到等业务错误下余额刷新仍有效）
	remain, rerr := s.cfg.Upstream.UserResource(a)
	if rerr != nil {
		log.Printf("checkin credits failed platform=%s uid=%s err=%v", name, uid, rerr)
		r.OK = false // 签到后的积分确认失败，整次操作向 GUI 报告失败
		if r.Msg == "" {
			r.Msg = "余额查询失败"
		} else {
			r.Msg += "；余额查询失败"
		}
	} else {
		r.Remain, r.HasRemain = remain, true
		log.Printf("checkin credits platform=%s uid=%s remain=%d", name, uid, remain)
		s.cfg.Pool.ReenableIfCredits(uid, remain)
	}
	s.cfg.Pool.RecordCheckin(uid, r.OK, r.Msg)
	s.notifyCheckin(r)
	return r
}

// checkinThrottled 由上游客户端实现的"签到被瞬时限流"标记。
//
// 与 rateLimited 的区别（两者都表示"限流"，但消费方不同）：
//   - rateLimited   ：通用请求限流，供 isRateLimited 判定
//   - checkinThrottled：**签到专属**的 9074，供调度器安排延迟重试
//
// 为什么要分开：*traework.ErrCheckinRateLimited 的 IsRateLimited() 恒返回
// false（这是有意设计 —— 它的重试已在请求内同步做完，不该再走 isRateLimited
// 那套通用判定）。所以判断 9074 必须用**独立接口**，否则永远判不出来。
type checkinThrottled interface {
	IsCheckinThrottled() bool
}

// isCheckinThrottled 判断错误是否为签到 9074（瞬时限流，应安排延迟重试）。
//
// ⚠️ 不要用 isRateLimited 代替：见 checkinThrottled 的说明，那样会恒为 false。
func isCheckinThrottled(err error) bool {
	if err == nil {
		return false
	}
	var ct checkinThrottled
	if errors.As(err, &ct) {
		return ct.IsCheckinThrottled()
	}
	// 兜底：上游若只暴露裸错误而不实现接口，用错误文本识别。
	// 仅在包含明确的 9074 标记时才认定，避免误伤其它含 "checkin" 的错误。
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "9074") || strings.Contains(s, "transient throttle")
}

// checkinAttempts 从签到限流错误里取出已尝试的轮换次数（供日志用）。
// 取不到时返回 0。
func checkinAttempts(err error) int {
	var att interface{ AttemptCount() int }
	if errors.As(err, &att) {
		return att.AttemptCount()
	}
	return 0
}

// rateLimited 由上游客户端实现的"可重试瞬时错误"标记。
// 用接口探测而非直接 import 具体平台包，避免 scheduler 与各上游耦合。
type rateLimited interface {
	IsRateLimited() bool
}

// isRateLimited 判断错误是否为上游**通用**高峰限流（瞬时、可重试，非账号异常）。
// 同时兼容两类表达：
//   - 上游客户端定义的错误类型实现了 IsRateLimited() bool
//   - provider.Error 被分类为 ErrSoftRate（429 类软限流）
//
// ⚠️ 状态说明（v0.7.5）：本函数目前**没有生产调用点**。
//
// 它此前唯一的调用点是签到 9074 分支，而那是个永不成立的死分支
// （*ErrCheckinRateLimited.IsRateLimited() 恒为 false），已改用
// isCheckinThrottled。保留本函数是因为：
//
//  1. rateLimited 接口是各上游表达"通用限流"的公共约定，值得留着；
//  2. provider.ErrSoftRate 的映射仍需有地方承接（HTTP 侧在用它）。
//
// 若将来要给"通用请求限流"加调度器级重试，直接复用本函数即可。
// 但**签到 9074 不要用它** —— 那是 checkinThrottled 的职责。
func isRateLimited(err error) bool {
	if err == nil {
		return false
	}
	var rl rateLimited
	if errors.As(err, &rl) {
		return rl.IsRateLimited()
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe.Kind == provider.ErrSoftRate
	}
	return false
}

// deviceClaimed 由上游客户端实现的"本设备今日额度已被领走"标记。
// 用接口探测而非直接 import 具体平台包，避免 scheduler 与各上游耦合。
type deviceClaimed interface {
	IsDeviceClaimed() bool
}

// isDeviceClaimed 判断错误是否为「本设备今日签到额度已被其它账号领走」。
//
// 与 isAlready 的区别（两者上游文案都含"已签到"，极易混淆）：
//   - deviceClaimed：额度被**同设备的别的账号**领走了 → 本账号今天没份，OK=false
//   - already      ：本账号自己签过了 → 幂等成功，OK=true
func isDeviceClaimed(err error) bool {
	if err == nil {
		return false
	}
	var dc deviceClaimed
	return errors.As(err, &dc) && dc.IsDeviceClaimed()
}

// isAlready 只匹配明确的“今日已签到”，不能因错误文本包含 checkin 就判成功。
func isAlready(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "已签到") ||
		strings.Contains(s, "already check") ||
		strings.Contains(s, "already checked") ||
		strings.Contains(s, "code=9095")
}

func shortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

func (s *Scheduler) name() string {
	if s.cfg.Name != "" {
		return s.cfg.Name
	}
	return "unknown"
}

func (s *Scheduler) refreshForCheckin(a *auth.Auth, uid string) error {
	name := s.name()
	log.Printf("refresh start platform=%s uid=%s reason=checkin", name, uid)
	if err := s.cfg.Upstream.RefreshToken(a); err != nil {
		log.Printf("refresh failed platform=%s uid=%s err=%v", name, uid, err)
		return err
	}
	if err := a.SaveAtomic(); err != nil {
		log.Printf("refresh save failed platform=%s uid=%s err=%v", name, uid, err)
		return fmt.Errorf("refresh save: %w", err)
	}
	log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, uid, a.ExpiresAt)
	return nil
}

func isSessionDead(err error) bool {
	var ue *provider.Error
	return errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
func (s *Scheduler) RunKeepaliveNow() {
	name := s.name()
	log.Printf("refresh batch start platform=%s", name)
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			log.Printf("refresh skip platform=%s uid=%s reason=disabled", name, st.UID)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshToken == "" {
			msg := "no refresh token"
			log.Printf("refresh skip platform=%s uid=%s reason=%s", name, st.UID, msg)
			s.notifyRefresh(st.UID, false, msg)
			continue
		}
		log.Printf("refresh start platform=%s uid=%s reason=keepalive", name, st.UID)
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("refresh failed platform=%s uid=%s err=%v", name, st.UID, err)
			var ue *provider.Error
			if errors.As(err, &ue) && ue.Kind == provider.ErrSessionDead {
				s.cfg.Pool.Disable(st.UID, "12153 session dead")
				log.Printf("refresh disabled platform=%s uid=%s reason=session_dead", name, st.UID)
			}
			s.notifyRefresh(st.UID, false, err.Error())
			continue
		}
		if err := a.SaveAtomic(); err != nil {
			log.Printf("refresh save failed platform=%s uid=%s err=%v", name, st.UID, err)
			s.notifyRefresh(st.UID, false, "refresh save: "+err.Error())
			continue
		}
		log.Printf("refresh success platform=%s uid=%s expires_at=%d", name, st.UID, a.ExpiresAt)
		s.notifyRefresh(st.UID, true, "ok")
	}
	log.Printf("refresh batch done platform=%s", name)
}
