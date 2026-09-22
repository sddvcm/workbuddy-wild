package pool

import (
	"testing"
	"time"

	"github.com/rockswang/workbuddy-wild/internal/auth"
)

// newStrategyPool 构造三账号池（积分 100 / 500 / 300）。
func newStrategyPool(t *testing.T, s Strategy) *Pool {
	t.Helper()
	p := New("")
	p.SetStrategy(s)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 300)
	return p
}

func TestParseStrategy(t *testing.T) {
	cases := []struct {
		in    string
		want  Strategy
		valid bool
	}{
		{"", StrategyCredits, true},
		{"credits", StrategyCredits, true},
		{"CREDITS", StrategyCredits, true},
		{"expire", StrategyExpire, true},
		{" roundrobin ", StrategyRoundRobin, true},
		{"bogus", StrategyCredits, false},
	}
	for _, c := range cases {
		got, ok := ParseStrategy(c.in)
		if got != c.want || ok != c.valid {
			t.Errorf("ParseStrategy(%q)=%s,%v want %s,%v", c.in, got, ok, c.want, c.valid)
		}
	}
}

// 默认（未设置）必须保持历史行为：选积分最多。
func TestDefaultStrategyIsCredits(t *testing.T) {
	p := New("")
	if p.GetStrategy() != StrategyCredits {
		t.Fatalf("默认策略应为 credits，实际 %s", p.GetStrategy())
	}
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 10)
	p.SetCredits("u2", 99)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2", got)
	}
}

func TestPickCreditsStrategy(t *testing.T) {
	p := newStrategyPool(t, StrategyCredits)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2", got)
	}
}

// 负载均衡：连续挑选必须轮转，且走完一圈后回到起点。
func TestPickRoundRobinRotates(t *testing.T) {
	p := newStrategyPool(t, StrategyRoundRobin)
	// healthy 集合按 UID 排序为 u1,u2,u3；游标为空从 u1 开始。
	want := []string{"u1", "u2", "u3", "u1", "u2", "u3"}
	for i, w := range want {
		got := p.Pick()
		if got == nil || got.UID != w {
			t.Fatalf("第 %d 次 pick=%v want %s", i+1, got, w)
		}
	}
}

// 负载均衡：跳过冷却账号后仍要轮转，不能卡死在同一账号。
func TestPickRoundRobinSkipsCooling(t *testing.T) {
	p := newStrategyPool(t, StrategyRoundRobin)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	for i := 0; i < 4; i++ {
		got := p.Pick()
		if got == nil {
			t.Fatalf("第 %d 次 pick 为 nil", i+1)
		}
		if got.UID == "u1" {
			t.Fatalf("第 %d 次选中了冷却账号 u1", i+1)
		}
	}
}

// 负载均衡：游标要写进 state.json 并能恢复。
func TestRoundRobinCursorPersists(t *testing.T) {
	fp := t.TempDir() + "/state.json"
	p := New(fp)
	p.SetStrategy(StrategyRoundRobin)
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	if got := p.Pick(); got == nil || got.UID != "u1" {
		t.Fatalf("首次 pick=%v want u1", got)
	}
	// 重新加载：游标 u1 → 下次应从 u2 开始
	p2 := New(fp)
	p2.SetStrategy(StrategyRoundRobin)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	if got := p2.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("重载后 pick=%v want u2（游标未持久化）", got)
	}
}

// 优先过期：有积分者优先，且过期早者先用。
func TestPickExpirePrefersSoonestExpiry(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyExpire)
	now := time.Now().Unix()
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: now + 7200}) // 2h 后过期
	p.Add(&auth.Auth{UID: "u2", ExpiresAt: now + 600})  // 10min 后过期
	p.Add(&auth.Auth{UID: "u3", ExpiresAt: now + 3600}) // 1h 后过期
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 100)
	p.SetCredits("u3", 100)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2（最快过期）", got)
	}
}

// 优先过期：没积分的账号不应被优先消耗（否则请求必失败）。
func TestPickExpireSkipsZeroCredits(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyExpire)
	now := time.Now().Unix()
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: now + 60}) // 最快过期但没积分
	p.Add(&auth.Auth{UID: "u2", ExpiresAt: now + 7200})
	p.SetCredits("u1", 0)
	p.SetCredits("u2", 50)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2（有积分）", got)
	}
}

// 优先过期：过期时间也相同时按积分降序，保证结果稳定。
func TestPickExpireTieBreakByCredits(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyExpire)
	exp := time.Now().Unix() + 3600
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: exp})
	p.Add(&auth.Auth{UID: "u2", ExpiresAt: exp})
	p.SetCredits("u1", 10)
	p.SetCredits("u2", 900)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2（同过期时间取积分高）", got)
	}
}

// 优先过期：无过期信息的账号排在有信息者之后。
func TestPickExpireUnknownExpiryLast(t *testing.T) {
	p := New("")
	p.SetStrategy(StrategyExpire)
	p.Add(&auth.Auth{UID: "u1", ExpiresAt: 0})                             // 未知
	p.Add(&auth.Auth{UID: "u2", ExpiresAt: time.Now().Unix() + 86400})     // 明天
	p.SetCredits("u1", 9999)
	p.SetCredits("u2", 1)
	if got := p.Pick(); got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2（有过期信息者优先）", got)
	}
}

// NotifyUsed 记录调用时间，供面板显示"正在调用"。
func TestNotifyUsedRecordsTimestamp(t *testing.T) {
	p := newStrategyPool(t, StrategyCredits)
	p.NotifyUsed("u2")
	st, ok := p.Status("u2")
	if !ok {
		t.Fatal("u2 状态缺失")
	}
	if st.LastUsedAt.IsZero() {
		t.Fatal("LastUsedAt 未记录")
	}
	if time.Since(st.LastUsedAt) > 3*time.Second {
		t.Fatalf("LastUsedAt 时间异常：%v", st.LastUsedAt)
	}
	if st.LastCallCredits != 500 {
		t.Fatalf("LastCallCredits=%d want 500", st.LastCallCredits)
	}
}

// 请求级轮换：PickExcluding 必须每次都换账号（不能反复选同一个）。
func TestPickExcludingAdvances(t *testing.T) {
	p := newStrategyPool(t, StrategyRoundRobin)
	tried := map[string]bool{}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		got := p.PickExcluding(tried)
		if got == nil {
			t.Fatalf("第 %d 次 pick 为 nil", i+1)
		}
		if seen[got.UID] {
			t.Fatalf("第 %d 次重复选中 %s", i+1, got.UID)
		}
		seen[got.UID] = true
		tried[got.UID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("轮换只覆盖 %d 个账号，应为 3", len(seen))
	}
}

// 全部试完后应允许回到起点（返回非 nil），而不是直接放弃。
func TestPickExcludingFallsBackAfterExhaust(t *testing.T) {
	p := newStrategyPool(t, StrategyCredits)
	tried := map[string]bool{"u1": true, "u2": true, "u3": true}
	got := p.PickExcluding(tried)
	if got == nil {
		t.Fatal("全部试完后应回退到可用账号，而不是 nil")
	}
}

// 负载均衡下 NotifyUsed 会推进游标。
func TestNotifyUsedAdvancesRoundRobinCursor(t *testing.T) {
	p := newStrategyPool(t, StrategyRoundRobin)
	p.NotifyUsed("u2") // 显式把游标推到 u2
	if got := p.Pick(); got == nil || got.UID != "u3" {
		t.Fatalf("pick=%v want u3（游标应已推进）", got)
	}
}
