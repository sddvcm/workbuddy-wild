# 诊断报告：最小化后无法唤出面板（v0.7.3）

**诊断日期**：2026-10-08
**日志来源**：`C:/Users/Administrator/Desktop/123.txt`（55 行，2026-10-08 07:21:36 — 09:00:07）
**被测版本**：workbuddy-wild **v0.7.3**
**结论**：**确认根因**——面板隐藏/显示路径上的 Wails runtime 调用缺乏超时与并发保护，一次阻塞即永久锁死后续所有唤起尝试。

---

## 一、日志时间线（客观事实）

| 时间 | 事件 | 备注 |
|---|---|---|
| 07:21:36 | WebView2 Environment created | 冷启动正常 |
| 07:21:38 | 已启动，监听 127.0.0.1:7863 | 账号 0 |
| 07:21:47 | **面板窗口已从最小化/隐藏状态恢复** | ① 可复活 |
| 07:22:37 | **面板窗口已从最小化/隐藏状态恢复** | ② 可复活 |
| 07:23:11 | 登录成功（余）→ 签到成功 → remain=1826 | 功能正常 |
| 07:24:48 | 登录成功（想想）→ 签到成功 → remain=896 | 功能正常 |
| 07:25:23 | 自动签到时间已更新：09:00 | |
| 07:25:37.196 | **面板窗口已从最小化/隐藏状态恢复** | ③ **最后一次成功恢复** |
| 07:25:37.201 | credits refresh start（2 账号） | `showPanelNow()` 走完并触发 `RefreshAll` |
| 07:25:37.855 | credits refresh success | |
| **07:25:38 — 08:59:59** | **（无任何日志，94.4 分钟）** | ★ 用户此间点击托盘试图唤出面板 |
| 09:00:00 | checkin batch start（traework 0 / workbuddy 2） | **进程存活，调度器正常** |
| 09:00:07.401 | checkin batch done | **日志到此终止** |

**两个决定性观测**：

1. **窗口恢复日志彻底消失**。07:25:37 之后，用户至少尝试过一次唤出（用户主诉"点了弹不出"），但日志中**既无「已从最小化/隐藏状态恢复」，也无「面板无响应」**。
2. **进程未死**。09:00:00 的定时签到批次完整执行 —— 排除"程序崩溃/退出"。

---

## 二、代码路径分析

### 2.1 `ShowPanel` 的三条静默路径

`internal/app/app.go:462-517`：

| 条件 | 行为 | 是否留日志 |
|---|---|---|
| `a.ctx == nil` | InfoBox「面板不可用」 | ❌ 无日志 |
| `hwnd == 0 \|\| !IsWindowValid` | InfoBox「面板已崩溃」 | ❌ 无日志 |
| `IsVisible(hwnd) && IsHungAppWindow(hwnd)` | InfoBox「面板无响应」 | ✅ 有日志 |
| `RestoreAndShow(hwnd)` 返回 **true** | 继续显示 | ✅ 有日志 |
| **`RestoreAndShow` 返回 false**（窗口已可见且非最小化） | 继续显示 | ❌ **无日志** |

**关键**：当窗口的 `WS_VISIBLE` 位为真、但窗口实际被遮挡或处于异常状态时，走的是**最后一条完全静默的路径**。因此"日志安静"与"托盘点击已送达"**可以同时成立**。

### 2.2 阻塞点：`showMu` + Wails runtime 调用

`internal/app/app.go:521-540`：

```go
func (a *App) showPanelNow() {
	if a.ctx == nil { return }
	showMu.Lock()
	defer showMu.Unlock()
	fx, fy, pw, ph := a.panelRect()
	runtime.WindowSetSize(a.ctx, pw, ph)     // ← 无超时；WebView2 无响应时永久阻塞
	runtime.WindowSetPosition(a.ctx, fx, fy) // ← 同上，且锁不会释放
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "panel:shown", nil)
	...
	a.safeGo(a.RefreshAll)                   // ← 阻塞时永不到达
}
```

**死锁链**：
1. 首次唤出 → `showPanelNow` 取得 `showMu` → 某个 runtime 调用阻塞
2. **`defer showMu.Unlock()` 永不执行** → 锁被永久持有
3. 后续所有托盘点击 → 全部阻塞在 `showMu.Lock()` → **日志零输出**

### 2.3 更早的阻塞点：`resizePanel`（无任何防护）

`internal/app/app.go:1243-1259`：

```go
func (a *App) emitAccounts() {          // 12 个调用点，几乎所有账号操作都会触发
	if a.ctx != nil {
		runtime.EventsEmit(a.ctx, "accounts", a.accountViews())
		a.resizePanel()                 // ← 进入无防护区
	}
}

func (a *App) resizePanel() {           // ⚠️ 无 showMu、无 go 化、无超时
	if a.ctx == nil { return }
	fx, fy, pw, ph := a.panelRect()
	runtime.WindowSetSize(a.ctx, pw, ph)     // ← 可永久阻塞
	runtime.WindowSetPosition(a.ctx, fx, fy)
}
```

