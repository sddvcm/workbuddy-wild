// Package pool 账号池：内存索引 + 冷却/禁用状态机 + state.json 持久化。
// 挑选策略：healthy 账号中按 Strategy 选出一个（默认积分最多）。
package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// CoolKind 冷却类型。
type CoolKind int

const (
	CoolHard CoolKind = iota // 余额不足 → 长冷却
	CoolSoft                 // 429 → 短冷却
	CoolErr                  // 连续错误 → 中冷却
)

// ---------------------------------------------------------------------------
// 选号策略
// ---------------------------------------------------------------------------

// Strategy 账号挑选策略。
type Strategy string

const (
	// StrategyCredits 优先积分：healthy 中剩余积分最多者（默认，与旧行为一致）。
	StrategyCredits Strategy = "credits"
	// StrategyExpire 优先过期：有积分的前提下，凭证最快过期者优先被消耗，
	// 避免 token 到期浪费额度；无法判断过期时间的排最后。
	StrategyExpire Strategy = "expire"
	// StrategyRoundRobin 负载均衡：从上次调用位置的下一个账号开始，轮流使用。
	StrategyRoundRobin Strategy = "roundrobin"
)

// AllStrategies 全部合法策略（用于校验与前端下拉）。
var AllStrategies = []Strategy{StrategyCredits, StrategyExpire, StrategyRoundRobin}

// ParseStrategy 解析策略串并校验；空串返回默认策略。
func ParseStrategy(s string) (Strategy, bool) {
	v := Strategy(strings.ToLower(strings.TrimSpace(s)))
	if v == "" {
		return StrategyCredits, true
	}
	for _, k := range AllStrategies {
		if v == k {
			return v, true
		}
	}
	return StrategyCredits, false
}

// Label 策略中文名（日志/面板用）。
func (s Strategy) Label() string {
	switch s {
	case StrategyExpire:
		return "优先过期"
	case StrategyRoundRobin:
		return "负载均衡"
	default:
		return "优先积分"
	}
}

func (k CoolKind) String() string {
	switch k {
	case CoolHard:
		return "hard_credit"
	case CoolSoft:
		return "soft_rate"
	case CoolErr:
		return "error_threshold"
	}
	return "unknown"
}

// Status 单个账号对外暴露的状态（脱敏）。
type Status struct {
	UID            string    `json:"uid"`
	Nickname       string    `json:"nickname,omitempty"`
	Credits        int64     `json:"credits"`
	Cooling        bool      `json:"cooling"`
	Until          time.Time `json:"until,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	Disabled       bool      `json:"disabled"`
	ErrCount       int       `json:"err_count,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`

	// LastUsedAt 最近一次被挑选调用的时间（含成功与失败），零值 = 从未调用。
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	// LastCallCredits 最后一次调用前的积分快照（供面板显示消耗量）。
	LastCallCredits int64 `json:"last_call_credits,omitempty"`
}

type entry struct {
	a        *auth.Auth
	credits  int64
	disabled bool
	reason   string
	until    time.Time
	errCount int

	lastCheckinOK  bool
	lastCheckinAt  time.Time
	lastCheckinMsg string

	lastUsedAt      time.Time // 最近一次被挑选调用的时间
	lastCallCredits int64     // 调用时的积分快照
}

func (e *entry) healthy(now time.Time) bool {
	if e.disabled {
		return false
	}
	if !e.until.IsZero() && now.Before(e.until) {
		return false
	}
	return true
}

// stateFile 持久化格式。
type accountState struct {
	Credits        int64     `json:"credits"`
	Disabled       bool      `json:"disabled"`
	Reason         string    `json:"reason,omitempty"`
	Until          time.Time `json:"until,omitempty"`
	LastCheckinOK  bool      `json:"last_checkin_ok,omitempty"`
	LastCheckinAt  time.Time `json:"last_checkin_at,omitempty"`
	LastCheckinMsg string    `json:"last_checkin_msg,omitempty"`

	LastUsedAt      time.Time `json:"last_used_at,omitempty"`
	LastCallCredits int64     `json:"last_call_credits,omitempty"`
}

type stateFile struct {
	Accounts map[string]accountState `json:"accounts"`
	// RRLast 负载均衡游标：上一次被选中的 uid（下轮从它的下一个开始）。
	RRLast string `json:"rr_last,omitempty"`
}

// Pool 账号池。
type Pool struct {
	mu       sync.RWMutex
	byUID    map[string]*entry
	stateFp  string
	strategy Strategy
	rrLast   string // 负载均衡游标：上一次选中的 uid
}

// New 构建池；stateFp 非空时尝试加载旧状态。
// 策略默认 StrategyCredits（与历史行为一致）；用 SetStrategy 切换。
func New(stateFp string) *Pool {
	p := &Pool{byUID: map[string]*entry{}, stateFp: stateFp, strategy: StrategyCredits}
	if stateFp != "" {
		p.load()
	}
	return p
}

