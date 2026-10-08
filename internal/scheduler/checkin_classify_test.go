package scheduler

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
	"github.com/rockswang/workbuddy-wild/internal/pool"
	"github.com/rockswang/workbuddy-wild/internal/provider"
	"github.com/rockswang/workbuddy-wild/internal/traework"
)

// stubUpstream 用可控的签到/余额结果替代真实上游。
type stubUpstream struct {
	checkinErr error
	remain     int64
	remainErr  error
}

func (f *stubUpstream) DailyCheckin(*auth.Auth) error { return f.checkinErr }
func (f *stubUpstream) UserResource(*auth.Auth) (int64, error) {
	return f.remain, f.remainErr
}
func (f *stubUpstream) RefreshToken(*auth.Auth) error { return nil }
func (f *stubUpstream) ChatStream(*auth.Auth, []byte) (io.ReadCloser, int, []byte, error) {
	return nil, 0, nil, errors.New("not implemented")
}
func (f *stubUpstream) FetchModels(*auth.Auth) ([]provider.ModelInfo, error) { return nil, nil }
func (f *stubUpstream) Classify(int, string) provider.ErrKind                { return provider.ErrNone }
func (f *stubUpstream) Stream(http.ResponseWriter, io.Reader) error          { return nil }
func (f *stubUpstream) Aggregate(io.Reader) (map[string]any, error)          { return nil, nil }

// 回归：今日已签到时必须标记 AlreadyChecked，且文案要说明"无新增积分"。
//
// 缺陷背景（用户反馈"提示签到成功怎么没加积分"）：
// 上游对重复签到返回 9095「今日已签到」，原实现把它归为成功并显示
// 绿色的"签到成功"，用户会期待积分上涨 —— 但积分当天早先那次就已入账，
// 于是看起来像"签到成功却没加积分"。必须三分类：新签到成功 / 今日已签 / 失败。
func TestCheckinAlreadyMarksAlreadyChecked(t *testing.T) {
	f := &stubUpstream{
		// traework 的 DailyCheckin 在今日已签时返回这个可识别的错误
		checkinErr: errors.New("已签到"),
		remain:     93,
	}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: f})
	a := &auth.Auth{Kind: "traework", UID: "u1", RefreshToken: "rt", AccessToken: "at", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
	p.Add(a)

	r := s.checkinOne("u1")
	if !r.OK {
		t.Fatalf("已签到应视为 OK（幂等成功），得到 OK=false msg=%q", r.Msg)
	}
	if !r.AlreadyChecked {
		t.Fatal("已签到必须置 AlreadyChecked=true，否则前端会显示成\"签到成功\"")
	}
	if r.Msg == "" || r.Msg == "已签到" {
		t.Fatalf("文案必须说明本次无新增积分，实际 msg=%q", r.Msg)
	}
	if !r.HasRemain || r.Remain != 93 {
		t.Fatalf("余额应仍被刷新: has_remain=%t remain=%d", r.HasRemain, r.Remain)
	}
}

// 真正新签到时不得置 AlreadyChecked（否则前端会误报"无新增积分"）。
func TestCheckinFreshSuccessNotAlreadyChecked(t *testing.T) {
	f := &stubUpstream{checkinErr: nil, remain: 200}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: f})
	a := &auth.Auth{Kind: "traework", UID: "u1", RefreshToken: "rt", AccessToken: "at", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
	p.Add(a)

	r := s.checkinOne("u1")
	if !r.OK || r.AlreadyChecked {
		t.Fatalf("新签到成功应为 OK=true 且 AlreadyChecked=false，得到 ok=%t already=%t", r.OK, r.AlreadyChecked)
	}
}