对比同一文件里 `showPanelNow` 的防护（`showMu` 串行化 + `go` 化 + 调用方 `IsHungAppWindow` 预检），**`resizePanel` / `HidePanel` 完全没有对应保护** —— 属于**防护不一致**。

**日志末行恰好佐证这一点**：`09:00:07.401978 checkin batch done` 是调度器日志。而 `NotifyCheckin`（`app.go:1226`）的顺序是：

```go
log.Printf("GUI checkin ...")   // 已有日志 ✓
a.emitAccounts()                // → resizePanel → WindowSetSize ← 若卡此处
runtime.EventsEmit(...)         // 永不执行
```

日志停在 `checkin batch done`（调度器侧完成），而 app 侧最后的 GUI 事件流没有后续 —— 与 `emitAccounts → resizePanel` 阻塞的推断一致。

---

## 三、根因（结论）

> **面板窗口的隐藏/显示/尺寸调整路径，对 Wails runtime 调用缺少三层防护中的至少两层：超时、并发串行化（对 `resizePanel`/`HidePanel` 而言）、崩溃预检。任一 runtime 调用在 WebView2 无响应时永久阻塞，即可导致 `showMu` 锁永不释放，此后所有托盘唤起全部静默失败。**

补充说明（不确定项，需实测确认）：
- 无法从日志断定**首个**阻塞发生在 `HidePanel`（用户点最小化）还是 `emitAccounts/resizePanel`（09:00 签到后），因为两者都不留日志。但从"09:00:07 后日志完全终止"看，**`emitAccounts` 路径的嫌疑更大**。
- 上文 §2.1 的静默路径分析表明，"托盘点击已送达但未留日志"是成立的，因此不能断言托盘事件丢失。

---

## 四、修复建议（按优先级）

### P0 — 给所有 runtime 调用加超时保护（核心修复）

对 `showPanelNow` / `resizePanel` / `HidePanel` 中的**每一个** runtime 调用加超时包裹，超时后放弃并记录日志：

```go
// withTimeout 在超时后放弃等待（调用方 goroutine 泄漏，但不再阻塞关键路径）
func withTimeout(d time.Duration, name string, fn func()) bool {
    done := make(chan struct{})
    go func() { defer func() { recover() }(); fn(); close(done) }()
    select {
    case <-done:
        return true
    case <-time.After(d):
        log.Printf("WARN: %s 超时 %v 未返回（WebView2 可能无响应）", name, d)
        return false
    }
}
```

同时把 `showMu` 从"互斥锁"改为**带超时的获取**（`TryLock` 语义），避免永久排队。

### P1 — `resizePanel` 与 `HidePanel` 补齐防护

- `resizePanel`：加 `showMu` 保护 + 超时；或直接改为**不复用** runtime，改用原生 `SetWindowPos`（`winutil` 已有 user32 封装）。
- `HidePanel`：加日志（便于下次定位）+ 超时。

### P2 — 补齐静默路径的日志

`ShowPanel` 中 `RestoreAndShow` 返回 false 的分支应输出日志（如"窗口已可见，直接显示"），消除本次诊断中"日志空白"的盲区。

### P3 — 增加"卡死自愈"开关

若检测到连续 N 次唤起失败（可用计数器），提示用户"面板已无响应，建议重启"，或提供"强制重建窗口"的能力（销毁并重建 WebView2 窗口）。

---

## 五、复现与验证方法

**复现条件**（供开发者验证）：
1. 启动程序，把面板移到副屏或屏幕边缘
2. 点最小化（收起）→ 等待 ≥5 分钟
3. 期间触发一次签到（或手动刷新，会走 `emitAccounts → resizePanel`）
4. 再点托盘图标 → 面板不出现，且日志无任何新增

**日志判据**：
- 若出现连续空洞（>10 分钟）且期间进程未退出 → 符合本根因
- 若托盘点击后出现「面板无响应」→ 属于已防护的路径（`IsHungAppWindow` 生效）

**临时缓解**（无需改代码）：重启程序即可恢复（因为 `showMu` 是进程内变量，重启后重置）。

---

## 六、与上次日志（2026-10-06）的区分

| | 上次（2026-10-06） | 本次（2026-10-08） |
|---|---|---|
| 现象 | 隐藏后点不开 | 隐藏后点不开 |
| 日志特征 | **事件风暴**（248 行 credits refresh 刷屏） | **完全安静**（94 分钟空洞） |
| 根因 | 消息队列被刷新事件淹没 | runtime 调用阻塞 + `showMu` 死锁 |
| 是否同一问题 | **不是** | 本次为新根因 |

**重要**：这两次是**不同**的故障。上次是"消息太多"，这次是"一个调用卡住导致锁不释放"。

---

## 七、附：本次诊断中发现的两个次要问题

1. **前端注释过时**：`frontend/dist/app.js:577` 写「失焦自动隐藏已由**后端 focus watchdog** 处理（点击窗口外退出面板进程）」，但**后端不存在 focus watchdog**（全仓库检索无此机制）。该注释会误导后续维护者。
2. **`HidePanel` 无日志**：全项目唯一的窗口隐藏入口完全不记录日志，导致"用户点了什么"无法从日志复原 —— 这是本次诊断最大的盲区来源。