// SetStrategy 切换选号策略。
func (p *Pool) SetStrategy(s Strategy) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.strategy = s
}

// GetStrategy 返回当前策略。
func (p *Pool) GetStrategy() Strategy {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.strategy
}

// Add 加入账号；已存在则保留原状态、更新凭证。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	for _, a := range auths {
		seen[a.UID] = true
		if e, ok := p.byUID[a.UID]; ok {
			e.a = a
		} else {
			p.byUID[a.UID] = &entry{a: a}
		}
	}
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
		}
	}
}

// Pick 按当前策略返回一个可用账号；无可用返回 nil。
func (p *Pool) Pick() *auth.Auth {
	return p.PickExcluding(nil)
}

// PickExcluding 按当前策略挑选，跳过 tried 中的 uid（请求级轮换）。
// 关键约束：每次挑选都必须单调推进——用 tried 把上一次结果排除掉，
// 才能保证「本次选中 ≠ 上次选中」。否则负载均衡/优先过期会反复选中同一个
// 快照值最优的账号。若排除后无可用账号，说明已轮完一圈，允许回到起点。
func (p *Pool) PickExcluding(tried map[string]bool) *auth.Auth {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()

	cands := p.healthyLocked(now, tried)
	if len(cands) == 0 && len(tried) > 0 {
		// 已试完全部可用账号：允许重复（调用方通常也该结束重试循环了）
		cands = p.healthyLocked(now, nil)
	}
	if len(cands) == 0 {
		return nil
	}

	switch p.strategy {
	case StrategyRoundRobin:
		return p.pickRoundRobinLocked(cands)
	case StrategyExpire:
		return p.pickExpireLocked(cands)
	default:
		return p.pickCreditsLocked(cands)
	}
}

// healthyLocked 收集当前可用（未禁用、未冷却、未在 tried 中）的账号。
// 调用方需持有 p.mu。
func (p *Pool) healthyLocked(now time.Time, tried map[string]bool) []*entry {
	out := make([]*entry, 0, len(p.byUID))
	for uid, e := range p.byUID {
		if tried != nil && tried[uid] {
			continue
		}
		if !e.healthy(now) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// pickCreditsLocked 优先积分：剩余积分最多者；同分时取 UID 较小者（稳定）。
func (p *Pool) pickCreditsLocked(cands []*entry) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if e.credits > best.credits ||
			(e.credits == best.credits && e.a.UID < best.a.UID) {
			best = e
		}
	}
	return best.a
}

// pickExpireLocked 优先过期：有积分者优先；积分相同时凭证最快过期者优先；
// 过期时间未知（零值）排在有积分者之后。
func (p *Pool) pickExpireLocked(cands []*entry) *auth.Auth {
	best := cands[0]
	for _, e := range cands[1:] {
		if expireBetter(e, best) {
			best = e
		}
	}
	return best.a
}

// expireBetter 判断 a 是否比 b 更应该优先被消耗。
func expireBetter(a, b *entry) bool {
	// 没积分的账号不该被浪费（调用会直接失败）。
	aHas, bHas := a.credits > 0, b.credits > 0
	if aHas != bHas {
		return aHas
	}
	aExp, bExp := a.a.ExpiresAt, b.a.ExpiresAt
	switch {
	case aExp > 0 && bExp > 0:
		if aExp != bExp {
			return aExp < bExp // 先过期者先用
		}
	case aExp > 0:
		return true // a 有过期时间，b 未知 → a 优先
	case bExp > 0:
		return false
	}
	// 过期时间也无法区分 → 退化为比积分，再退化到 UID 稳定排序
	if a.credits != b.credits {
		return a.credits > b.credits
	}
	return a.a.UID < b.a.UID
}

// pickRoundRobinLocked 负载均衡：按 UID 排序后，从 rrLast 的下一个开始轮转。
func (p *Pool) pickRoundRobinLocked(cands []*entry) *auth.Auth {
	sort.Slice(cands, func(i, j int) bool { return cands[i].a.UID < cands[j].a.UID })
	start := 0
	if p.rrLast != "" {
		for i, e := range cands {
			if e.a.UID == p.rrLast {
				start = (i + 1) % len(cands)
				break
			}
		}
	}
	chosen := cands[start]
	p.rrLast = chosen.a.UID
	if p.stateFp != "" {
		p.saveLocked()
	}
	return chosen.a
}

// NotifyUsed 记录一次实际调用（策略为轮询时推进游标）。
// 调用方在请求发出前调用；此时 lastUsedAt 即"正在调用"的时间戳。
func (p *Pool) NotifyUsed(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return
	}
	e.lastUsedAt = time.Now()
	e.lastCallCredits = e.credits
	if p.strategy == StrategyRoundRobin {
		p.rrLast = uid
	}
	p.saveLocked()
}

// SetCredits 更新账号余额。
func (p *Pool) SetCredits(uid string, credits int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = credits
	}
	p.saveLocked()
}