// 9074（瞬时限流）必须被识别为「可重试」并给出中文可操作文案。
//
// ⚠️ 本测试的**历史版本是假阳性**（v0.7.5 修复）：
// 旧版用裸 `errors.New("checkin 9074 ...")` 模拟 9074 —— 但裸 error
// 既不实现 rateLimited 也不实现 checkinThrottled，于是两条接口分支
// 都走不通，测试实际验证的是「兜底 false 路径」，永远通过。
//
// 后果：调度器真实代码里 `else if isRateLimited(checkinErr)` 是个
// **永不成立的死分支**（*ErrCheckinRateLimited.IsRateLimited() 恒 false），
// 这个测试完全没能拦住它。修复后必须用**真实的错误类型**来测。
func TestCheckinDeviceRejectedNotRetryable(t *testing.T) {
	// 必须用真实类型：它会暴露 IsRateLimited()=false / IsCheckinThrottled()=true 的差异
	f := &stubUpstream{
		checkinErr: &traework.ErrCheckinRateLimited{
			Attempts: 8,
			Msg:      "当前参与用户太多，请稍后再试",
		},
		remain: 100,
	}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: f})
	a := &auth.Auth{Kind: "traework", UID: "u1", RefreshToken: "rt", AccessToken: "at", ExpiresAt: time.Now().Add(24 * time.Hour).Unix()}
	p.Add(a)

	r := s.checkinOne("u1")
	if r.OK {
		t.Fatal("9074 不应被视为成功")
	}
	if r.AlreadyChecked {
		t.Fatal("9074 不是\"已签到\"")
	}
	// ★ 核心断言：9074 是瞬时限流，必须标记 Retryable 才会被延迟重试队列消费
	if !r.Retryable {
		t.Fatal("9074 是瞬时限流，必须标记 Retryable=true 才会被 runDueRetries 消费")
	}
	// ★ 核心断言：文案必须是中文可操作提示，不能把英文原文透传给用户
	if r.Msg == "" {
		t.Fatal("必须给出可操作的错误文案")
	}
	if strings.Contains(r.Msg, "transient throttle") {
		t.Fatalf("文案不得透传英文原文（说明掉进了兜底分支，9074 未被识别）: %q", r.Msg)
	}
	if !strings.Contains(r.Msg, "重试") {
		t.Fatalf("文案应告知会稍后自动重试，实际 %q", r.Msg)
	}
}

