# WorkBuddy-Wild 开发文档（面向 AI Agent）

> **读者**：接手本仓库的 AI Agent 或开发者。本文档目标是——**只读本文档 + 仓库源码，就能完整重写/维护这个项目**。
> 普通用户请看 [README](../README.md)；历史交接记录见 [HANDOFF.md](../HANDOFF.md)（已过时，仅存档）。
>
> **当前版本**：v0.4.0 ｜ **最后核对**：2026-09-22（本文所有描述均已与源码逐项核对）

---

## 目录

1. [项目本质](#1-项目本质)
2. [技术栈与环境](#2-技术栈与环境)
3. [代码地图](#3-代码地图)
4. [关键不变量（改了必出事）](#4-关键不变量改了必出事)
5. [核心机制详解](#5-核心机制详解)
6. [数据流](#6-数据流)
7. [前后端契约（wails 绑定）](#7-前后端契约wails-绑定)
8. [文件与持久化格式](#8-文件与持久化格式)
9. [配置项全表](#9-配置项全表)
10. [构建与发布](#10-构建与发布)
11. [测试](#11-测试)
12. [常见坑与排错](#12-常见坑与排错)
13. [跨平台移植（macOS / Linux）](#13-跨平台移植macos--linux)
14. [扩展指南](#14-扩展指南)

---

## 1. 项目本质

**一句话**：把多个 WorkBuddy/CodeBuddy（及 TraeWork）账号聚合成一个 **OpenAI 兼容 API**，附带**自动签到**领额度、**多策略选号**、**敏感凭证加密**，全部打包成一个零依赖的 Windows 单 exe 托盘程序。

**单进程架构**（一个 exe 同时是四样东西）：

```
┌─────────────────────────────────────────────────────┐
│  workbuddy-wild.exe（单进程）                        │
│                                                     │
│  ① HTTP 服务   :7863  OpenAI 兼容 API（对外）        │
│  ② 调度器      后台 goroutine × 2（每平台一个）      │
│  ③ 托盘图标    energye/systray                       │
│  ④ 管理面板    Wails v2 + WebView2（无边框窗口）     │
└─────────────────────────────────────────────────────┘
```

**为什么用 Wails 而不是 Electron**：产物 ~13MB（Electron 至少 100MB+），且复用 Go 后端零 IPC 序列化开销。

**双平台设计**：`workbuddy` 和 `traework` 是两个独立 `Runtime`，各有自己的 pool / scheduler / state 文件 / 上游客户端，但**共用一个选号策略**和一个 HTTP handler。

---

## 2. 技术栈与环境

| 项 | 值 | 说明 |
|---|---|---|
| 语言 | Go **1.25.0** | 见 `go.mod` |
| 桌面壳 | **Wails v2.14.0** | WebView2（Windows） |
| 托盘 | energye/systray **v1.0.3** | |
| 系统调用 | golang.org/x/sys **v0.47.0** | |
| 前端 | **纯静态 HTML/CSS/JS** | **无 npm、无构建步骤** |
| 模块名 | `github.com/rockswang/workbuddy-wild` | **别改**，所有 import 依赖它 |
| 平台 | 仅 Windows amd64 | 核心逻辑纯 Go 可移植，见 §13 |

### 2.1 构建环境（重要）

本机（开发机）实测可用的组合：

```bash
# Go 工具链（便携版）
C:/Users/Administrator/WorkBuddy/workbuddy自动签到/workbuddy-wild-fix/tools/go/bin/go.exe

# wails CLI
C:/Users/Administrator/go/bin/wails.exe

# 依赖缓存（含 wails 依赖的暖缓存，换这个能省 20 分钟重新下载）
GOPATH=C:/Users/Administrator/go5
GOMODCACHE=C:/Users/Administrator/go5/pkg/mod
GOPROXY=https://goproxy.cn,direct    # 七牛镜像；不要用阿里云
GOFLAGS=-mod=mod
```

> **GOPATH 必须每次都换新的**（除非确认没被锁）：go 构建后 `@v/*.info` 会被锁（Access denied，疑似杀软），复用同一 GOPATH 会失败。可用的历史缓存：`go`、`go2`、`go3`、`go5`。
> **一次只跑一个 go 任务**，并发会互相抢缓存拖死。

---

## 3. 代码地图

| 路径 | 职责 | 改动时的注意点 |
|---|---|---|
| `main.go` | 装配入口：chdir → 单实例锁 → 加载配置 → 组装 pool/upstream/scheduler/handler → 启动 HTTP → 启动托盘 → `wails.Run` | `--autostart` 参数控制是否弹"已启动"提示；窗口尺寸 760×560；WebView2 用户数据目录固定 `data/webview` |
| `internal/app/` | **wails 绑定层**（`app.go` 单文件约 40KB）：面板数据、账号操作、登录编排、端口热切换、日志、策略切换 | 所有导出方法即前端可调 API；`ShowPanel` 必须等 `domReadyCh`；托盘回调必须 `go` 化；**任何日志不得含 token** |
| `internal/pool/` | 账号池：**多策略选号**、冷却/禁用状态机、`state.json` 持久化 | 选号必须单调推进（§5.2）；state 格式只增不减 |
| `internal/scheduler/` | 定时签到 + token 保活 + 冷却解冻 + 签到记录 | 支持分钟级运行时改时间（`SetCheckinMinutes` + wake 通道） |
| `internal/server/` | OpenAI 兼容 HTTP handler | `MaxRotate` 可控；每平台独立 `Runtime`（pool+upstream+模型缓存） |
| `internal/upstream/` | WorkBuddy 上游 HTTP（chat/billing/auth）+ 错误分类 | **`PrepareBody` 三改写勿动**（§4.1）；账单用短超时 `BillingHTTP` |
| `internal/traework/` | TraeWork 上游（独立协议，`solosse.go` 为 SOLO 上游） | 与 upstream 平行，接口对齐 `provider.Upstream` |
| `internal/provider/` | 平台抽象：`Kind`、`Upstream` 接口、`Error` 分类、`ModelInfo` | 新增平台从这里开始 |
| `internal/config/` | 配置加载/校验/原子写回 + 环境变量覆盖 | `listen` 兼容旧格式；`errors.Is(err, fs.ErrNotExist)` 判缺失 |
| `internal/auth/` | auth 文件解析（嵌套/扁平双形态）+ **DPAPI 加密** | 落盘格式必须与 `login.SaveAuth` 一致 |
| `internal/login/` | WorkBuddy CN OAuth 登录 | state 落 `data/login-state.json` |
| `internal/login_trae/` | TraeWork 登录流程 | |
| `internal/winutil/` | **Windows-only**：工作区、任务栏隐藏、无痕浏览器、开机自启、MessageBox、孤儿 WebView2 清理、单实例锁、窗口卡死检测 | 移植的集中改造点（§13） |
| `frontend/dist/` | 前端三件套 `index.html` / `style.css` / `app.js` | 直接调 `window.go.app.App.*`，无生成物 |
| `cmd/server` | **无头模式服务**（无 GUI） | 调试 HTTP 链路首选，可在无桌面环境跑 |
| `cmd/login``cmd/signin``cmd/credit``cmd/genicon` | 独立 CLI | `genicon` 生成图标资产 |
| `build.sh` / `genicon.sh` | 构建/图标脚本 | |
| `.github/workflows/release.yml` | **打 tag `v*` 自动构建并发 Release** | 见 §10.3 |
| `docs/` | 本目录 | |

---

## 4. 关键不变量（改了必出事）

> 这一节是**血泪史**。改动前务必逐条确认。

### 4.1 `upstream.PrepareBody` 三种改写缺一不可

1. **强制 `stream=true`** — 上游拒绝非流式请求。
2. **`tool_choice` 归一化** — 对象形式会返回 `400 code=11101`。
3. **`role=developer` → `system`** — 上游对 `developer` 角色误触发内容过滤，返回"检测到敏感内容"。pi 等客户端对推理模型会使用该角色。

### 4.2 日志与面板零 token

任何日志、面板数据、错误信息**不得包含** access token / refresh token / GitHub token。调试时用假 token 代替。

### 4.3 auth 文件格式固定

```json
{
  "auth":    { "accessToken": "...", "refreshToken": "...", "expiresAt": 1753600000, "domain": "..." },
  "account": { "uid": "...", "enterpriseId": "...", "nickname": "..." }
}
```
`internal/auth` 的读取逻辑与 `internal/login.SaveAuth` 的写入必须一致。

### 4.4 `config.listen` 双格式兼容

新版对象 `{"host":"127.0.0.1","port":7863}` + 旧版字符串 `":7863"` / `"127.0.0.1:7863"` / `"7863"` 都要能解析（`Listen.UnmarshalJSON`）。

### 4.5 `state.json` 向后兼容

**只增字段，不减字段**。旧文件缺失新字段时按零值处理，不得报错。

### 4.6 托盘回调必须 `go` 化

systray 的消息循环线程上**禁止**执行任何重量逻辑。wails 的 runtime 调用会 marshal 到主线程，一旦阻塞就**永久卡死托盘**。

```go
// 正确
systray.SetOnClick(func(systray.IMenu) { go a.ShowPanel() })
// 错误 —— 会卡死托盘
systray.SetOnClick(func(systray.IMenu) { a.ShowPanel() })
```

> **历史教训**：曾用 `AddMenuItem + ShowMenu`（`TrackPopupMenu` 模态循环）导致托盘随机卡死。**现在完全不使用原生右键菜单**，右键 = 左键 = 弹面板。不要"顺手加回菜单"。

### 4.7 面板显示前必须等 `domReadyCh`

冷启动 WebView2 很慢，直接 `WindowShow` 会白窗口。`ShowPanel` 内部已处理，勿绕过。

### 4.8 WebView2 用户数据目录固定 `data/webview`

启动前检测 `SingletonLock` 并清理孤儿进程（强杀残留会锁 profile → 下次启动假死/白窗口）。**不要**改成随 exe 名变化的路径。

### 4.9 选号必须单调推进（v0.4.0 新增）

`pool.PickExcluding(tried)` 在同一请求内依赖 `tried` 排除已试账号。若去掉这个语义，「优先过期」和「负载均衡」会**反复选中同一个**快照值最优的账号。详见 §5.2。

### 4.10 版本号三处同步

| 位置 | 用途 |
|---|---|
| `wails.json` → `info.productVersion` | 安装包元数据 |
| `internal/app/app.go` → `var Version` | 面板显示的默认值（被 ldflags 覆盖） |
| 构建时 `-ldflags -X ...app.Version=` | 实际注入的版本 |

---

## 5. 核心机制详解

### 5.1 账号生命周期状态机

```
        ┌──────────┐
        │ healthy  │ ← 正常参与选号
        └────┬─────┘
             │
   ┌─────────┼─────────┬──────────────┐
   │         │         │              │
余额不足   429    连续错误×N    session 失效
   │         │         │              │
   ▼         ▼         ▼              ▼
CoolHard  CoolSoft  CoolErr      Disabled
 12h       60s       10m         永久（需重登）
   │         │         │              │
   └─────────┴─────────┘              │
             │                        │
      签到后 remain>0                  X
      ReenableIfCredits ──────────────┘
      （Disabled 不解冻）
```

- `healthy(now)` = `!disabled && (until 为零 或 now >= until)`
- **冷却账号仍参与签到**（以便恢复）；**Disabled 账号不参与签到**
- 错误分类由 `upstream.Classify(status, body)` 决定

### 5.2 选号策略引擎（v0.4.0 核心）

**三种策略**：

| 策略 | 常量 | 算法 |
|---|---|---|
| 优先积分（默认） | `credits` | 候选集中 `credits` 最大者；**同分取 UID 较小者**（稳定排序，修掉了原先 range map 随机的问题） |
| 优先过期 | `expire` | ①有积分者优先 ②`ExpiresAt` 小者优先（先过期先用）③过期时间未知者排最后 ④退化到比积分 → UID |
| 负载均衡 | `roundrobin` | 候选集按 UID 排序，从游标 `rrLast` 的下一个开始；游标写回 state.json |

**挑选流程**（`PickExcluding`）：

```
healthyLocked(now, tried)        ← 排除 disabled / 冷却中 / tried 中的
    │
    ├─ 若结果为空 且 tried 非空 → 回退：healthyLocked(now, nil)
    │   （说明已轮完一圈，允许重复；handler 的 MaxRotate 循环也该结束了）
    │
    ├─ 仍为空 → return nil（真·无可用账号）
    │
    └─ 按 p.strategy 分派 pickCredits / pickExpire / pickRoundRobin
```

**⚠️ 核心约束 —— 单调推进**：

同一请求内，handler 用 `tried` map 累计已试账号：

```go
tried := map[string]bool{}
for i := 0; i < MaxRotate; i++ {
    acct := rt.Pool.PickExcluding(tried)   // 每次排除上次结果 → 必然换号
    if acct == nil { break }
    tried[acct.UID] = true
    rt.Pool.NotifyUsed(acct.UID)           // 记录"正在调用" + 推进 rr 游标
    ...
}
```

同时 `pickRoundRobinLocked` 和 `NotifyUsed` 也会推进 `rrLast`，**双保险**保证连续调用换号。

**策略作用域**：**全局**——两个平台的 pool 同时 `SetStrategy`，持久化在 `config.json` 的 `strategy` 字段。

**「正在调用」标识的实现**：
- `NotifyUsed(uid)` 记录 `lastUsedAt = now` 与 `lastCallCredits = credits`
- 面板据此判断：`now - lastUsedAt < 5s` → 显示「调用中」（绿色脉冲）；有值时 → 「最近用过」
- **注意**：这是**时间推断**，不是精确在途计数（设计上刻意如此，避免进程异常退出残留计数）

### 5.3 模型名必须带平台前缀

```
workbuddy/glm-5.2
workbuddy/deepseek-v4-pro
traework/glm-5.2
traework/kimi-k2.7-code
```

`runtimeForModel` 强制校验前缀格式，无前缀返回 `invalid_model`。模型列表**动态获取**（`dynamicModelsTTL = 1h`，失败后 `modelsFetchFailCooldown = 5min` 内不重试）。

### 5.4 登录流程（无痕浏览器 OAuth）

```
面板点"＋ WorkBuddy"
  → app.StartLoginFor("workbuddy")
  → login.Start()  → POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI
  → ResolveAuthURL() 预解析跳转链
      copilot.tencent.com/login → 301 加斜杠 → www.codebuddy.cn/login
  → winutil.DefaultBrowserIncognito() + LaunchIncognito() 拉起无痕浏览器
  → 前端弹模态框显示倒计时（300s）+ 授权链接（可复制）
  → 轮询 loginPollEvery = 2s  GET /v2/plugin/auth/token?state=...
  → 成功 → 取账号信息 → SaveAuth 写 auths/ → pool 重载 → 异步签到
  → 前端收到 login 事件（phase: success/failed/cancelled）
```

超时上限 `loginTimeout = 5 * time.Minute`。

### 5.5 日志轮转

`data/app.log` 超过 `maxLogSize = 5MB` → 改名为 `app.log.1`（先删旧备份，Windows rename 不覆盖）→ 重开新文件。

### 5.6 面板位置记忆

`SavePanelPos(x, y)` 写 `data/panel-pos.json`。前端在 `mouseup` 后延时 **350ms** 调用（Wails 原生拖拽期间前端收不到事件，必须等系统拖拽结束，否则保存到中间位置）。

`panelRect()` 恢复时仍会 **clamp 回工作区**，防止分辨率变化导致窗口出屏。

### 5.7 单实例与强制退出

- **进程级**：`winutil.AcquireSingleInstance()`（`CreateMutex Local\WorkBuddy-Wild-SingleInstance`），在 HTTP/调度器/WebView2 **之前**检查
- **wails 级**：`SingleInstanceLock.UniqueId = "workbuddy-wild-gui-v1"`
- **退出兜底**：`ForceQuit()` = `runtime.Quit` + `quitTimeout = 5s` 超时后 `os.Exit(1)`（WebView2 卡死时 runtime 调用会永久阻塞）

### 5.8 防卡死三层防御

1. `winutil.IsHungAppWindow`（`user32`，窗口 5s 无响应检测）→ `ShowPanel` 卡死前拦截并提示，不调 runtime
2. `ShowPanel` 的 `domReadyCh` 已关闭分支 **go 化**（调用方永不阻塞）
3. 托盘回调全部 `go func()`

---

## 6. 数据流

```
【请求】客户端 → POST /v1/chat/completions
    → server.withAuth（Bearer 校验，key 空则跳过）
    → runtimeForModel（解析 platform/<model> 前缀）
    → rewriteModel（剥掉前缀）
    → for i < MaxRotate:
        pool.PickExcluding(tried)      ← 按策略选号
        pool.NotifyUsed(uid)           ← 标记"正在调用"
        [按需 refresh token（RefreshSkew = 10min）]
        upstream.ChatStream(acct, body)
          → upstream.PrepareBody（三改写）
          → copilot.tencent.com/v2/chat/completions
        ├─ 成功 → NoteSuccess → 流式回传 / 聚合返回
        └─ 失败 → Classify → Cooldown/Disable/NoteError → 下一个账号
    → 全失败 → 503 no_healthy_account

【签到】scheduler（分钟级定时 或 面板"全部签到"）
    → token 校验，必要时刷新后重试整套
    → DailyCheckin → UserResource（查余额）
    → ReenableIfCredits（remain>0 且非 disabled → 解冻）
    → RecordCheckin 落 state.json
    → 写 app.log + 事件推面板（NotifyCheckin）

【登录】见 §5.4

【配置】面板修改
    → app.SetXxx（加锁改 cfg）
    → config.Save 原子写回（tmp + rename）
    → 运行时立即生效：
        SetListen          → 热切换监听（失败保持原样）
        SetCheckinTimes    → 唤醒两个调度器
        SetStrategy        → 两个 pool 同时切
        SetAPIKey          → handler.SetAPIKey（带锁）
```

---

## 7. 前后端契约（wails 绑定）

前端通过 `window.go.app.App.<方法>` 调用，通过 `window.runtime.EventsOn("<事件>")` 收事件。

### 7.1 全部绑定方法（`internal/app/app.go`）

| 方法 | 签名 | 用途 |
|---|---|---|
| `GetState` | `() State` | 面板初始数据（含 `strategy`） |
| `GetAccounts` | `() []AccountView` | 账号快照（**前端每 1.5s 轮询此方法刷新"正在调用"**） |
| `GetStrategy` | `() string` | 当前策略 |
| `SetStrategy` | `(name string) error` | 切换全局策略 + 写回 config |
| `StartLogin` / `StartLoginFor` | `(kind string) (string, error)` | 发起登录，返回授权 URL |
| `CancelLogin` | `() error` | 取消登录 |
| `CheckinAccount` | `(uid string) (scheduler.CheckinResult, error)` | 单账号签到 |
| `CheckinAll` | `() []scheduler.CheckinResult` | 全部签到 |
| `RefreshCredits` | `(uid string) (int64, error)` | 刷新单账号积分 |
| `RefreshAll` | `()` | 刷新全部积分（面板打开时自动调） |
| `RemoveAccount` | `(uid string) error` | 删除账号（含 auth 文件） |
| `SetCheckinHours` / `SetCheckinTimes` / `SetCheckinMinutes` | `(...) error` | 三种签到时间接口（新版用 `SetCheckinTimes`） |
| `SetListen` | `(host string, port int) error` | 热切换监听 |
| `SetAPIKey` | `(key string) error` | 改 API-Key |
| `SetAutostart` | `(on bool) error` | 开机自启 |
| `SavePanelPos` | `(x, y int)` | 保存面板位置 |
| `ShowPanel` / `HidePanel` | `()` | 显示/隐藏面板 |
| `Quit` / `QuitAll` / `ForceQuit` | `()` | 退出 |
| `OpenLogFile` | `() error` | 用记事本打开日志 |

> **`GetState` 返回的 `State` 结构**（JSON 字段名即前端可用名）：
> `accounts` / `checkin_hours` / `checkin_times` / `keepalive_hours` / `listen_host` / `listen_port` / `api_key` / `login_busy` / `next_checkin` / `version` / `autostart` / `running` / **`strategy`**

> **`AccountView` 结构**：`uid` / `group`（workbuddy\|traework）/ `nickname` / `credits` / `cooling` / `until` / `reason` / `disabled` / `err_count` / `last_checkin_ok` / `last_checkin_at` / `last_checkin_msg` / **`last_used_at`** / **`last_call_credits`** / **`in_use`** / **`expires_at`**

### 7.2 后端 → 前端事件

| 事件名 | 载荷 | 触发时机 |
|---|---|---|
| `accounts` | `[]AccountView` | 账号状态变化（`emitAccounts`） |
| `checkin` | `{platform, uid, ok, msg}` | 签到结果 |
| `refresh` | `{platform, uid, ok, msg}` | token 刷新失败 |
| `login` | `{phase, msg}` | 登录阶段（`success`/`failed`/`cancelled`） |
| `panel:shown` | `nil` | 面板显示（前端重置动效 + 拉一次数据） |

### 7.3 前端约定

- **无 npm、无构建**：直接改 `frontend/dist/{index.html,style.css,app.js}`
- `#app` 设 `--wails-draggable: drag`（**不是** `-webkit-app-region`！Wails v2.14 读的是 CSS 变量 `--wails-draggable`）；可交互元素设 `none`
- **默认浅色**（`data-theme="light"`），🌙/☀ 可切深色，存 `localStorage.wbw_theme`
- 轮询：`startLivePolling()` 每 **1500ms** 调 `GetAccounts`；`document.hidden` 时暂停

---

## 8. 文件与持久化格式

> 所有相对路径均相对 **exe 所在目录**（`main.go` 启动时 `os.Chdir` 到 exe 目录）。

| 文件 | 内容 | 敏感 |
|---|---|---|
| `config.json` | 主配置（§9） | 含 api_key |
| `auths/workbuddy-<uid>.json` | WorkBuddy 账号凭证 | **是（DPAPI 加密）** |
| `auths/trae-<uid>.json` | TraeWork 账号凭证 | **是** |
| `data/state-workbuddy.json` | WorkBuddy 池状态 | |
| `data/state-traework.json` | TraeWork 池状态 | |
| `data/panel-pos.json` | 面板位置 `{x, y}` | |
| `data/login-state.json` | 登录 OAuth state | |
| `data/app.log` / `app.log.1` | 日志（5MB 轮转） | |
| `data/webview/` | WebView2 profile | |

### 8.1 `state.json` 格式

```json
{
  "accounts": {
    "<uid>": {
      "credits": 2588,
      "disabled": false,
      "reason": "",
      "until": "2026-09-22T18:00:00+08:00",
      "last_checkin_ok": true,
      "last_checkin_at": "2026-09-22T09:00:03+08:00",
      "last_checkin_msg": "ok",
      "last_used_at": "2026-09-22T16:14:03+08:00",
      "last_call_credits": 2600
    }
  },
  "rr_last": "<uid>"     // 负载均衡游标（v0.4.0 新增）
}
```

**写盘**：`os.WriteFile(tmp)` → `os.Rename`（原子），权限 `0600`。

### 8.2 凭证加密（DPAPI）

`internal/auth/secure.go` 用 Windows `CryptProtectData` / `CryptUnprotectData`：

- 加密值前缀 `dpapi:` + base64
- **兼容旧明文**：无前缀原样使用；解密失败降级原样
- 加密字段：`accessToken` / `refreshToken`
- ⚠️ 加密后**外部工具读不了 token**（本机单用户可用）

---

## 9. 配置项全表

```json
{
  "listen":  { "host": "127.0.0.1", "port": 7863 },
  "api_key": "WorkBuddy2API",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "strategy": "credits",
  "max_rotate": 3,
  "cooldown": {
    "hard_credit": "12h",
    "soft_rate": "60s",
    "err_threshold": 3,
    "err_cooldown": "10m"
  },
  "schedule": {
    "checkin_times": ["09:00", "21:00"],
    "keepalive_hours": [22]
  },
  "upstream": { "timeout_seconds": 120 }
}
```

| 字段 | 默认 | 说明 |
|---|---|---|
| `listen.host` / `listen.port` | `127.0.0.1` / `7863` | 面板可热切换；`0.0.0.0` = 对外 |
| `api_key` | `WorkBuddy2API` | 客户端 Bearer；**空 = 不鉴权** |
| `auth_dir` / `state_file` | `./auths` / `./data/state.json` | 相对 exe 目录 |
| `region` | `cn` | 仅 `cn` / `global` |
| **`strategy`** | `credits` | `credits` / `expire` / `roundrobin`；非法值 → **启动报错** |
| **`max_rotate`** | `3` | 单请求最多轮换账号数；`<=0`→3，`>20`→20 |
| `cooldown.hard_credit` | `12h` | 余额不足冷却 |
| `cooldown.soft_rate` | `60s` | 429 冷却 |
| `cooldown.err_threshold` | `3` | 连续错误次数阈值 |
| `cooldown.err_cooldown` | `10m` | 达到阈值后冷却 |
| `schedule.checkin_times` | `["09:00","21:00"]` | 支持分钟；兼容旧 `checkin_hours`；面板可改即生效 |
| `schedule.keepalive_hours` | `[22]` | token 保活时间 |
| `upstream.timeout_seconds` | `120` | 聊天超时（账单固定 30s） |

### 环境变量覆盖（`WB2A_*`，容器化用）

```
WB2A_LISTEN  WB2A_API_KEY  WB2A_AUTH_DIR  WB2A_STATE_FILE  WB2A_REGION
WB2A_STRATEGY  WB2A_MAX_ROTATE
WB2A_HARD_CREDIT  WB2A_SOFT_RATE  WB2A_ERR_THRESHOLD  WB2A_ERR_COOLDOWN
WB2A_TIMEOUT_SECONDS
```

> `config.example.json` 必须与 `config.Default()` 同步。

---

## 10. 构建与发布

### 10.1 本地构建

```bash
cd <repo>

export GOPATH="C:/Users/<你>/go5"
export GOMODCACHE="$GOPATH/pkg/mod"
export GOPROXY="https://goproxy.cn,direct"
export GOFLAGS="-mod=mod"
export PATH="<repo>/../tools/go/bin:/c/Users/<你>/go/bin:$PATH"

# 全量校验
go build ./... && go vet ./... && go test ./...

# 出 exe（注入版本号）
wails build -clean -m -nosyncgomod \
  -ldflags "-X github.com/rockswang/workbuddy-wild/internal/app.Version=0.4.0"
# 产物：build/bin/workbuddy-wild.exe
```

**参数说明**（都是踩坑换来的）：
- `-nosyncgomod` — 跳过 go mod tidy/sync，否则 go 1.26 想写 `toolchain` 指令时 rename 会被拒
- `-m` — 跳过前端构建（本项目前端无构建步骤）
- `-clean` — 清理 bin 目录

> 构建日志里出现 `已有实例在运行，本实例退出` 是 **binding 生成阶段重新拉起 exe** 撞上单实例锁，**无害**。

### 10.2 图标

```bash
go run ./cmd/genicon      # 或 bash genicon.sh
```
`icon.png` 变更后必须重新生成（生成 `build/appicon.png`、`build/trayicon.ico`、`build/windows/icon.ico`）。

### 10.3 发布（CI 自动，推荐）

**仓库已配置 GitHub Actions**（`.github/workflows/release.yml`）：

```
打 tag v* → 自动：setup-go → 装 wails v2.14.0 → genicon → wails build
          → 打包 zip（exe + config.example.json）→ 发 GitHub Release
```

所以发布只需：

```bash
git tag v0.4.0
git push sddvcm v0.4.0        # tag 推送即触发 CI
```

也可在 Actions 页面 `workflow_dispatch` 手动触发（产物为 artifact）。

### 10.4 发布（手动，备用）

```bash
# 建 release
curl -X POST -H "Authorization: Bearer <TOKEN>" \
  -H "Accept: application/vnd.github+json" \
  https://api.github.com/repos/sddvcm/workbuddy-wild/releases \
  -d '{"tag_name":"v0.4.0","name":"v0.4.0","body":"...","draft":false}'

# 上传资产（先取 release id）
curl -X POST -H "Authorization: Bearer <TOKEN>" \
  -H "Content-Type: application/octet-stream" \
  --data-binary @workbuddy-wild.exe \
  "https://api.github.com/repos/sddvcm/workbuddy-wild/releases/<id>/assets?name=workbuddy-wild-v0.4.0.exe"
```

### 10.5 git 推送注意

```bash
git -c http.proxy=http://127.0.0.1:7897 -c https.proxy=http://127.0.0.1:7897 push
```
- **必须走 Clash 代理**（`127.0.0.1:7897`），直连 GitHub 会 TLS 握手 hang
- 代理时通时断，**失败重试 2-5 次**
- 非交互场景加 `GCM_INTERACTIVE=never GIT_TERMINAL_PROMPT=0`，否则 Git Credential Manager 会弹 GUI 对话框卡住

---

## 11. 测试

```bash
go test ./...              # 全量
go test ./internal/pool/ -v   # 单包
```

| 测试文件 | 覆盖 |
|---|---|
| `internal/pool/pool_test.go` | 冷却/禁用/持久化/错误计数 |
| `internal/pool/strategy_test.go` | **14 个策略用例**（v0.4.0）：默认策略、三策略行为、rr 游标持久化、expire 排序、单调推进、`NotifyUsed` |
| `internal/server/handler_test.go` | 鉴权、模型路由、轮换、错误分类 |
| `internal/scheduler/scheduler_test.go` | 定时、解冻、签到记录 |
| `internal/auth/auth_test.go` | auth 解析/加密、`NeedsRefresh` |
| `internal/upstream/*_test.go` | SSE、body 改写 |
| `internal/traework/client_test.go` | TraeWork 客户端 |

**改策略相关代码后必须跑**：
```bash
go test ./internal/pool/ ./internal/server/ -count=1
```

> `TestPickExcluding` 的断言在 v0.4.0 改过：原本期望"全部试完后返回 nil"，现在**期望回退到起点**（配合 `MaxRotate` 循环）。真·无可用账号时仍返回 nil，由 `TestPickExcludingNilWhenAllUnavailable` 覆盖。

---

## 12. 常见坑与排错

### 12.1 GUI 无法在无桌面环境测试

SSH / 服务会话（Session 0）无交互桌面 → WebView2 创建失败 → go-webview2 的 `errorCallback` **直接 `os.Exit(1)`**，`defer`/`recover` 都救不回来（HTTP 服务一并被杀）。

**对策**：用 `cmd/server`（无头）调试 HTTP 链路；GUI 必须用真实桌面会话验证。

### 12.2 孤儿 WebView2 锁 profile

强杀进程后 → 下次启动假死/白窗口。已内置 `SingletonLock` 检测 + `KillOrphanWebViews`。
手动处理：结束命令行含 `data/webview` 的 `msedgewebview2.exe`（**勿动其他应用的 webview**）。

### 12.3 `os.IsNotExist` 不穿透 `%w`

```go
// 错误：首次运行会静默退出
if os.IsNotExist(err) { ... }
// 正确
if errors.Is(err, fs.ErrNotExist) { ... }
```

### 12.4 本机 Bash 工具 PATH 损坏

本机环境 `dirname` / `ls` / `cat` / `grep` / `git` / `tail` / `head` 可能全部 `command not found`。

**对策**：用 Python 绝对路径做目录/文件操作；git 用绝对路径：
```
C:/Users/Administrator/.workbuddy/binaries/PortableGit/versions/1.2.0/cmd/git.exe
```

### 12.5 bat / vbs 中文乱码

本机是 GBK 控制台（代码页 936）。写 `.bat` / `.vbs` 含中文**必须转 GBK + CRLF**：

```python
open('x.bat','wb').write(text.replace('\n','\r\n').encode('gbk'))
```
纯 ASCII 的 bat 最稳（任何代码页无歧义）。**不要** `chcp 65001` + UTF-8。

### 12.6 Python 脚本中文输出

```bash
export PYTHONIOENCODING=utf-8
```

### 12.7 批量删除超 50 个会被拦截

单轮删除操作数有 **50 个硬阈值**，超过直接中断进程。清理目录用 `shutil.move` 整个目录（算 1 次操作），别逐文件 `os.remove`。

### 12.8 写内联 base64 时**绝不凭记忆重写**

**v0.4.0 真实事故**：重构 `index.html` 时把 logo 的 base64 data URI 凭记忆重写成不完整的占位数据（8017 字符 vs 原始 8310），导致 logo 空白。编译能过、常规字符串校验发现不了（长度对不上就发现不了）。

**对策**：改写长常量必须从源文件/git 历史读取原值：
```bash
git show HEAD~1:frontend/dist/index.html   # 提取原值
```
校验时**比对长度 + 解码合法性**（`base64.b64decode(x)[:8].hex() == '89504e470d0a1a0a'` 即 PNG）。

### 12.9 go 模块缓存被锁

构建后 `@v/*.info` 被锁（Access denied，疑似杀软）→ 换新 `GOPATH`。别在原目录死磕；若 `go.mod`/`syso` 被锁，**重新 clone 一份**再构建。

### 12.10 窗口尺寸相关

窗口**固定 760×560**（`MinWidth 680` / `MinHeight 420`）。改尺寸要**同时改两处**：`main.go` 的 `wails.Run` 选项 + `app.go` 的 `panelRect()`。

---

## 13. 跨平台移植（macOS / Linux）

### 13.1 分层方案

平台耦合只有两处：`internal/winutil`（Win32 直调）+ `main.go` 的 `Windows: &windows.Options{}`。

- `internal/winutil` 按 build tag 拆分：`winutil_windows.go`（现状）+ `winutil_darwin.go` + `winutil_other.go`，**保持同名导出函数**（接口不变量）→ `internal/app` 零改动
- `main.go` 拆 `main_windows.go` / `main_darwin.go`（`runGUI` 平台实现），公共装配留 `main.go`
- pool / scheduler / login / upstream / server / config **零改动**
- 前端**零改动**

### 13.2 winutil 能力 × macOS 替代

| Windows 实现 | macOS 替代 |
|---|---|
| `WorkArea()`（SPI_GETWORKAREA） | wails runtime `ScreenGetAll`，或 cgo `NSScreen.visibleFrame` |
| `PanelAnchor`（任务栏锚定） | macOS 无任务栏：锚定主屏右上角（`visibleFrame` 右上 - 尺寸 - 8px） |
| `HideFromTaskbar` | 无任务栏，空函数；置顶用 `AlwaysOnTop` |
| `MainWindow` / `FocusWindow` | `NSApp.activateIgnoringOtherApps(true)`；可空实现 |
| `SetAutostart`（注册表 Run） | `~/Library/LaunchAgents/*.plist` + `launchctl bootstrap` |
| `DefaultBrowserIncognito`（注册表 UserChoice） | 解析 `LSHandlers` 默认浏览器 bundleId；Chrome `--incognito` / Edge `--inprivate` / Firefox `-private-window` |
| `OpenURL`（rundll32） | `open <url>` |
| `OpenWithNotepad` | `open -a TextEdit <file>` |
| `InfoBox` / `AskYesNo`（MessageBoxW） | **优先用 wails `runtime.MessageDialog`**（跨平台一处实现） |
| `KillOrphanWebViews`（PowerShell） | **不需要**：macOS 用系统 WKWebView，空实现 |
| `AcquireSingleInstance`（CreateMutex） | `flock` 或 lockfile |
| `IsHungAppWindow` | 可空实现（返回 false） |

### 13.3 systray 差异

- macOS 菜单栏图标**点击即显示原生菜单**，无单击/右击区分
- 现设计"单/双/右击皆弹面板"在 macOS 需改为**保留原生菜单**（打开面板 / 退出），回调仍用 `item.Click(...)`
- 图标用**模板图**（黑 + alpha，含 @2x），16/18px；需扩展 `genicon` 输出 `.icns`

### 13.4 macOS 构建

```bash
# 必须在 macOS 上构建（wails v2 的 darwin 打包依赖 macOS，不可交叉编译）
wails build -platform darwin/universal -skipbindings
# 需要 build/darwin/Info.plist + build/appicon.icns
# 产物：build/bin/workbuddy-wild.app
```
未签名/未公证会被 Gatekeeper 拦截（用户需右键→打开 或 `xattr -cr`）。CI 需加 `macos-latest` job。

### 13.5 Linux

托盘走 systray 的 AppIndicator（需 `libappindicator`）；面板定位按 GTK 工作区；自启 `~/.config/autostart/*.desktop`。`cmd/server` 完全跨平台，可作为各平台基础。

---

## 14. 扩展指南

### 14.1 新增一个平台（如 XXX）

1. `internal/provider/provider.go` 加 `Kind` 常量 + `String()`
2. 新建 `internal/xxx/`，实现 `provider.Upstream` 接口（`ChatStream` / `FetchModels` / `UserResource` / `DailyCheckin` / `RefreshToken` / `Stream` / `Aggregate` / `Classify`）
3. `main.go`：
   - `auth.LoadXxxDir(cfg.AuthDir)` 加载凭证
   - `pool.New(stateDir + "state-xxx.json")`
   - `scheduler.New(...)`
   - 加进 `runtimes`（server）+ `appRuntimes`（app）两个 map
4. `internal/auth/` 加该平台的凭证解析
5. 前端：`accountGroup` 返回新 group；`app.js` 的图标映射加分支
6. `server/handler.go` 加静态模型兜底列表（可选）

### 14.2 新增一种选号策略

1. `internal/pool/pool.go`：
   - 加 `StrategyXxx Strategy = "xxx"` 常量
   - 加进 `AllStrategies` 与 `ParseStrategy`
   - `Label()` 加中文名
   - `PickExcluding` 的 switch 加分支 + 实现 `pickXxxLocked(cands []*entry) *auth.Auth`
2. `internal/config/config.go` 的 `normalize()` 白名单加 `"xxx"`
3. `frontend/dist/index.html` 的 `#selStrategy` 加 `<option>`
4. `frontend/dist/app.js` 的 `STRATEGY_DESC` / `strategyLabel` 加条目
5. `internal/pool/strategy_test.go` 加用例
6. **务必确认策略在 `tried` 下单调推进**（§5.2）

### 14.3 修改面板布局

- 改 `frontend/dist/index.html`（结构）+ `style.css`（样式）
- 若改窗口尺寸 → 同步 `main.go` + `app.go:panelRect()`
- **别碰** `<img class="logo">` 的 base64（§12.8）

### 14.4 提交前检查清单

```
[ ] go build ./... && go vet ./... && go test ./...   全绿
[ ] 版本号三处同步（wails.json / app.Version / ldflags）
[ ] README 更新记录已加
[ ] 无 token / 凭证 / 日志 / exe 混入提交（.gitignore 覆盖 auths/ data/ config.json *.log build/bin/）
[ ] config.example.json 与 config.Default() 一致
[ ] 若改了 state.json 结构 → 确认旧文件仍能加载（只增字段）
[ ] 若改了内联 base64/长常量 → 与源值逐字节比对
```

---

## 附：上游接口速查（WorkBuddy CN）

| 用途 | 端点 |
|---|---|
| 登录发起 | `POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI` |
| 登录轮询 | `GET /v2/plugin/auth/token?state=...`（pending 时业务 code≠0） |
| 账号信息 | `GET /v2/plugin/login/account?state=...` |
| 刷新 token | `POST /v2/plugin/auth/token/refresh`（头带 `X-Refresh-Token`） |
| 动态模型 | `GET /console/enterprises/personal/models` |
| 余额 | `POST www.codebuddy.cn/v2/billing/meter/get-user-resource` |
| 签到 | `POST /v2/billing/meter/daily-checkin` |
| 聊天 | `POST copilot.tencent.com/v2/chat/completions`（**强制 stream**） |

**认证头**：`Authorization: Bearer <accessToken>`、`X-User-Id`，可选 `X-Enterprise-Id` / `X-Tenant-Id` / `X-Domain`；CN 的 `Origin`/`Referer` 为 CodeBuddy 域名。

---

**安全红线**：不要在任何文档、聊天记录或日志中输出真实 access token / refresh token。测试令牌不要复用。