// Cooldown 冷却账号至 now+d。
func (p *Pool) Cooldown(uid string, kind CoolKind, d time.Duration, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Now().Add(d)
		e.reason = reason
		e.errCount = 0
	}
	p.saveLocked()
}

// Disable 永久禁用（session 死亡），需人工重登后手工恢复或文件替换。
func (p *Pool) Disable(uid, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.disabled = true
		e.reason = reason
	}
	p.saveLocked()
}

// ReenableIfCredits 签到后解冻：仅当 remain > 0 且账号处于冷却（非禁用）时恢复。
func (p *Pool) ReenableIfCredits(uid string, remain int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.credits = remain
		if remain > 0 && !e.disabled {
			e.until = time.Time{}
			e.reason = ""
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// RecordCheckin 记录一次签到结果（含错误信息），随 state.json 持久化。
func (p *Pool) RecordCheckin(uid string, ok bool, msg string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.lastCheckinOK = ok
		e.lastCheckinAt = time.Now()
		e.lastCheckinMsg = msg
	}
	p.saveLocked()
}

// Remove 从池中移除账号（内存 + 状态文件）；auth 文件删除由调用方负责。
func (p *Pool) Remove(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.byUID, uid)
	p.saveLocked()
}

// NoteError 记录一次非余额/非 429 错误；达到 threshold 自动冷却 d 时长。
func (p *Pool) NoteError(uid string, threshold int, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount++
		if e.errCount >= threshold {
			e.until = time.Now().Add(d)
			e.reason = "consecutive errors"
			e.errCount = 0
		}
	}
	p.saveLocked()
}

// NoteSuccess 成功请求重置错误计数。
func (p *Pool) NoteSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.errCount = 0
	}
}

// Status 查询单账号状态。
func (p *Pool) Status(uid string) (Status, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return Status{}, false
	}
	return p.statusOf(uid, e), true
}

// AuthByUID 返回账号的完整凭证（给调度器/运维接口用）。
func (p *Pool) AuthByUID(uid string) *auth.Auth {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e, ok := p.byUID[uid]; ok {
		return e.a
	}
	return nil
}

// List 返回所有账号状态（按 UID 排序，稳定输出）。
func (p *Pool) List() []Status {
	p.mu.RLock()
	defer p.mu.RUnlock()
	uids := make([]string, 0, len(p.byUID))
	for uid := range p.byUID {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	out := make([]Status, 0, len(uids))
	for _, uid := range uids {
		out = append(out, p.statusOf(uid, p.byUID[uid]))
	}
	return out
}

func (p *Pool) statusOf(uid string, e *entry) Status {
	now := time.Now()
	return Status{
		UID:             uid,
		Nickname:        e.a.Nickname,
		Credits:         e.credits,
		Cooling:         !e.until.IsZero() && now.Before(e.until),
		Until:           e.until,
		Reason:          e.reason,
		Disabled:        e.disabled,
		ErrCount:        e.errCount,
		LastCheckinOK:   e.lastCheckinOK,
		LastCheckinAt:   e.lastCheckinAt,
		LastCheckinMsg:  e.lastCheckinMsg,
		LastUsedAt:      e.lastUsedAt,
		LastCallCredits: e.lastCallCredits,
	}
}

// ---------------------------------------------------------------------------
// 持久化
// ---------------------------------------------------------------------------

func (p *Pool) load() {
	raw, err := os.ReadFile(p.stateFp)
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(raw, &sf) != nil {
		return
	}
	for uid, s := range sf.Accounts {
		p.byUID[uid] = &entry{
			a:              &auth.Auth{UID: uid}, // placeholder，Add 时会换成完整凭证
			credits:        s.Credits,
			disabled:       s.Disabled,
			reason:         s.Reason,
			until:          s.Until,
			lastCheckinOK:  s.LastCheckinOK,
			lastCheckinAt:  s.LastCheckinAt,
			lastCheckinMsg: s.LastCheckinMsg,

			lastUsedAt:      s.LastUsedAt,
			lastCallCredits: s.LastCallCredits,
		}
	}
	p.rrLast = sf.RRLast
}

func (p *Pool) saveLocked() {
	if p.stateFp == "" {
		return
	}
	sf := stateFile{Accounts: map[string]accountState{}, RRLast: p.rrLast}
	for uid, e := range p.byUID {
		sf.Accounts[uid] = accountState{
			Credits:        e.credits,
			Disabled:       e.disabled,
			Reason:         e.reason,
			Until:          e.until,
			LastCheckinOK:  e.lastCheckinOK,
			LastCheckinAt:  e.lastCheckinAt,
			LastCheckinMsg: e.lastCheckinMsg,

			LastUsedAt:      e.lastUsedAt,
			LastCallCredits: e.lastCallCredits,
		}
	}
	raw, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(p.stateFp); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := p.stateFp + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p.stateFp)
}
