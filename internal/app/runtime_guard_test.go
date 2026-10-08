package app

import (
	"sync"
	"testing"
	"time"
)

// TestRuntimeCallTimeoutOnBlockedCall 验证：当 runtime 调用永久阻塞时，
// runtimeCall 必须在上限内返回 false，而不是一起卡死。
//
// 背景：2026-10-08 事故 —— WebView2 无响应导致 runtime 调用永久阻塞，
// 原实现持 showMu 阻塞，锁永不释放，后续所有托盘唤起静默失败。
func TestRuntimeCallTimeoutOnBlockedCall(t *testing.T) {
	block := make(chan struct{}) // 永不关闭 → 模拟永久阻塞
	start := time.Now()
	ok := runtimeCall("blocked-test", func() { <-block })
	elapsed := time.Since(start)

	if ok {
		t.Fatal("被阻塞的调用不应返回 true")
	}
	if elapsed > runtimeCallTimeout+2*time.Second {
		t.Fatalf("超时保护失效：耗时 %v，期望约 %v", elapsed, runtimeCallTimeout)
	}
}

// TestRuntimeCallFastPath 验证正常（快速返回）的调用不受影响。
func TestRuntimeCallFastPath(t *testing.T) {
	executed := false
	ok := runtimeCall("fast-test", func() { executed = true })
	if !ok {
		t.Fatal("正常调用应返回 true")
	}
	if !executed {
		t.Fatal("闭包应被执行")
	}
}

// TestRuntimeCallPanicIsolated 验证闭包 panic 不会导致 runtimeCall 挂死。
func TestRuntimeCallPanicIsolated(t *testing.T) {
	ok := runtimeCall("panic-test", func() { panic("boom") })
	if !ok {
		t.Fatal("panic 的调用应已返回（被 recover），返回 true")
	}
}

// TestTryLockTimeoutOnHeldLock 验证：锁被占用时，tryLockTimeout 会超时返回 false，
// 而不是永久排队 —— 这是修复 showMu 死锁的核心机制。
func TestTryLockTimeoutOnHeldLock(t *testing.T) {
	var mu sync.Mutex
	mu.Lock() // 模拟被僵死调用持有
	defer mu.Unlock()

	start := time.Now()
	got := tryLockTimeout(&mu, 200*time.Millisecond)
	elapsed := time.Since(start)

	if got {
		t.Fatal("锁被持有时不应获取成功")
	}
	if elapsed < 150*time.Millisecond {
		t.Fatalf("过早放弃：耗时 %v", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("超时上限失效：耗时 %v", elapsed)
	}
}

// TestTryLockTimeoutAcquiresFreeLock 验证空闲锁能被正常获取。
func TestTryLockTimeoutAcquiresFreeLock(t *testing.T) {
	var mu sync.Mutex
	if !tryLockTimeout(&mu, time.Second) {
		t.Fatal("空闲锁应能获取")
	}
	mu.Unlock()
}

// TestTryLockTimeoutWaitsForRelease 验证锁在等待期内被释放时能成功获取。
func TestTryLockTimeoutWaitsForRelease(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	go func() {
		time.Sleep(100 * time.Millisecond)
		mu.Unlock()
	}()
	if !tryLockTimeout(&mu, time.Second) {
		t.Fatal("锁在等待期内释放后应能获取成功")
	}
	mu.Unlock()
}

// TestShowMuNotHeldAfterBlockedShow 回归测试：模拟 showPanelNow 中的核心场景 ——
// 即使某个 runtime 调用卡死，showMu 也必须在有限时间内被释放，
// 保证后续唤起不会被永久阻塞。
func TestShowMuNotHeldAfterBlockedShow(t *testing.T) {
	var mu sync.Mutex
	block := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		if !tryLockTimeout(&mu, time.Second) {
			t.Error("首次应能取锁")
			return
		}
		defer mu.Unlock()
		runtimeCall("simulated-WindowSetSize", func() { <-block }) // 模拟卡死的 runtime 调用
	}()

	// 等待第一个 goroutine 走完（含超时）
	select {
	case <-done:
	case <-time.After(runtimeCallTimeout + 3*time.Second):
		t.Fatal("持有 showMu 的 goroutine 未在预期时间内退出")
	}

	// 关键断言：锁已释放，后续唤起可立即获取
	if !tryLockTimeout(&mu, 500*time.Millisecond) {
		t.Fatal("showMu 未被释放 —— 死锁未修复！")
	}
	mu.Unlock()
}