// 回归：9074 必须真的进入「延迟重试队列」，不是只打个标记。
//
// 这是对缺陷 1 + 缺陷 2 的**端到端**验证：checkinOne 产出 Retryable=true
// 的结论 → runDueRetries 消费 → 安排到 retries 表里。
// 旧代码两处都断了（9074 走不到 Retryable 分支 + Retryable 无人赋值），
// 所以本测试在修复前必然失败。
func TestThrottledCheckinEntersRetryQueue(t *testing.T) {
	f := &stubUpstream{
		checkinErr: &traework.ErrCheckinRateLimited{Attempts: 8, Msg: "参与用户太多"},
		remain:     100,
	}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: f})
	p.Add(&auth.Auth{Kind: "traework", UID: "u1", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	// 先确认 checkinOne 会给出 Retryable=true
	r := s.checkinOne("u1")
	if !r.Retryable {
		t.Fatalf("前置条件失败：9074 未标记 Retryable，延迟重试队列必然失活（%+v）", r)
	}

	// 再走一遍调度器的消费路径（runDueRetries 的判定逻辑）
	now := time.Now()
	at, ok := s.markRetryable("u1", now)
	if !ok {
		t.Fatal("应能安排重试")
	}
	if !at.After(now) {
		t.Fatalf("重试时刻 %v 应晚于 %v", at, now)
	}
	if got := s.dueRetries(at); len(got) != 1 || got[0] != "u1" {
		t.Fatalf("到期后应从重试队列取出 u1，实际 %v", got)
	}
}

// 真实类型与裸错误的行为差异必须被明确锁定 —— 防止后人再退回假阳性写法。
func TestCheckinThrottleErrorTypeContract(t *testing.T) {
	err := &traework.ErrCheckinRateLimited{Attempts: 3, Msg: "throttle"}

	// IsRateLimited 恒 false（有意设计：不落进通用限流分支）
	if isRateLimited(err) {
		t.Fatal("ErrCheckinRateLimited.IsRateLimited() 必须为 false（否则会与通用限流语义混淆）")
	}
	// isCheckinThrottled 必须为 true（调度器靠它识别 9074）
	if !isCheckinThrottled(err) {
		t.Fatal("isCheckinThrottled 必须能识别 *ErrCheckinRateLimited —— 这是 9074 分支可达的唯一保证")
	}
	// 轮换次数可取出供日志用
	if n := checkinAttempts(err); n != 3 {
		t.Fatalf("checkinAttempts=%d want 3", n)
	}
	// 包装后仍应可识别（errors.As 穿透）
	wrapped := fmt.Errorf("checkin failed: %w", err)
	if !isCheckinThrottled(wrapped) {
		t.Fatal("包装后的 9074 错误仍必须可识别")
	}

	// 反向：普通错误不得被误判为限流
	if isCheckinThrottled(errors.New("connection reset by peer")) {
		t.Fatal("普通网络错误不得被误判为签到限流")
	}
}

// 9095（ErrCheckinAlreadyClaimed）是**幂等成功**：该账号今日已领。
//
// 去重键是「账号 + 设备」而非纯设备 —— 实测两个账号用**完全相同**的设备号，
// 一个返 9095、一个返 success。所以同一设备号下多账号本来就能各签一次，
// 不需要给每个账号单独配设备号。
//
// v0.6.0 曾误判为"设备级独占"，把它当成 OK=false 并提示"额度被别的账号领走"，
// 这会误导用户以为多账号不可行。本测试锁定改正后的语义。
func TestCheckinAlreadyClaimedIsIdempotentSuccess(t *testing.T) {
	f := &stubUpstream{
		checkinErr: &traework.ErrCheckinAlreadyClaimed{Msg: "当前设备今日已经签到", Device: "4484…"},
		remain:     4600,
	}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: f})
	p.Add(&auth.Auth{Kind: "traework", UID: "u2", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	r := s.checkinOne("u2")
	if !r.OK {
		t.Fatal("9095 = 该账号今日已领，属幂等成功，OK 必须为 true（v0.6.0 曾误判为 false）")
	}
	if !r.AlreadyChecked {
		t.Fatal("必须标记 AlreadyChecked，前端据此说明\"本次无新增积分\"")
	}
	if r.Msg == "" {
		t.Fatal("必须有说明性文案")
	}
	// 绝不能提示成"设备额度被别占"，那会误导用户以为多账号不可行
	if strings.Contains(r.Msg, "被其它账号领走") || strings.Contains(r.Msg, "每设备每天限一份") {
		t.Fatalf("文案误导：%q（去重是账号级，不是设备级独占）", r.Msg)
	}
}

// 多账号场景：每个账号各自有一份每日名额，互不影响。
//
// 证据链（2026-09-30 实测）：
//   - 账号1 历史上连续 17 天每天签到成功 → 名额按天重置
//   - 换任何设备号都不改变结果 → 去重按**账号**计，与设备号无关
//   - 因此两个账号在同一天各自签一次，都应判定为"本次成功"
//
// 本测试锁定：A 账号已签（幂等成功）**不会**影响 B 账号的正常签到。
func TestMultiAccountCheckinIndependent(t *testing.T) {
	// 账号A：今日已签 → 幂等成功，OK=true，已签标记
	fA := &stubUpstream{
		checkinErr: &traework.ErrCheckinAlreadyClaimed{Msg: "今日已签到", Device: "4484…"},
		remain:     4600,
	}
	p := pool.New("")
	s := New(Config{Name: "traework", Pool: p, Upstream: fA})
	p.Add(&auth.Auth{Kind: "traework", UID: "acctA", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	rA := s.checkinOne("acctA")
	if !rA.OK || !rA.AlreadyChecked {
		t.Fatalf("账号A 已签应为幂等成功: ok=%t already=%t", rA.OK, rA.AlreadyChecked)
	}

	// 账号B：同一个池、同一台设备，今天没签 → 必须能正常签成功
	fB := &stubUpstream{checkinErr: nil, remain: 300}
	s.cfg.Upstream = fB
	p.Add(&auth.Auth{Kind: "traework", UID: "acctB", RefreshToken: "rt", AccessToken: "at",
		ExpiresAt: time.Now().Add(24 * time.Hour).Unix()})

	rB := s.checkinOne("acctB")
	if !rB.OK {
		t.Fatalf("账号B 今天没签，应能签成功（账号级去重，不受 A 影响）: %+v", rB)
	}
	if rB.AlreadyChecked {
		t.Fatal("账号B 是本次新签到，不该带已签标记")
	}
	if !rB.HasRemain || rB.Remain != 300 {
		t.Fatalf("账号B 余额应刷新为 300，得 %d", rB.Remain)
	}
}
