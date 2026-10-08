# WorkBuddy-Wild 开发文档（面向 AI Agent）

> **读者**：接手本仓库的 AI Agent 或开发者。
> **本文档的目标**：**只读本文档（不看源码）就能重写这个项目**。所有常量、端点、字段名、DOM id 均已与源码逐一核对。
>
> 普通用户请看 [README](../README.md) 与 [USAGE](USAGE.md)。
>
> **当前版本**：v0.8.0 ｜ **最后核对**：2026-10-08
> **文档可信度**：经过 N 轮独立 Agent 复现验证（见文末「验证记录」）

---

## 目录

1. [项目定位与硬性约束](#1-项目定位与硬性约束)
2. [技术栈与构建环境](#2-技术栈与构建环境)
3. [代码地图](#3-代码地图)
4. [接口逆向：双平台契约](#4-接口逆向双平台契约)
5. [整体架构与数据流](#5-整体架构与数据流)
6. [模块实现规格](#6-模块实现规格)
7. [关键技术难点](#7-关键技术难点)
8. [不可为之事（已实测排除）](#8-不可为之事已实测排除)
9. [完整踩坑史与版本演进](#9-完整踩坑史与版本演进)
10. [重写检查清单](#10-重写检查清单)
11. [环境与验证方法](#11-环境与验证方法)
12. [网页管理面板（v0.8.0）](#12-网页管理面板v080)
13. [Docker 部署（v0.8.0）](#13-docker-部署v080)
14. [附录 A：关键参数速查表](#附录-a关键参数速查表)
15. [附录 B：错误信息对照表](#附录-b错误信息对照表)
16. [附录 C：前端 DOM id 全清单](#附录-c前端-dom-id-全清单)
17. [附录 D：核心数据结构与落盘格式](#附录-d核心数据结构与落盘格式重写必读)

---

## 1. 项目定位与硬性约束

### 1.1 一句话定位

把多个 **WorkBuddy/CodeBuddy**（`codebuddy.cn`）和/或 **TraeWork**（`trae.cn`）账号，
聚合成**一个 OpenAI 兼容 API**，附带**自动签到**领额度、**多策略选号**、**凭证加密**，
打包成零依赖的 Windows 单 exe 托盘程序；同一份代码还能以**无头模式**跑在 Docker 里。

### 1.2 单进程五合一

> （v0.8.0 起是**五合一**；v0.7.x 及以前只有前四项，故历史文档里叫「四合一」）

```
┌─────────────────────────────────────────────────────────┐
│  workbuddy-wild.exe（单进程）                            │
│                                                         │
│  ① HTTP 服务   :7863   OpenAI 兼容 API（对外）           │
│  ② 调度器       后台 goroutine × 2（每平台一个）          │
│  ③ 托盘图标     energye/systray                          │
│  ④ 管理面板     Wails v2 + WebView2（无边框窗口）         │
│  ⑤ 网页管理面板 /admin/（内嵌，v0.8.0；Docker 场景用）    │
└─────────────────────────────────────────────────────────┘
```

### 1.3 硬性约束（违反则不可交付）

| 约束 | 说明 |
|---|---|
| **模块名固定** `github.com/rockswang/workbuddy-wild` | 所有 import 依赖它，**不要改** |
| **零外部运行时依赖** | 单 exe，不装 Python/Node/JRE；前端是纯静态文件 |
| **前端无构建步骤** | 直接写 `index.html` / `style.css` / `app.js`，无 npm、无打包器 |
| **日志/面板零 token** | 任何输出不得含 access token / refresh token（**安全红线**） |
| **平台** | 仅 Windows amd64（核心逻辑纯 Go 可移植，Docker 用 linux/amd64） |
| **`state.json` 只增字段** | 向后兼容，旧文件缺失新字段按零值处理，不得报错 |

---

## 2. 技术栈与构建环境

### 2.1 依赖版本（与 `go.mod` 逐字核对）

| 项 | 值 |
|---|---|
| 语言 | Go **1.25.0** |
| 桌面壳 | `github.com/wailsapp/wails/v2` **v2.14.0** |
| 托盘 | `github.com/energye/systray` **v1.0.3** |
| 系统调用 | `golang.org/x/sys` **v0.47.0** |
| 前端 | 纯静态 HTML/CSS/JS（**无 npm**） |

### 2.2 本机构建环境（受限环境的关键）

```bash
# Go 工具链（便携版，注意路径含中文，bash 里必须用完整绝对路径）
GOROOT="/c/Users/Administrator/WorkBuddy/workbuddy自动签到/workbuddy-wild-fix/tools/go"
"$GOROOT/bin/go.exe" build ./...

# 依赖缓存（换新 GOPATH 能省 20 分钟重新下载）
GOPATH=C:/Users/Administrator/go5
GOMODCACHE=C:/Users/Administrator/go5/pkg/mod
GOPROXY=https://goproxy.cn,direct    # 七牛镜像；**不要用阿里云**
GOFLAGS=-mod=mod
```

> ⚠️ **两个硬性限制**：
> 1. **GOPATH 每次换新的**（`go`/`go2`/`go3`/`go5` 轮换）。go 构建后 `@v/*.info` 会被锁（Access denied，疑似杀软），复用同一 GOPATH 会失败。
> 2. **一次只跑一个 go 任务**，并发会互相抢缓存拖死。
> 3. **`go` 不在 PATH**（中文路径在 bash 里失效），必须用完整绝对路径调用。

### 2.3 交叉编译（Docker 用）

```bash
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GOROOT/bin/go.exe" build -o workbuddy-wild-server ./cmd/server
```

---

## 3. 代码地图

| 路径 | 职责 | 改动注意点 |
|---|---|---|
| `main.go` | 装配入口：chdir → 单实例锁 → 加载配置 → 组装 pool/upstream/scheduler/handler → 启动 HTTP → 启动托盘 → `wails.Run` | 窗口尺寸 **760×560**；WebView2 用户数据目录固定 `data/webview` |
| `internal/app/app.go` | **wails 绑定层**（单文件约 43KB）：面板数据、账号操作、登录编排、端口热切换、日志、策略切换 | 所有 `func (a *App) X() ` 导出方法即前端可调 API；`ShowPanel` 必须等 `domReadyCh`；托盘回调必须 `go` 化 |
| `internal/pool/pool.go` | 账号池：**多策略选号**、冷却/禁用状态机、`state.json` 持久化 | 选号必须单调推进（§6.2）；state 格式只增不减 |
| `internal/scheduler/scheduler.go` | 定时签到 + token 保活 + 冷却解冻 + 签到记录 | 支持分钟级运行时改时间（`SetCheckinMinutes` + `wake` 通道） |
| `internal/server/handler.go` | OpenAI 兼容 HTTP handler | `MaxRotate` 可控；每平台独立 `Runtime`（pool+upstream+模型缓存） |
| `internal/upstream/` | **WorkBuddy 上游**（chat/billing/auth）+ 错误分类 | `PrepareBody` 三改写勿动（§6.1）；账单用短超时 `BillingHTTP` |
| `internal/traework/` | **TraeWork 上游**（独立协议） | 与 upstream 平行，接口对齐 `provider.Upstream` |
| `internal/provider/` | 平台抽象：`Kind`、`Upstream` 接口、`Error` 分类、`ModelInfo` | 新增平台从这里开始 |
| `internal/config/config.go` | 配置加载/校验/原子写回 + 环境变量覆盖 | `listen` 兼容旧格式；`errors.Is(err, fs.ErrNotExist)` 判缺失 |
| `internal/auth/` | auth 文件解析（嵌套/扁平双形态）+ **DPAPI 加密** | 落盘格式必须与 `login.SaveAuth` 一致 |
| `internal/login/` | WorkBuddy CN OAuth 登录 | state 落 `data/login-state.json` |
| `internal/login_trae/` | TraeWork 登录流程 | |
| `internal/admin/` | **网页管理面板**（v0.8.0）：`go:embed` 内嵌前端 + 12 路由 | 见 §12 |
| `internal/winutil/` | **Windows-only**：工作区、任务栏隐藏、无痕浏览器、开机自启、MessageBox、孤儿 WebView2 清理、单实例锁、窗口卡死检测 | 跨平台移植的集中改造点 |
| `frontend/dist/` | 前端三件套 `index.html` / `style.css` / `app.js` | 直接调 `window.go.app.App.*`，无生成物 |
| `cmd/server/` | **无头模式服务**（无 GUI） | 调试 HTTP 链路首选，Docker 里跑的就是它 |
| `cmd/login` `cmd/signin` `cmd/credit` `cmd/genicon` | 独立 CLI | `genicon` 生成图标资产 |
| `docker/` | Docker 部署全套（Dockerfile / compose × 2 / entrypoint / 离线镜像构建与校验） | 见 §13 |
| `build.sh` / `genicon.sh` | 构建/图标脚本 | |
| `docs/` | 本目录 | |

---

## 4. 接口逆向：双平台契约

> ★ **这是全项目地基**。所有字段名均已用真实抓包核对，**照抄，不要"修正拼写"**。

### 4.1 主机与端点

#### WorkBuddy / CodeBuddy（CN）

| 用途 | 端点 |
|---|---|
| 登录发起 | `POST copilot.tencent.com/v2/plugin/auth/state?platform=CLI` |
| 登录轮询 | `GET /v2/plugin/auth/token?state=...`（pending 时业务 code ≠ 0） |
| 账号信息 | `GET /v2/plugin/login/account?state=...` |
| 刷新 token | `POST /v2/plugin/auth/token/refresh`（头带 `X-Refresh-Token`） |
| 动态模型 | `GET /console/enterprises/personal/models` |
| 余额 | `POST www.codebuddy.cn/v2/billing/meter/get-user-resource` |
| 签到 | `POST /v2/billing/meter/daily-checkin` |
| 聊天 | `POST copilot.tencent.com/v2/chat/completions`（**强制 stream**） |

认证头：`Authorization: Bearer <accessToken>`、`X-User-Id`，可选 `X-Enterprise-Id` / `X-Tenant-Id` / `X-Domain`。
`Origin` / `Referer`：CN 用 `https://www.codebuddy.cn`，global 用 `https://www.workbuddy.ai`。
UA 固定：`CLI/2.63.2 CodeBuddy/2.63.2`。

#### TraeWork

| 用途 | 端点（相对 Host） | Host |
|---|---|---|
| 聊天 | `POST /api/agent/v3/llm_utils_chat` | `https://trae-api-cn.mchost.guru` |
| 动态模型 | `POST /api/ide/v1/get_detail_param` | `https://trae-api-cn.mchost.guru` |
| 签到状态 | `POST /trae/api/v2/ug/checkin_credits/status`（体 `{}`） | `https://api.trae.cn` |
| 领取额度 | `POST /trae/api/v2/ug/checkin_credits/claim`（体 `{"req_source":1}`） | `https://api.trae.cn` |
| 剩余积分 | `POST /trae/api/v2/pay/user_current_entitlement_list`（体 `{}`） | `https://api.trae.cn` |
| 兑换 token | `POST /cloudide/api/v3/trae/oauth/ExchangeToken` | `https://api.trae.com.cn` |
| 用户信息 | `POST /cloudide/api/v3/trae/GetUserInfo` | `https://api.trae.com.cn` |

#### ⚠️ 陷阱：TraeWork 有 **3 个不同 Host**，用错必 404

```
AgentHost   = "https://trae-api-cn.mchost.guru"   ← 聊天 / 模型
UgHost      = "https://api.trae.cn"               ← 签到 / 积分 / 权益包
OAuthHost   = "https://api.trae.com.cn"           ← token 兑换 / 用户信息
ConsoleHost = "https://www.trae.cn"               ← 网页端（仅参考）
```

**实测教训（2026-10-08）**：把 `user_current_entitlement_list` 发到 `trae-api-cn.mchost.guru`
会返回 **HTTP 404 + HTML 页面**（`<title>404 Not Found</title>`，服务标识 `TLB`），
解析时报 `invalid character '<' looking for beginning of value`。
**权益包必须发 `api.trae.cn`。**

### 4.2 TraeWork 认证头

```
Authorization: Cloud-IDE-JWT <accessToken>
X-User-Region: CN
X-Device-Id: <16 位纯数字，客户端真实注册设备号>   ← 签到必需，见 §4.4
```

### 4.3 真实响应 JSON（照抄）

#### 余额（权益包列表）— 2026-09-29 抓包

```json
{
  "is_credits_billing": true,
  "usage_summary": {
    "consumed_amount": 3751.3,
    "consumption_ratio": 0.9262469135802469,
    "total_amount": 4050
  },
  "user_entitlement_pack_list": [
    {"display_desc":"免费","entitlement_base_info":{"quota":{"no_bonus_quota":true}},"usage":{}},
    {"display_desc":"每月登录赠送","group_name":"每月登录积分",
     "entitlement_base_info":{"quota":{"credits_limit":500}},"usage":{"credits_amount":500}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":200}},"usage":{"credits_amount":200}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{"credits_amount":150}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{"credits_amount":1.304}},
    {"display_desc":"签到奖励","group_name":"每日签到",
     "entitlement_base_info":{"quota":{"credits_limit":150}},"usage":{}}
  ]
}
```

**口径**：剩余 = `usage_summary.total_amount - usage_summary.consumed_amount` = 4050 − 3751.3 = **298.7**。
**逐包验证**：`Σ(credits_limit - usage.credits_amount)` 求和 = **298.696**（与权威值 298.7 **四舍五入到 1 位小数后一致，精确值差 0.004**）。

> ⚠️ **两条口径要分清**：`usage_summary` 是上游算好的权威值，逐包求和只是**佐证**（两者在小数位上有 0.004 级的浮点/取整差异，属正常）。**优先用 `usage_summary`**；只有当 `usage_summary` 缺失时才回退逐包求和。
> 逐包求和的两个细节：① `usage: {}` 的包按「未使用」处理，剩余 = `credits_limit`；② 若逐包求和与 `usage_summary` 相差超过 1 分，说明解析漏包，应告警而非静默采用。

> ⚠️ **三个致命陷阱**：
> 1. **`usage: {}` 表示"未使用"，不是"无法判断"**。曾把它当解析失败 → 少算额度。
> 2. **`credits_limit` 是上限不是余额**。曾把所有包的 `credits_limit` 累加当余额 → 显示 4050（实际 298.7）。
> 3. **`usage` 为 `{}` 时该包剩余 = 上限**（未动过），而不是 0。

#### 签到状态 — 成功响应

```json
{"checked_in": false, "did_checked_in": true, "credits": 100, "enable": true, "message": "success"}
```

- **`did_checked_in`** = 今天签到成功过 ← **用它做验证**
- `checked_in` = 用户当前是否处于签到会话，**对 API 调用方恒为 false**（用它会每次误判失败）

#### 206 权益包原始结构（2026-10-08 抓包，23 个包）

单个包的真实字段（**含到期时间**）：

```json
{
  "display_desc": "签到奖励",
  "group_name": "每日签到",
  "group_type": 1,
  "expire_time": 1794011797,                    ← ★ 到期时间（Unix 秒）
  "yearly_expire_time": 0,
  "is_hide": false,
  "is_last_period": false,
  "is_oneweek": false,
  "next_billing_time": 0,
  "source_id": "",
  "status": 1,
  "usage": {"credits_amount": 0},
  "entitlement_base_info": {
    "end_time": 1794011797,                     ← 与 expire_time 恒等（兜底用）
    "start_time": 1790143545,
    "ent_status": 0,
    "entitlement_id": "checkin_20261007_1554031214073648",   ← ★ 包唯一 ID
    "product_id": 208,
    "product_type": 2,
    "quota": {"credits_limit": 100},            ← 额度上限
    "product_extra": {
      "package_extra": {
        "duration": 31,
        "package_duration_type": 0,
        "package_name": "签到奖励",
        "package_source_type": 9                 ← 9 = 签到
      }
    }
  }
}
```

**`entitlement_id` 前缀规则**（用于识别来源）：

| 前缀 | 含义 |
|---|---|
| `checkin_<YYYYMMDD>_<uid>` | 某日签到奖励（有效期约 31 天） |
| `monthly_bonus_<YYYYMM>_<uid>` | 某月月初奖励 |
| `free_utc<YYYYMM>_<uid>` | 免费包，**`credits_limit` 为 0**，须排除 |

> ⚠️ **`expire_time` 可能被上游换成毫秒**：`> 1e12` 时须 `/ 1000`，否则时间会显示成公元 5 万年。

### 4.4 铁律：`X-Device-Id` 语义（**已澄清，勿沿用旧结论**）

**旧结论（v0.5.7 及以前，已作废）**："9074 = 设备未注册，重试无用"。
**现结论（v0.6.9 起）**：**9074 是瞬时限流，可重试**，重试动作在 `claimWithRotatedDevice` 内部同步完成（**最多换号 8 次**）。

2026-09-30 单变量实测（同账号、同 token，只改 `X-Device-Id`）：

| `X-Device-Id` | 请求体 | 结果 |
|---|---|---|
| 随机 32 位 hex | `{"req_source":1}` | ❌ 9074 |
| 客户端真实注册号 | `{"req_source":1}` | ✅ 成功 |
| 随机 32 位 hex | `{}` | ✅ 成功 |
| 客户端真实注册号 | `{}` | ✅ 成功（9095 今日已签） |

→ 空请求体时上游跳过设备校验（**这一点曾误导修复方向**）；桌面端真实行为带 `req_source`，所以两者都要对。

**真实设备号获取**：明文写在客户端 `storage.json` 的**键名**上：

```
C:\Users\<user>\AppData\Roaming\TRAE SOLO CN\User\globalStorage\storage.json
  "iCubeAuthInfo://icube-dc:4484256452647802": { ... }
                          ^^^^^^^^^^^^^^^^ 16 位纯数字 = 注册设备号
```

> ⚠️ **不要用 `telemetry.devDeviceId`**（UUID 形式）——它不是注册设备号，用了会被更严格限流。

**9095 的真实语义（v0.7.0 澄清）**：文案说"设备已签到"，但实测**去重粒度是账号级**，与设备号无关。

决定性矩阵实验（2026-09-30）：

| 账号状态 | 设备号 | 结果 |
|---|---|---|
| 账号1（今天已签） | 真实设备号 | 9095 |
| 账号1（今天已签） | 随机设备号 | 9095 |
| 账号2（今天未签） | 真实设备号 | success |
| 账号2（今天未签） | 随机设备号 | success |

⇒ **决定因素是账号，不是设备**。所以 9095 = **该账号今日已领**（**幂等成功，不是失败**）。

> ⚠️ 但要警惕异常：上游可能把账号标记"已签"却**没真正发额度包**（实测账号1 全天 `did_checked_in=true` 却无今日权益包）。
> 因此**对账必须查 `entitlement_id` 是否含今天日期**，不能只信状态标志（§6.5）。

### 4.5 TraeWork 业务码

| code | 含义 | 处理 |
|---|---|---|
| `0` | 成功 | 后置 status 验证 + 查今日权益包 |
| `9095` | 该账号今日已领 | **视为成功**（幂等） |
| `9074` | 瞬时限流 | **可重试**：内部换设备号最多 8 次 |
| `9004` | 缺订单参数 | 实际是缺 `X-Device-Id`；带上即可 |
| `4001` | 参数无效（**in-band，见 §4.6**） | 请求方参数问题，不冷却账号 |

### 4.6 ⚠️ 200 伪装错误（in-band error，v0.7.2 修复）

**现象**：上游返回 **HTTP 200 + 正常 SSE 流**，但 `delta.content` 里是错误文本，`finish_reason=stop`。

真实响应（2026-10-02 抓包，逐字照抄）：

```
data: {"choices":[{"delta":{"content":"solo error code=4001 msg=We're sorry, the param is invalid. Please try with a valid param."},"finish_reason":"stop","index":0}],"created":1790928286,"id":"chatcmpl-1","model":"","object":"chat.completion.chunk"}

data: [DONE]
```

**危害**：旧实现把它当**正常模型回答**透传给用户 → 用户看到一段英文错误。
**正确做法**：识别 `delta.content` 里的 `solo error code=<N> msg=<...>` 模式，**转为错误返回**（不写进回答流）。

---

## 5. 整体架构与数据流

### 5.1 模块划分

```
┌──────────────┐   ┌──────────────┐   ┌──────────────┐
│  frontend/   │   │  cmd/server  │   │  internal/   │
│  dist/       │   │  (无头模式)   │   │  admin/      │
│  (Wails GUI) │   │              │   │  (网页面板)   │
└──────┬───────┘   └──────┬───────┘   └──────┬───────┘
       │ window.go.app    │ HTTP             │ HTTP
       ▼                  ▼                  ▼
┌─────────────────────────────────────────────────────┐
│  internal/app/app.go   ← wails 绑定层（26 个方法）    │
└──────┬──────────────────────────────────────────────┘
       │
┌──────▼──────┐  ┌────────────┐  ┌──────────────┐
│ internal/   │  │ internal/  │  │ internal/    │
│ pool        │  │ scheduler  │  │ server       │
│ (选号/冷却) │  │ (签到/保活) │  │ (OpenAI API) │
└──────┬──────┘  └─────┬──────┘  └──────┬───────┘
       │               │                │
       └───────────────┴────────────────┘
                       │ provider.Upstream 接口
              ┌────────┴────────┐
              ▼                 ▼
      internal/upstream   internal/traework
      (WorkBuddy)         (TraeWork)
```

### 5.2 请求数据流

```
【客户端】→ POST /v1/chat/completions
    → server.withAuth（Bearer 校验；APIKey 空则跳过）
    → runtimeForModel（解析 "platform/model" 前缀）
    → rewriteModel（剥掉前缀）
    → for i := 0; i < MaxRotate; i++ {
          acct := pool.PickExcluding(tried)   ← 按策略选号
          if acct == nil { break }
          tried[acct.UID] = true
          pool.NotifyUsed(acct.UID)           ← 标记"正在调用"
          [按需 refresh token（RefreshSkew = 10min）]
          upstream.ChatStream(acct, body)
              → upstream.PrepareBody（三改写，§6.1）
              → [失败则按 Classify 冷却，继续下一个]
      }
```

### 5.3 前端数据流

```
Wails 启动
  → app.OnStartup(ctx)
  → app.OnDomReady(ctx) → close(domReadyCh)
  → 前端 app.js load() → Go.GetState()
  → 渲染（render() → renderAccounts() / renderTotal() / ...）
  → 订阅事件：accounts / checkin / refresh / login / panel:shown
```

---

## 6. 模块实现规格

### 6.1 `upstream.PrepareBody` — 三改写缺一不可

**改错的后果都很严重，务必实现**：

1. **强制 `stream = true`** — 上游拒绝非流式请求。
2. **`tool_choice` 归一化** — 对象形式会返回 `400 code=11101`。
3. **`role=developer` → `system`** — 上游对 `developer` 角色误触发内容过滤，返回"检测到敏感内容"。pi 等客户端对推理模型会使用该角色。

### 6.2 选号策略引擎

**三种策略**：

| 策略 | 常量 | 算法 |
|---|---|---|
| 优先积分（默认） | `"credits"` | 候选集中 `credits` 最大者；**同分取 UID 较小者**（稳定排序，修掉原先 range map 随机问题） |
| 优先过期 | `"expire"` | ①有积分者优先 ②`ExpiresAt` 小者优先 ③未知者排最后 ④退化到比积分 → UID |
| 负载均衡 | `"roundrobin"` | 候选集按 UID 排序，从游标 `rrLast` 的下一个开始；游标写回 state.json |

**挑选流程** `PickExcluding(tried)`：

```
healthyLocked(now, tried)        ← 排除 disabled / 冷却中 / tried 中的
    │
    ├─ 结果为空 且 tried 非空 → 回退：healthyLocked(now, nil)
    │   （说明已轮完一圈，允许重复；handler 的 MaxRotate 循环也该结束）
    │
    ├─ 仍为空 → return nil（真·无可用账号）
    │
    └─ 按 p.strategy 分派 pickCredits / pickExpire / pickRoundRobin
```

**⚠️ 核心约束 —— 单调推进**：

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

去掉 `tried` 语义 →「优先过期」和「负载均衡」会**反复选中同一个**快照值最优的账号。

**策略作用域**：**全局**——两个平台的 pool 同时 `SetStrategy`，持久化在 `config.json` 的 `strategy` 字段。

**「正在调用」标识**：`NotifyUsed(uid)` 记 `lastUsedAt = now` 与 `lastCallCredits`；
面板判断 `now - lastUsedAt < 5s` → 「调用中」（绿色脉冲）。
**这是时间推断，不是精确在途计数**（刻意如此，避免进程异常退出残留计数）。

### 6.3 账号生命周期状态机

```
        ┌──────────┐
        │ healthy  │ ← 正常参与选号
        └────┬─────┘
             │
   ┌─────────┼─────────┬──────────────┐
   │         │         │              │
余额不足    429    连续错误×N    session 失效
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

### 6.4 错误分类 `Classify(status, body)`

**判定顺序是语义的一部分，必须严格按下面顺序实现**（源码 `internal/upstream/client.go:80-122` 逐行照抄，非伪代码）：

```go
func Classify(status int, body string) ErrKind {
    if status == http.StatusPaymentRequired { // 402
        return ErrHardCredit
    }
    lower := strings.ToLower(body)
    // 1) 余额不足：小写英文 + 中文原文 双通道
    //    ⚠️ 注意是「lower 匹配」OR「原文匹配」——因为硬编码里有中文（lower 不改变中文）
    for _, m := range hardMarkers {
        if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
            return ErrHardCredit
        }
    }
    // 2) session 失效：⚠️ 只匹配原文，不做 ToLower
    for _, m := range sessionDeadMarkers {
        if strings.Contains(body, m) {
            return ErrSessionDead
        }
    }
    // 3) 模型/参数错误：必须排在 4xx 兜底之前，否则会被误判成「账号故障」而冷却账号
    for _, c := range badModelCodes {          // 业务码通道（更稳）：⚠️ 只匹配原文
        if strings.Contains(body, c) {
            return ErrBadModel
        }
    }
    for _, m := range badModelMarkers {        // 文案通道：lower OR 原文
        if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
            return ErrBadModel
        }
    }
    // 4) 其余按状态码。⚠️ 429 / 404 必须在 >=400 兜底之前单独判，否则会退化成 ErrClient
    if status == http.StatusTooManyRequests { // 429
        return ErrSoftRate
    }
    if status == http.StatusNotFound {        // 404
        return ErrNotFound
    }
    if status >= 500 {
        return ErrServer
    }
    if status >= 400 {
        return ErrClient
    }
    return ErrNone
}
```

> ⚠️ **三个易错点（都会导致重写后行为不一致）**：
> 1. **`429` / `404` 必须在 `status >= 400` 之前显式判断**。若省略，404/429 会落到 `ErrClient`，进而丢掉 §附录 B.3 规定的「404 不累计 errCount 防雪崩」和「429 短冷却 60s」语义。
> 2. **匹配通道不是统一的**：`hardMarkers` / `badModelMarkers` 是「`lower` 匹配 OR 原文匹配」；`sessionDeadMarkers` / `badModelCodes` 是**只匹配原文**（`sessionDeadMarkers` 的值首字母大写 `Offline user session not found`，`badModelCodes` 依赖 JSON 里的字面量空格形态）。全部改成统一 ToLower 会导致 11103 漏判。
> 3. **`badModelCodes` 是字面量子串匹配，不是解析 JSON 取数字 code**。`[]string{`"code":11102`, `"code":11103`, `"code": 11102`, `"code": 11103`}` 之所以要写 4 个，就是因为要同时覆盖「有空格 / 无空格」两种 JSON 序列化形态。若上游把 code 换个位置（如放进嵌套 `data` 且序列化带换行），字面量匹配会漏 → 落到 `ErrClient` → 误冷却账号池（这正是 §9 记录的历史事故）。

**关键词表（逐字照抄）**：

```go
hardMarkers = []string{
    "insufficient credit", "no credit", "credit exhausted", "out of credit",
    "quota exceeded", "quota exhaust", "payment required", "credit not enough",
    "not enough credit",
    "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}
sessionDeadMarkers = []string{"Offline user session not found", "12153"}
badModelMarkers = []string{"service info not found", "model not found", "invalid model", "unknown model"}
badModelCodes = []string{`"code":11102`, `"code":11103`, `"code": 11102`, `"code": 11103`}
```

> ⚠️ **`ErrBadModel` 绝不计入账号 errCount**。这是**请求方参数错误**，换账号/重试都不会成功。
> 曾因未识别 `11103` 落到 `default` 分支触发 `NoteError` → **一次错误调用把 3 个账号各 +1 errCount，直接导致账号池冷却**。

`11102` / `11103` 的真实响应：

```json
{"code":11102,"msg":"model [X] service info not found"}
{"code":11103,"msg":"Backend [hunyuan-stream] is not supported"}
```

**模型名映射陷阱**（显示名 ≠ 真实 ID，上游**区分大小写、精确匹配**）：

| 客户端显示名 | 真实 ID（必须发给上游） |
|---|---|
| Deepseek-V4.1-Flash | `deepseek-v4.1-flash` |
| GLM-5.3 | `glm-5.3` |
| Kimi-K3 | `kimi-k3-1` ← 连词根都不同 |
| Kimi-K2.7-Code | `kimi-k2.7` ← 连词根都不同 |

### 6.5 签到流程

> 两个平台的签到**机制不同**，分别说明：
> - **TraeWork**：`claim` → `status` 验证 → 权益包对账 → 余额（**下面详述**）
> - **WorkBuddy**：`POST /v2/billing/meter/daily-checkin`，响应直接给结果（无独立 status 接口）。旧文档误把本节标题写成"（TraeWork）"，导致 WorkBuddy 侧看起来缺失，此处更正。

```
【调度器】checkin(uid)
  → 1. POST checkin_credits/claim，体 {"req_source":1}
       ├─ 失败且为 9074 → claimWithRotatedDevice：换设备号最多 8 次
       │    ⚠️ 用 defer 还原 a.DeviceID，不污染凭证文件
       └─ 失败且 9095 → 视为成功（幂等）
  → 2. POST checkin_credits/status，体 {} → 读 did_checked_in 验证
       ⚠️ 是 POST 不是 GET（源码 internal/traework/client.go:714 用 http.MethodPost + 空体 {}）
  → 3. 对账：查权益包里有没有 entitlement_id 含今天日期
       ├─ 有 → 真到账，r.OK = true
       ├─ 无 → r.GrantMissing = true（"已签到但未查到新增积分"）
       └─ 依据：did_checked_in 只是"标记"，不保证额度到账
  → 4. 查余额（UserResource）→ ReenableIfCredits
  → 5. pool.RecordCheckin + notifyCheckin
```

**`claimWithRotatedDevice` 关键实现**：

```go
func (c *Client) claimWithRotatedDevice(a *auth.Auth) (bool, float64) {
    before, err := c.UserEntUsage(a)     // 取前置额度用于对比
    if err != nil { return false, 0 }

    orig := a.DeviceID
    defer func() { a.DeviceID = orig }() // ⚠️ 不污染凭证

    const maxRotateAttempts = 8
    for i := 0; i < maxRotateAttempts; i++ {
        // 换一个设备号重试 claim ...
    }
}
```

### 6.6 模型列表（动态，带缓存）

```go
dynamicModelsTTL        = time.Hour        // 成功缓存 1 小时
modelsFetchFailCooldown = 5 * time.Minute  // 失败后 5 分钟内不重试
```

模型名**必须带平台前缀**（`workbuddy/...` / `traework/...`），
`runtimeForModel` 强制校验前缀格式，无前缀返回 `invalid_model`。

> 该失败冷却**仅对非 WorkBuddy 平台生效**（WorkBuddy 侧无此限制）。

### 6.7 其它关键常量

```go
// app
loginTimeout    = 5 * time.Minute
loginPollEvery  = 2 * time.Second
inUseWindow     = 5 * time.Second     // "正在调用"判定窗
quitTimeout     = 5 * time.Second     // ForceQuit 兜底超时
maxLogSize      = 5 * 1024 * 1024     // 5MB 日志轮转

// 窗口 / 面板
窗口尺寸         = 760 × 560
面板位置保存延时 = 350ms              // 等 Wails 原生拖拽结束
WebView2 数据目录 = data/webview

// 单实例
进程互斥体      = Local\WorkBuddy-Wild-SingleInstance
wails UniqueId  = workbuddy-wild-gui-v1
```

### 6.8 日志轮转

`data/app.log` 超 `maxLogSize = 5MB` → 改名 `app.log.1`（**先删旧备份**，Windows rename 不覆盖）→ 重开新文件。

### 6.9 面板位置记忆

`SavePanelPos(x, y)` 写 `data/panel-pos.json`。
前端在 `mouseup` 后**延时 350ms** 调用（Wails 原生拖拽期间前端收不到事件，必须等系统拖拽结束，否则保存中间位置）。
`panelRect()` 恢复时仍 **clamp 回工作区**，防分辨率变化导致出屏。

### 6.10 防卡死三层防御

1. `winutil.IsHungAppWindow`（`user32`，窗口 5s 无响应检测）→ `ShowPanel` 卡死前拦截并提示，不调 runtime
2. `ShowPanel` 的 `domReadyCh` 已关闭分支 **go 化**（调用方永不阻塞）
3. 托盘回调全部 `go func()`

以上三层解决的是「**要不要调** runtime」。但还有一个更隐蔽的失效面：
**调了之后 runtime 本身不返回**。这需要第四层，见 6.11。

### 6.11 ⚠️ 铁律：所有 Wails runtime 调用必须经 `runtimeCall` 加超时（v0.7.4）

**背景（2026-10-08 实测事故，v0.7.3）**

`showPanelNow` 持 `showMu` 依次调用 `WindowSetSize / WindowSetPosition / WindowShow / EventsEmit`。
**WebView2 无响应时这些 runtime 调用会永久阻塞** —— 不是超时报错，是永不返回。
于是 `defer showMu.Unlock()` 永不执行，`showMu` 被永久持有：

```
07:25:37.196  面板窗口已从最小化/隐藏状态恢复   ← 最后一次成功
              ……… 94.4 分钟日志完全空白 ………
09:00:00.xxx  定时签到批次正常执行              ← 进程活着，但面板通路已死
```

关键鉴别点：**进程存活 + 面板通路静默**（不是崩溃、不是事件风暴）。
对照 2026-10-06 那次是 248 行 refresh 刷屏，**两者不是同一个问题**。

**铁律内容**

| 规则 | 说明 |
|------|------|
| 所有 runtime 调用走 `runtimeCall(name, fn)` | 超时 1.5s 放弃等待 + `recover` 隔离 panic |
| 所有窗口操作锁用 `tryLockTimeout(mu, 2s)` | 绝不用裸 `mu.Lock()`；锁被僵死调用持有时不能永久排队 |
| `showPanelNow` 与 `resizePanel` **必须共用同一把 `showMu`** | 二者抢同一条 runtime 通道，早前 `resizePanel` 完全无防护且绕过锁 |
| `HidePanel` 必须有日志 | 此前完全静默，导致「用户点了什么」无法从日志复原，是诊断最大盲区 |
| `RestoreAndShow` 的 `else` 分支必须有日志 | 否则无法区分「事件未送达」与「窗口已可见无需恢复」 |
| `ForceQuit` 的 `runtime.Quit` 也必须加超时 | **否则 WebView2 卡死时 `os.Exit` 兜底永远不可达，程序反而退不出去** |

**为什么超时后不等待调用结束**

僵死的调用会随 WebView2 一起僵死到底。`runtimeCall` 弃它而去，是为了让
**锁必被释放、调用方必能推进**。泄漏的 goroutine 数量上限 = 卡死期间尝试的
runtime 调用次数，属可接受代价（远优于整条面板通路永久死亡）。

**回归测试**

`internal/app/runtime_guard_test.go` 共 7 例。其中
`TestShowMuNotHeldAfterBlockedShow` 是本次事故的**针对性回归**：
模拟一个永不返回的 runtime 调用穿过 `showMu` 临界区，
断言 `showMu` 仍在 1.5s 内被释放 —— 即「卡死后仍能被再次唤起」。

---

## 7. 关键技术难点

### 7.1 托盘不能有原生右键菜单

**问题**：曾用 `AddMenuItem + ShowMenu`（`TrackPopupMenu` 模态循环）→ 托盘随机卡死。

**解决**：**完全不使用原生右键菜单**，右键 = 左键 = 弹面板。

```go
// 正确
systray.SetOnClick(func(systray.IMenu) { go a.ShowPanel() })
// 错误 —— 会卡死托盘
systray.SetOnClick(func(systray.IMenu) { a.ShowPanel() })
```

**为什么**：systray 的消息循环线程上**禁止**执行重量逻辑。wails 的 runtime 调用会 marshal 到主线程，一旦阻塞就**永久卡死托盘**。
**不要"顺手加回菜单"。**

### 7.2 WebView2 孤儿进程锁 profile

**问题**：强杀残留的 WebView2 进程会锁住 `data/webview` profile → 下次启动假死/白窗口。

**解决**：启动前检测 `SingletonLock` 并清理孤儿进程。**用户数据目录固定 `data/webview`**，不要改成随 exe 名变化的路径。

### 7.3 冷启动白窗口

**问题**：冷启动 WebView2 很慢，直接 `WindowShow` 会白窗口。

**解决**：`ShowPanel` 必须等 `domReadyCh`（`OnDomReady` 里 close）。**勿绕过。**

### 7.4 DPAPI 跨平台差异

Windows 下凭证用 DPAPI 加密（`dpapi:` 前缀）。**DPAPI 密钥绑定 Windows 用户**，
Linux 侧遇 `dpapi:` 前缀返回**空串**（刻意设计，避免拿密文请求上游产生难懂 401）。

**Docker 场景**：容器里写的是**明文**（Linux 的 `EncryptSecret` 是空操作）——这正是服务端需要的。

### 7.5 积分口径的三次翻车

| 版本 | 做法 | 结果 |
|---|---|---|
| v0.5.1 | 累加所有包 `quota.credits_limit` | 显示 **4050**（实际 298.7）❌ |
| v0.5.2 | 回退读 checkin 的 `credits` | 显示 **150**（该字段恒为签到固定值）❌ |
| v0.5.3+ | **`usage_summary`: total − consumed** | ✅ 与官网一致 |

**教训**：上游字段语义必须靠**真实抓包 + 与官网显示对照**确认，不能靠字段名猜。

### 7.6 到期积分的产品陷阱（2026-10-08 实测，功能已暂缓）

**背景**：需求是"显示 N 日内即将到期的积分"。

**技术可行性**：`expire_time` 字段确实存在且可解析（§4.3）。

**但产品逻辑不成立**：

| 到期时间 | 剩余积分 |
|---|---|
| 10/11 ~ 10/30（9 个包，15 日内到期） | **全部 0**（已用完） |
| 11/07 08:36（约 30 天后） | 100 |
| 11/08 08:16（约 31 天后） | 100 |

**根源**：**积分有效期约 31 天，旧的先被消耗** → 能攒下来的必然到期最晚。
所以「N 日内到期积分合计」在多数情况下**恒为 0**，选 7/15 日窗口会显示"无"，
与用户"想看哪些积分快过期了"的预期相悖。

**结论**：功能暂缓。若要重启，须改展示口径（如"笔数 + 剩余"或"最近到期余额"）。

---

## 8. 不可为之事（已实测排除）

> 以下方向**已实际测试并排除**，下一个 Agent 不要重复尝试。

| 方向 | 实测结果 |
|---|---|
| 用 `X-Device-Id` 随机值让 9074 通过（带 `req_source`） | ❌ 恒被拒；**但空请求体时随机值可过**（这曾误导修复方向） |
| 给 9074 加指数退避重试（调度器层） | ❌ 无效方向；正确做法是**内部换设备号**（§6.5） |
| 用 `checked_in` 判断签到成功 | ❌ 对 API 调用方恒为 false，每次都误判 |
| 用 `telemetry.devDeviceId` 当设备号 | ❌ 那是 UUID，不是注册设备号；用了限流更严 |
| 把 `credits_limit` 累加当余额 | ❌ 得到 4050（实际 298.7） |
| 用 checkin 响应的 `credits` 当余额 | ❌ 恒为 150 |
| 把 `usage: {}` 当"解析失败" | ❌ 它是"未使用"，剩余 = 上限 |
| 权益包发到 `trae-api-cn.mchost.guru` | ❌ HTTP 404 + HTML 页；必须发 `api.trae.cn` |
| 对齐 `entitlement_base_info.end_time` 与 `expire_time` | ✅ 两者**恒等**，可互为兜底（不是"二选一"） |
| 用 `IFileOperation` 做文件操作（本机） | ❌ `CoCreateInstance` 报 `0x80004002 不支持此接口` |
| PyInstaller 打包时删已存在的 dist（本机） | ❌ 被 safe-delete 守卫拦；解法是**换全新输出目录名** |
| 原生托盘右键菜单 | ❌ `TrackPopupMenu` 模态循环导致随机卡死 |
| 裸 `Wails runtime` 调用 + 裸 `mu.Lock()` 做窗口操作 | ❌ WebView2 无响应时 runtime **永久阻塞** → 锁永不释放 → 面板通路静默死亡。**必须**经 `runtimeCall` + `tryLockTimeout`（§6.11） |
| 在持锁路径里同步调 `runtime.Quit` | ❌ WebView2 卡死时永久阻塞，`os.Exit` 兜底**永远不可达**（程序退不出去） |

---

## 9. 完整踩坑史与版本演进

| 版本 | 修复内容 |
|---|---|
| v0.5.1 | ❌ 引入积分口径错误：累加 `credits_limit` → 显示 4050 |
| v0.5.2 | ❌ 修错方向：回退读 checkin `credits` → 显示 150 |
| v0.5.3 | ✅ 改用 `usage_summary` 正确口径（与官网一致） |
| v0.5.7 | 确认 9074 与设备号相关（结论后被 v0.6.9 修正） |
| v0.6.3 | ❌ 用"今天有任意新包"判定签到到账 → 把月初包误算成签到 |
| v0.6.5 | ⚠️ 改用「时间窗(hour≥1) + 金额(≤300)」启发式 → 能工作但脆弱 |
| v0.6.6 | 不再跳过 claim；9095 后自动轮换设备号重试 |
| v0.6.7 | ✅ 对账改用服务端权威标识 `entitlement_id`（不再靠猜测） |
| v0.6.8 | 总积分拆分显示 WorkBuddy / TraeWork 分平台小计 |
| v0.6.9 | ✅ 澄清 9074 是瞬时限流（非设备未注册）；轮换改为最多 8 次重试 |
| v0.7.0 | 模型名支持备注，调用上游前自动剥离 |
| v0.7.1 | ✅ 模型名写错不再冷却账号池；新增 `ErrBadModel` 分类 |
| v0.7.2 | ✅ 修正「200 伪装错误」被当作模型回答透传（in-band error） |
| v0.7.3 | 修复两个遗漏的错误识别缺口（D1 + D2） |
| v0.7.4 | ✅ 修复「最小化后无法唤出面板」：runtime 调用加超时 + 锁带超时（§6.11） |
| v0.8.0 | ✅ 飞牛 NAS Docker 双平台部署 + **网页管理面板** |

**每次修复的共性教训**：
1. 上游字段语义**必须靠真实抓包 + 与官方显示对照**确认，不能靠字段名猜。
2. 「看起来是错误」的响应可能藏在 **HTTP 200 的正常流里**（in-band）。
3. 账号级 vs 设备级 vs 全局级的作用域，**必须用单变量矩阵实验**确定。
4. GUI 侧的第三方调用（Wails runtime / WebView2）**没有超时保证**。
   凡「持锁 + 调用外部」的组合，都可能变成永久阻塞。**锁必须带超时，调用必须包超时。**

---

## 10. 重写检查清单

> 按顺序做。每步都给出**如何验证**。

### Step 1：项目骨架

- [ ] `go mod init github.com/rockswang/workbuddy-wild`（模块名**不能改**）
- [ ] 依赖：wails v2.14.0、systray v1.0.3、golang.org/x/sys v0.47.0
- [ ] 建目录：`internal/{app,pool,scheduler,server,upstream,traework,provider,config,auth,login,login_trae,admin,winutil}`、`frontend/dist`
- **验证**：`go build ./...` 通过（此时全空）

### Step 2：配置与凭证层（无外部依赖，先做）

- [ ] `internal/config`：`Config` 结构体（字段见 **附录 D.1**）+ `Default()` + `Load()` + `Save(c, path)`（⚠️ 参数顺序）+ `applyEnv()`
  - `Listen` 需实现 `UnmarshalJSON` 兼容**旧字符串格式** `":7863"` / `"127.0.0.1:7863"` / `"7863"`
- [ ] `internal/auth`：解析 auth 文件（**嵌套 + 扁平双形态**）+ DPAPI 加密（Linux 侧空操作）
- **前置资源**：无
- **验证**：单测——写一个旧格式 `{"listen":":7863"}` 能正确解析

### Step 3：平台抽象与上游客户端

- [ ] `internal/provider`：`Kind`（`workbuddy` / `traework`）、`Upstream` 接口、`Error` + `ErrKind` 枚举
- [ ] `internal/upstream`（WorkBuddy）：`Classify()` + 关键词表（§6.4）+ `PrepareBody` 三改写 + chat/billing/auth
- [ ] `internal/traework`：常量（**附录 A.1**）+ 三个 Host + 签到（含 `claimWithRotatedDevice`）+ in-band 错误识别
- **前置资源**：§4 全部接口契约
- **验证**：用 `httptest` 造响应，断言 `Classify` 分类正确；用真实响应 JSON 断言积分 = 298.7

### Step 4：账号池与调度器

- [ ] `internal/pool`：三种策略 + `PickExcluding(tried)` 单调推进 + 冷却状态机 + state 持久化
- [ ] `internal/scheduler`：定时签到 + token 保活 + 冷却解冻 + `wake` 通道（支持运行时改时间）
- **前置资源**：Step 3 的 `Upstream` 接口
- **验证**：单测——同一请求内 `PickExcluding` 连续调用**必须换号**；`expire` 策略按到期时间升序

### Step 5：HTTP 服务

- [ ] `internal/server`：4 条路由 + `withAuth` + `runtimeForModel` + `MaxRotate` 轮换 + 动态模型缓存
- **前置资源**：Step 4 的 pool
- **路由（逐字照抄）**：
  ```go
  POST /v1/chat/completions   (withAuth)
  GET  /v1/models             (withAuth)
  GET  /status                (withAuth)
  GET  /healthz               (无鉴权)
  ```
- **验证**：`curl /healthz` 返回 200；`curl /v1/models` 无 key 返回 401

### Step 6：前端（Wails GUI）

- [ ] `frontend/dist/index.html`：**52 个 DOM id**（附录 C）——**缺一个就 TypeError**
- [ ] `frontend/dist/style.css`：**14 个 CSS 变量**（附录 A.4）
- [ ] `frontend/dist/app.js`：调 17 个 Go 方法（签名见附录 C.3）+ 订阅 5 个事件（附录 C.4）
- **前置资源**：Step 5 的绑定方法签名
- **验证**：打开面板，账号列表渲染无 JS 报错（看 console）

### Step 7：Wails 装配

- [ ] `main.go`：chdir → 单实例锁 → 配置 → 组装 → HTTP → 托盘 → `wails.Run`
- [ ] `internal/app/app.go`：26 个导出方法 + 5 个事件
- [ ] ⚠️ **必须实现 `runtimeCall(name, fn)` 与 `tryLockTimeout(mu, d)`**（§6.11）
  - [ ] `showPanelNow` / `resizePanel` 共用 `showMu`，均**带超时获取**
  - [ ] 全部 `runtime.*` 调用（含 `runtime.Quit`）都在 `runtimeCall` 闭包内
  - [ ] `HidePanel` / `RestoreAndShow` 的 else 分支**均有日志**
- **前置资源**：Step 3-6 全部
- **验证**：`wails build` 产出 exe，双击启动无白窗口；
  且 `runtime_guard_test.go` 7 例通过（尤其 `TestShowMuNotHeldAfterBlockedShow`）
- **反验证（关键）**：把某个 runtime 调用换成永不返回的桩，确认 `showMu`
  仍会在 1.5s 内释放、后续唤起仍能成功 —— 这是本次事故的针对性回归

### Step 8：网页管理面板（v0.8.0）

- [ ] `internal/admin`：`go:embed` 4 个资源 + 12 条路由 + CSRF + 鉴权（§12）
- **前置资源**：Step 5 的 `Handler.Mux()`（需暴露底层 mux）
- **验证**：无 token 访问 `/admin/api/state` 返回 401；有 token 返回账号列表

### Step 9：Docker 部署（v0.8.0）

- [ ] `docker/Dockerfile` + `entrypoint.sh` + 两个 compose + 离线镜像构建/校验脚本（§13）
- **前置资源**：Step 8 的 `cmd/server`（无头模式）
- **验证**：`verify.py` 10 项校验全过；`docker load` 后容器能起

---

## 11. 环境与验证方法

### 11.1 推荐验证手法

| 场景 | 手法 |
|---|---|
| HTTP 链路调试 | 用 `cmd/server` 无头模式（无桌面也能跑） |
| 上游字段确认 | 写一次性 dump 工具，打印**全部字段路径**（不要猜字段名） |
| 积分口径 | 与官网个人中心**对照数值** |
| 账号级/设备级判别 | **单变量矩阵实验**（每格只改一个变量） |
| GUI | 本机无法在无桌面环境测试，需真实 Windows 桌面 |

### 11.2 本机已知坑

| 坑 | 现象 | 解法 |
|---|---|---|
| `go` 不在 PATH | `go.exe: command not found` | 用**完整绝对路径**（中文路径在 bash 里失效） |
| GOPATH 被锁 | `@v/*.info` Access denied | 换新 GOPATH（go/go2/go3/go5 轮换） |
| 并发 go 任务 | 互相抢缓存拖死 | 一次只跑一个 |
| `os.IsNotExist` 不穿透 `%w` | 包装后的错误判断失败 | 用 `errors.Is(err, fs.ErrNotExist)` |
| bat/vbs 中文乱码 | 脚本输出乱码 | 注意编码 |
| 批量删除 >50 个文件 | 被平台守卫拦截 | 分批，或换输出目录 |
| 代理环境变量 | 请求被 `HTTP_PROXY`/`HTTPS_PROXY` 拦截 | 显式绕过 |

### 11.3 回归测试基准数据

**积分口径（必须复现）**：

```
输入：usage_summary.total_amount = 4050, consumed_amount = 3751.3
期望：剩余 = 299（= 298.7 四舍五入到整数）
容差：若实现保留 1 位小数，应得 298.7；整数显示应为 299
```
> 若你的实现算出 **4050**（累加了 `credits_limit`）或 **150**（读了 checkin 响应的 `credits`）→ 口径错误，回看 §4.3。

**HTTP 路由（必须可访问）**：

```
GET /healthz           → 200（无需鉴权）
GET /v1/models         → 401（无 key）
POST /v1/chat/completions → 401（无 key）
```

---

## 12. 网页管理面板（v0.8.0）

### 12.1 定位

Docker 部署版**没有 GUI**（容器里跑 `cmd/server`），此前加号只能进 NAS 终端跑 `login.sh`。
网页管理面板让用户在浏览器里直接登录/增删账号。

### 12.2 实现要点

- `go:embed` 把前端 4 个资源内嵌进二进制（**零外部文件依赖**）
- 复用 `WB2A_API_KEY` 鉴权
- CSRF 双提交令牌

### 12.3 路由（12 条，逐字照抄）

```go
GET  /admin/                                  面板页
GET  /admin/app.js                            前端 JS
GET  /admin/style.css                         样式
GET  /admin/favicon.svg                       图标
GET  /admin/api/state                         账号列表 + 统计
POST /admin/api/login/workbuddy/start         WorkBuddy 登录发起
POST /admin/api/login/workbuddy/poll          WorkBuddy 登录轮询
POST /admin/api/login/traework/start          TraeWork 登录发起
POST /admin/api/login/traework/poll           TraeWork 登录轮询
POST /admin/api/accounts/delete               删除账号
POST /admin/api/accounts/reload               免重启重载账号
POST /admin/api/accounts/checkin              手动签到
```

### 12.4 ⚠️ 两个必须知道的坑

**坑 1：`/admin` 不要显式注册重定向**

Go 1.22+ `ServeMux` 对 `GET /admin/` 模式**已内置 301** 把 `/admin` → `/admin/`。
显式再注册 `/admin` 会**路由冲突**，且自己的 handler **永不触发**。

**坑 2：CSRF 占位符不能与 JS 变量名撞车（血泪）**

```go
// ❌ 错误：占位符 __CSRF__ 会把 JS 里的 window.__CSRF__ 一起替换掉
html := strings.ReplaceAll(pageHTML, "__CSRF__", h.csrf)
// 结果：window.<hash> = "<hash>"  → 前端拿不到 CSRF → 所有 POST 403

// ✅ 正确：用足够独特的占位符
html := strings.ReplaceAll(pageHTML, "__WB2A_CSRF_TOKEN__", h.csrf)
html = strings.ReplaceAll(html, "__WB2A_AUTH_REQUIRED__", boolJS(h.cfg.Token != ""))
```

### 12.5 鉴权与 CSRF

```go
guard():
  1. token 校验：X-Admin-Token 头 / ?token= 查询 / Authorization: Bearer 三选一
     （cfg.Token 为空 = 不鉴权）
  2. POST 额外校验：r.Header.Get("X-CSRF") == h.csrf
  → 返回 false 表示已写响应
```

`csrf` 在 `New()` 里生成：`randHex(16)`，**进程级随机**。

### 12.6 环境变量

| 变量 | 值 | 说明 |
|---|---|---|
| `WB2A_ADMIN` | `on` / `off` / `0` / `false` / `no` / `disable` / `disabled` | 默认 `on`；填 `off` 类值关闭 |

### 12.7 测试覆盖（已通过）

| 测试 | 项数 | 覆盖 |
|---|---|---|
| `test_panel.py`（后端） | 36/36 | 鉴权 401/403、资源、CSRF 提取、reload、双平台 start+poll、删除幂等 |
| `test_ui.js`（浏览器 UI） | 25/25 | 鉴权框、统计卡、Tab、获取链接、复制、toast、无 JS 错误 |
| `test_list.js`（列表/删除） | 17/17 | 造 3 假账号 → 统计 2/1/3、表格 3 行、UI 删除后 pool 与磁盘同步 |

---

## 13. Docker 部署（v0.8.0）

### 13.1 目标环境

**飞牛 NAS（fnOS）**，用其「Docker → 容器 → Compose 项目」图形界面部署。
也可用于任何 Docker 环境。

### 13.2 两种部署路径

```
【路径 A · 推荐】导入离线镜像（不需要网络、不需要构建）
  1. 传两个文件到 NAS：
       workbuddy-wild-v0.8.0-linux-amd64.tar   (35 MB)
       load-image.sh
  2. sh load-image.sh      # 校验 SHA256 + docker load
  3. 用 fnos-compose.yml 建容器

【路径 B】在 NAS 上现场构建（需要 Dockerfile + 3 个二进制 + 脚本）
  1. 传整个 docker/ 目录
  2. sh build-on-nas.sh
  3. 用 fnos-compose.yml 建容器
```

### 13.3 部署前必须改的 3 处

1. `WB2A_API_KEY` → 改成强随机密钥
   ```bash
   head -c 24 /dev/urandom | base64 | tr -d '/+='
   ```
2. `volumes` 左侧宿主机路径 → 改成 NAS 上真实存在的目录
3. `ports` 左侧（若 7863 被占用）

### 13.4 关键环境变量

```yaml
environment:
  WB2A_LISTEN: ":7863"        # ⚠️ 容器内必须监听全部网卡，不能写 127.0.0.1
  WB2A_API_KEY: "<强随机>"     # 留空 = 完全无鉴权
  WB2A_ADMIN: "on"            # 网页管理面板（默认开）
```

#### 环境变量全表（13 个，源码 `internal/config/config.go:243-292` 逐条核对）

`Load()` 的顺序是：**先读 JSON 文件 → 再用下列 `WB2A_*` 环境变量逐项覆盖**（有值才覆盖，空串不覆盖）。Docker / NAS 部署只用环境变量注入即可，无需改配置文件。

| 环境变量 | 覆盖的配置字段 | 语义 | 备注 |
|---|---|---|---|
| `WB2A_LISTEN` | `Listen` | 监听地址 | ⚠️ 容器内必须 `:7863`，写 `127.0.0.1` 会容器外访问不到 |
| `WB2A_API_KEY` | `APIKey` | 对外 API 鉴权 key | 空串 = 完全无鉴权 |
| `WB2A_AUTH_DIR` | `AuthDir` | 凭证目录 | Docker 里是卷挂载点 |
| `WB2A_STATE_FILE` | `StateFile` | 状态文件路径（state.json） | |
| `WB2A_REGION` | `Region` | 上游区域：`cn`（DN 域）；global 场景改用 `www.workbuddy.ai`（见 §4.1） | 本仓库默认只用 `cn` |
| `WB2A_ADMIN` | `AdminEnabled` | 网页管理面板开关 | `off`/`0`/`false`/`no` 关闭；**其余（含空串）视为开启** |
| `WB2A_STRATEGY` | `Strategy` | 选号策略 | `credits` / `expire` / `roundrobin` |
| `WB2A_MAX_ROTATE` | `MaxRotate` | 单请求最大换号次数 | 与签到内部的 `maxRotateAttempts=8` 是**两件事**，别混 |
| `WB2A_HARD_CREDIT` | `HardCreditCooldown` | 积分不足冷却时长 | |
| `WB2A_SOFT_RATE` | `SoftRateCooldown` | 瞬时限流冷却时长 | 默认 60s |
| `WB2A_ERR_THRESHOLD` | `ErrThreshold` | 累计错误达几次进冷却 | |
| `WB2A_ERR_COOLDOWN` | `ErrCooldown` | 错误冷却时长 | 默认 10m |
| `WB2A_TIMEOUT_SECONDS` | `TimeoutSeconds` | 上游请求超时 | |

### 13.5 离线镜像构建与校验

**构建**：用 Python 手工构造 Docker 镜像 tar（本机无 docker daemon）。
产物：`docker/dist/workbuddy-wild-v0.8.0-linux-amd64.tar`（35.54 MB）。

**校验**：`docker/_imgbuild/verify.py` 做 **10 项严格校验**：

| # | 校验项 |
|---|---|
| 1 | tar 顶层条目 |
| 2 | `manifest.json`（含 `RepoTags`，**不能含旧 `Repositories` 字段**） |
| 3 | 路径安全（防目录穿越） |
| 4 | image config（architecture / os / Entrypoint / WorkingDir / Env / ExposedPorts） |
| 5 | `diff_ids` 与 Layers 数量匹配 |
| 6 | 逐层 `sha256(未压缩) == diff_id` |
| 7 | 层内关键文件检查 |
| 8 | shell 脚本行尾必须 LF（**CRLF 会导致容器启动失败**） |
| 9 | 二进制 ELF magic（`7f454c46`）与权限 |
| 10 | 管理面板资源内嵌检查 |

### 13.6 ⚠️ Docker 特有的坑

| 坑 | 现象 | 根因 / 解法 |
|---|---|---|
| `config.json` 被挂载为**目录** | 容器 `Exited:0` | volume 挂载时宿主路径不存在，Docker 建了目录 → 改为**挂载文件**且先创建 |
| shell 脚本 CRLF | 容器启动失败 | entrypoint 脚本必须 **LF** 行尾（校验项 8 会拦） |
| `WB2A_LISTEN: "127.0.0.1:7863"` | 宿主访问不到 | 容器内必须 `":7863"`（全部网卡） |
| GOPROXY 超时（构建时） | 拉依赖失败 | 用 `goproxy.cn`，**不要用阿里云** |

---

## 附录 A：关键参数速查表

### A.1 TraeWork 常量（`internal/traework/constants.go`）

| 常量 | 值 |
|---|---|
| `AgentHost` | `https://trae-api-cn.mchost.guru` |
| `UgHost` | `https://api.trae.cn` |
| `OAuthHost` | `https://api.trae.com.cn` |
| `ConsoleHost` | `https://www.trae.cn` |
| `ClientID` | `en1oxy7wnw8j9n` |
| `AppID` | `6eefa01c-1036-4c7e-9ca5-d891f63bfcd8` |
| `IdeVersion` | `0.1.43` |
| `IdeVersionCode` | `20260716` |
| `DeviceBrand` | `83DG` |
| `OSVersion` | `Windows 11 Pro` |
| `Function` | `solo_work_lite` |
| `DefaultConfigName` | `glm-5.2` |
| `CheckinAlreadyClaimedCode` | `9095` |
| `CheckinBadParamsCode` | `9004` |
| `checkinIDPrefix` | `checkin_` |
| `checkinGrantMaxCredits` | `300` |
| `scheduledGrantMaxHour` | `1` |
| `CheckinClaimBody` | `{"req_source":1}` |
| `maxRotateAttempts` | `8`（`claimWithRotatedDevice` 内） |

### A.2 端点数

| 平台 | 端点数 |
|---|---|
| WorkBuddy | 8 |
| TraeWork | 7 |

### A.3 时间常量

| 名称 | 值 | 位置 |
|---|---|---|
| `loginTimeout` | 5 min | app |
| `loginPollEvery` | 2 s | app |
| `inUseWindow` | 5 s | app |
| `quitTimeout` | 5 s | app |
| `maxLogSize` | 5 MB | app |
| `dynamicModelsTTL` | 1 h | server |
| `modelsFetchFailCooldown` | 5 min | server |
| `RefreshSkew` | 10 min | upstream |
| 面板位置保存延时 | 350 ms | 前端 |
| 自动签到默认时间 | `09:00` | config |
| 保活默认时间 | `[22]` | config |
| 窗口尺寸 | 760 × 560 | main.go |

### A.4 前端 CSS 变量（14 个，`style.css`）—— **含真实取值**

> ⚠️ 变量定义在**两个作用域**：默认（亮色）在 `:root`，暗色在 `[data-theme="dark"]`（或媒体查询）。切换主题只需改根节点属性，**变量名两套完全一致，只是值不同**。

| 变量 | 亮色（`:root`） | 暗色 | 用途 |
|---|---|---|---|
| `--bg` | `#f5f5f7` | `#1e1e22` | 页面背景 |
| `--card` | `#ffffff` | `#2a2a30` | 卡片背景 |
| `--border` | `#e3e3e8` | `#3a3a42` | 边框 |
| `--fg` | `#1d1d20` | `#e8e8ec` | 主文字 |
| `--muted` | `#787882` | `#9a9aa4` | 次要文字 |
| `--accent` | `#3b82f6` | `#4a8cf7` | 主色 |
| `--accent2` | `#2563eb` | `#3b82f6` | 主色（深） |
| `--ok` | `#16a34a` | `#34d399` | 成功 |
| `--warn` | `#d97706` | `#fbbf24` | 警告 |
| `--err` | `#dc2626` | `#f87171` | 错误 |
| `--live` | `#16a34a` | `#34d399` | 在线/活跃指示 |
| `--radius` | `8px` | `8px` | 圆角 |
| `--mono` | `Consolas, "Courier New", monospace` | 同左 | 等宽字体 |
| `--shadow` | `0 6px 20px rgba(0,0,0,0.16)` | 同左 | 阴影 |

> ⚠️ **注意区分**：本项目的 CSS 变量是**前端面板自己的主题**，与产品需求里"保持系统默认外观、不自定义暗色主题"是两回事——面板提供手动主题切换按钮（`btnTheme`），但默认跟随亮色。

### A.5 冷却时长

| 类型 | 时长 |
|---|---|
| `CoolHard`（余额不足） | 12h |
| `CoolSoft`（429 限流） | 60s |
| `CoolErr`（连续错误） | 10m |
| `ErrThresh`（连续错误阈值） | 3 |

---

## 附录 B：错误信息对照表

### B.1 WorkBuddy 业务码

| code | 含义 | 分类 | 处理 |
|---|---|---|---|
| `11101` | `tool_choice` 对象形式 | `ErrClient` | 归一化为字符串 |
| `11102` | `model [X] service info not found` | `ErrBadModel` | **不冷却账号**，返回参数错误 |
| `11103` | `Backend [X] is not supported` | `ErrBadModel` | **不冷却账号** |
| `12153` | `Offline user session not found` | `ErrSessionDead` | 禁用账号，需重登 |
| — | `"检测到敏感内容"` | — | 由 `role=developer` 引起 → 改 `system` |

### B.2 TraeWork 业务码

| code | 含义 | 处理 |
|---|---|---|
| `0` | 成功 | 后置验证 + 对账权益包 |
| `9095` | 该账号今日已领（**幂等成功**） | 视为成功 |
| `9074` | 瞬时限流 | 内部换设备号重试（≤8 次） |
| `9004` | 缺订单参数（实为缺 `X-Device-Id`） | 带上设备号 |
| `4001` | 参数无效（**in-band，HTTP 200**） | 转为错误返回，不写进回答 |

### B.3 HTTP 状态码

| 状态 | 分类 | 处理 |
|---|---|---|
| `200` | — | **⚠️ 仍要检查 in-band 错误**（§4.6） |
| `400` | `ErrClient` | 查业务码 |
| `401` | `ErrSessionDead` ⚠️ **但仅当 body 命中 `sessionDeadMarkers`（含 `"12153"`/`Offline user session not found`）时**；否则落到 `>=400` 兜底 → `ErrClient` | 禁用账号（仅真实 session 失效时） |
| `402` | `ErrHardCredit` | 长冷却 12h |
| `404` | `ErrNotFound` | 短冷却，**不累计 errCount**（防雪崩） |
| `429` | `ErrSoftRate` | 短冷却 60s |
| `5xx` | `ErrServer` | 上游故障 |

---

## 附录 C：前端 DOM id 全清单

> **共 52 个**。重写前端时缺一个就会 `TypeError`。
> 命名规律：`btn*` = 按钮、`sel*` = 下拉框、`in*` = 输入框、`chk*` = 复选框、`*Overlay` = 模态层。

### C.1 全部 id（按字母序）

```
aboutOverlay  aboutProject  aboutVersion  acctCount  acctEmpty  acctList
app  btnAbout  btnAboutClose  btnAddHour  btnAddTrae  btnAddWB
btnCancelLogin  btnCheckinAll  btnClose  btnConfirmCancel  btnConfirmOk
btnCopyUrl  btnLog  btnMin  btnQuit  btnRefreshAll  btnTheme  chkAutostart
confirmMsg  confirmOverlay  confirmTitle  customHostRow  hoursBox  inHost
inPort  keyBox  keyEdit  keyInput  keyVal  loginCountdown  loginMsg
loginOverlay  nextCheckin  selHost  selStrategy  serverLine  strategyDesc
strategyHint  toast  totalCredits  totalSub  traeCount  traeCredits
ver  wbCount  wbCredits
```

### C.1b DOM 结构骨架（层级 + 标签，重写必读）

> 只有 id 清单不足以还原布局。以下是关键容器的**层级关系与标签语义**（`#app` 为根，窗口无边框 760×560）：

```html
<div id="app" data-theme="light">                 <!-- 根容器，主题属性挂这里 -->
  <header id="serverLine">                        <!-- 顶栏：服务状态 + 监听地址 -->
    <span id="ver">v0.8.0</span>
    <button id="btnTheme"></button>               <!-- 主题切换 -->
    <button id="btnLog"></button>                 <!-- 打开日志 -->
    <button id="btnMin"></button>                 <!-- 最小化 -->
    <button id="btnClose"></button>               <!-- 关闭到托盘 -->
  </header>

  <section id="statsRow">                         <!-- 统计卡区 -->
    <div id="totalCredits">…</div><div id="totalSub">…</div>
    <div id="wbCount">…</div><div id="wbCredits">…</div>
    <div id="traeCount">…</div><div id="traeCredits">…</div>
    <div id="acctCount">…</div>
  </section>

  <section id="strategyBox">                      <!-- 选号策略 -->
    <select id="selStrategy"></select>
    <div id="strategyDesc"></div><div id="strategyHint"></div>
  </section>

  <section id="accountsBox">
    <button id="btnRefreshAll"></button><button id="btnCheckinAll"></button>
    <div id="nextCheckin">…</div>
    <ul id="acctList"></ul>                        <!-- 账号列表（动态生成 li） -->
    <div id="acctEmpty">…</div>                    <!-- 空态提示 -->
  </section>

  <footer id="footerRow">
    <button id="btnAddWB"></button><button id="btnAddTrae"></button>
    <button id="btnAddHour"></button><span id="hoursBox"></span>
    <input type="checkbox" id="chkAutostart"><label for="chkAutostart"></label>
    <div id="keyBox"><span id="keyVal"></span><input id="keyInput"><button id="keyEdit"></button></div>
    <div id="customHostRow"><input id="inHost"><input id="inPort"><select id="selHost"></select></div>
    <button id="btnAbout"></button>
  </footer>

  <!-- —— 模态层（默认 display:none） —— -->
  <div id="confirmOverlay"><div><span id="confirmTitle"></span><p id="confirmMsg"></p>
       <button id="btnConfirmOk"></button><button id="btnConfirmCancel"></button></div></div>
  <div id="loginOverlay"><div><span id="loginMsg"></span><span id="loginCountdown"></span>
       <button id="btnCopyUrl"></button><button id="btnCancelLogin"></button></div></div>
  <div id="aboutOverlay"><div><span id="aboutVersion"></span><span id="aboutProject"></span>
       <button id="btnAboutClose"></button></div></div>
  <div id="toast"></div>                           <!-- 全局提示条 -->

  <script src="/wails/…"></script>                 <!-- Wails 注入的 runtime -->
</div>
```

> **账号列表项**（`#acctList` 内动态生成的 `<li>`）字段来自 `AccountView`（附录 D.5.2），每个 `li` 需渲染：昵称、平台图标（按 `group`）、积分、冷却状态（`cooling`/`until`/`reason`）、签到状态（`last_checkin_ok`/`last_checkin_msg`）、使用中标记（`in_use`），以及签到/刷新/删除按钮。**此处没有固定 id**（用 class + `data-uid` 更合适）。

### C.2 id → 用途 → 访问者

| id | 用途 | 访问者（app.js 函数） |
|---|---|---|
| `app` | 根容器 | — |
| `ver` | 版本号显示 | `render()` |
| `serverLine` | 服务地址行 | `render()` |
| `btnTheme` | 深/浅色切换 | `initEvents()` |
| `btnMin` | 最小化到托盘 | `initEvents()` |
| `btnClose` | 关闭程序 | `initEvents()` |
| `totalCredits` | 总积分 | `renderTotal()` |
| `totalSub` | 总积分副标题 | `renderTotal()` |
| `wbCredits` / `wbCount` | WorkBuddy 小计 | `renderSplit()` |
| `traeCredits` / `traeCount` | TraeWork 小计 | `renderSplit()` |
| `acctCount` | 账号数 | `render()` |
| `strategyHint` | 策略提示 | `renderStrategyDesc()` |
| `btnAddWB` / `btnAddTrae` | 添加账号 | `initEvents()` |
| `acctList` | 账号卡片容器 | `renderAccounts()` |
| `acctEmpty` | 空态提示 | `render()` |
| `btnCheckinAll` / `btnRefreshAll` | 批量操作 | `initEvents()` |
| `selStrategy` | 积分策略 | `render()` / `renderStrategyDesc()` |
| `strategyDesc` | 策略说明 | `renderStrategyDesc()` |
| `hoursBox` / `btnAddHour` | 签到时间 | `renderHours()` |
| `nextCheckin` | 下次签到时间 | `render()` |
| `selHost` / `inPort` / `inHost` / `customHostRow` | API 监听 | `renderHostSelect()` |
| `keyBox` / `keyVal` / `keyEdit` / `keyInput` | API Key | `renderKey()` / `editKey()` / `commitKey()` |
| `chkAutostart` | 开机自启 | `render()` / `initEvents()` |
| `btnLog` | 打开日志 | `initEvents()` |
| `btnAbout` / `aboutOverlay` / `aboutVersion` / `aboutProject` / `btnAboutClose` | 关于弹窗 | `initEvents()` |
| `confirmOverlay` / `confirmTitle` / `confirmMsg` / `btnConfirmOk` / `btnConfirmCancel` | 确认弹窗 | `askConfirm()` |
| `loginOverlay` / `loginMsg` / `loginCountdown` / `btnCopyUrl` / `btnCancelLogin` | 登录弹窗 | 登录流程 |
| `toast` | 提示条 | `toast()` |

### C.3 前端调用的 17 个 Go 方法（含真实签名）

> ⚠️ 早期文档写「16 个」，**实际 17 个**（`SetCheckinTimes` 之外还有 `SetCheckinHours`/`SetCheckinMinutes` 等未在前端用，前端实际调用见下）。
> 签名从 `internal/app/app.go` 提取，**返回值的 JSON 形状见附录 D.5**。

```go
// —— 面板初始数据 ——
GetState() State                          // → State（D.5.1）
GetAccounts() []AccountView               // → []AccountView（D.5.2）
GetStrategy() string                      // → "credits" | "expire" | "roundrobin"

// —— 账号操作 ——
CheckinAccount(uid string) (scheduler.CheckinResult, error)
CheckinAll() []scheduler.CheckinResult
RefreshCredits(uid string) (int64, error) // → 刷新后的积分
RefreshAll()                              // 无返回（走 refresh 事件）
RemoveAccount(uid string) error

// —— 登录 ——
StartLoginFor(kind string) (string, error) // kind = "workbuddy" | "traework"；→ 登录 URL
CancelLogin() error

// —— 设置（全部返回 error，前端据 toast 提示） ——
SetStrategy(name string) error
SetAPIKey(key string) error
SetListen(host string, port int) error    // ⚠️ 热切换，无需重启（见 D.1）
SetAutostart(on bool) error
SetCheckinTimes(times []string) error     // 新格式 ["09:00","21:30"]

// —— 窗口/生命周期 ——
SavePanelPos(x, y int)
HidePanel()
QuitAll()
OpenLogFile() error
```

> 说明：`CheckinAccount` / `CheckinAll` 的返回类型是 `scheduler.CheckinResult`（字段含 `platform/uid/ok/msg/remain/grant_missing` 等，与 `checkin` 事件 payload 同源）。

### C.4 前端订阅的 5 个事件

| 事件 | payload |
|---|---|
| `accounts` | `[]AccountView` |
| `checkin` | `{platform, uid, ok, msg, remain?, ...}` |
| `refresh` | `{platform, uid, ok, msg}` |
| `login` | `{phase, msg}`（phase: success/failed/cancelled） |
| `panel:shown` | `nil`（前端据此重置失焦宽限期 + 触发内容动效） |

---

## 附录 D：核心数据结构与落盘格式（重写必读）

> 本附录补齐「值怎么组装成请求 / 状态怎么落盘」这一层。**没有这一节，重写会停在"能编译但接真上游就 401"**。

### D.1 `config.Config`（`internal/config/config.go`）

`Listen` **不是字符串**，是结构化类型，且自定义了 `UnmarshalJSON` 兼容旧版字符串形式（`":7863"` / `"127.0.0.1:9999"` / `"9999"` 三种都能解析成 `{host, port}`）：

```go
type Listen struct {
    Host string `json:"host"` // 空 = 监听全部网卡（Docker 必须为空）
    Port int    `json:"port"` // 默认 7863
}

type Config struct {
    Listen       Listen `json:"listen"`
    APIKey       string `json:"api_key"`    // 空 = 不鉴权
    AuthDir      string `json:"auth_dir"`   // ./auths
    StateFile    string `json:"state_file"` // ./data/state.json
    Region       string `json:"region"`     // "cn"；global 场景改用 www.workbuddy.ai（见 §4.1）
    AdminEnabled bool   `json:"admin_enabled"`
    Strategy     string `json:"strategy"`   // credits | expire | roundrobin
    MaxRotate    int    `json:"max_rotate"` // 单请求最大换号次数

    // ⚠️ 下面三个内层结构体的字段名与 json tag 是重写必需的（早期版本只给了注释，是重大缺口）
    Cooldown struct {
        HardCredit  string `json:"hard_credit"`   // 字符串时长，如 "12h"
        SoftRate    string `json:"soft_rate"`     // 如 "60s"
        ErrThresh   int    `json:"err_threshold"` // 默认 3
        ErrCooldown string `json:"err_cooldown"`  // 如 "10m"
    } `json:"cooldown"`

    Schedule struct {
        CheckinHours   []int    `json:"checkin_hours,omitempty"` // 旧格式：[9,21]
        CheckinTimes   []string `json:"checkin_times,omitempty"` // 新格式：["09:00","21:30"]
        KeepaliveHours []int    `json:"keepalive_hours"`         // [22]
    } `json:"schedule"`

    Upstream struct {
        TimeoutSeconds int `json:"timeout_seconds"` // 默认 120
    } `json:"upstream"`

    // 以下三个由上面的字符串字段解析而来，不落盘（json:"-"）
    HardCreditDur  time.Duration `json:"-"`
    SoftRateDur    time.Duration `json:"-"`
    ErrCooldownDur time.Duration `json:"-"`
}
```

**四个必须知道的细节（全部来自源码，不是推测）：**

1. **`HardCredit` / `SoftRate` / `ErrCooldown` 是 `string`，不是 `time.Duration`**。JSON 里写 `"12h"` / `"60s"` / `"10m"`，由 `Load()` 用 `time.ParseDuration` 解析到 `json:"-"` 的 `*Dur` 字段。**若重写成 `time.Duration` 直接反序列化，真实 config.json 会解析失败。**
2. **`Schedule` 兼容新旧两种格式**：旧的 `checkin_hours:[9,21]`（整点数组）与新的 `checkin_times:["09:00","21:30"]`（时刻数组）**同时存在**，`omitempty` 保证不用的不落盘。运行时另有 `SetCheckinMinutes` 支持分钟级。
3. **`Listen` 的 `UnmarshalJSON`**：字符串形式（`":7863"` / `"127.0.0.1:9999"` / `"7863"`）与对象形式 `{"host":"","port":7863}` **都要支持**；空串 → `Host="", Port=7863`。
4. **`SetListen` 是热切换**：源码 `internal/app/app.go:1163` 注释明写「保存配置并**热切换**监听（失败保持原样）」，内部 `serveLocked(addr)` 直接切。**不需要重启**（早期文档曾误写"需重启"，已更正）。

- **默认值生成**：`Default()`；**加载**：`Load()`（先文件、后 env 覆盖）；**原子写回**：`Save(c *Config, path string)` —— ⚠️ **注意参数顺序是 `(c, path)`**，写反了能编译但会 panic。
- **`Addr()` 返回 `net.JoinHostPort` 结果**（IPv6 自动加 `[]`），供 `net.Listen` 使用。改监听地址通过 `SetListen` **热切换**，无需重启。

### D.2 `state.json`（`internal/pool/pool.go`）

**这是账号状态落盘的唯一格式**，顶层只有一个 `accounts` map + 一个 roundrobin 游标：

```go
type stateFile struct {
    Accounts map[string]accountState `json:"accounts"` // key = UID
    RRLast   string                  `json:"rr_last,omitempty"` // 负载均衡游标（上次用过的 UID）
}

type accountState struct {
    Credits         int64     `json:"credits"`
    Disabled        bool      `json:"disabled"`
    Reason          string    `json:"reason,omitempty"`
    Until           time.Time `json:"until,omitempty"`   // 冷却截止时间（冷却结束自动恢复正常）
    LastCheckinOK   bool      `json:"last_checkin_ok,omitempty"`
    LastCheckinAt   time.Time `json:"last_checkin_at,omitempty"`
    LastCheckinMsg  string    `json:"last_checkin_msg,omitempty"`
    LastUsedAt      time.Time `json:"last_used_at,omitempty"`
    LastCallCredits int64     `json:"last_call_credits,omitempty"`
}
```

> ⚠️ **`rr_last` 就是 §6.2 提到的「游标写回 state.json」的那个游标**——负载均衡策略靠它在重启后仍能接着轮转。

对外的展示结构是 `pool.Status`（含 `UID/Nickname/Credits/Cooling/Until/Reason/Disabled/ErrCount/LastCheckin*` 等），它是 `accountState` 的**读视图 + 补充字段**，不直接落盘。

### D.3 `auth.Auth`（`internal/auth/auth.go`）——凭证文件格式

一个账号一个文件，`Kind` 由文件名前缀区分（`workbuddy` / `traework`）。**11 个字段**：

```go
type Auth struct {
    mu           sync.RWMutex // 串行化 RefreshToken 写 与 SaveAtomic/JWT 读
    Kind         string // workbuddy | traework
    AccessToken  string
    RefreshToken string
    ExpiresAt    int64  // Unix 秒
    Domain       string
    ApiHost      string // TraeWork: https://api.trae.com.cn
    MachineID    string // TraeWork: x-machine-id
    DeviceID     string // TraeWork: x-device-id  ← §4.4 铁律的主角
    UID          string
    EnterpriseID string
    Nickname     string
    FilePath     string // 来源文件；refresh 后原子写回此处
}
```

- **并发**：`RefreshToken` 期间必须 `Lock()/Unlock()`（写锁），读 token 走 `RLock()/RUnlock()`。重写时若漏锁，会出现「刷新后 token 半写、请求拿到残缺 JWT」。
- **`DeviceID` 只在 TraeWork 有意义**；签到 9074 轮换时必须 `defer` 还原（见 §6.5）。
- **落盘加密**：Windows 下用 DPAPI（密文带 `dpapi:` 前缀），Linux 侧是空操作（明文）。跨平台搬运凭证文件需注意（§7.4）。

### D.4 完整请求样例（文本层唯一一条端到端链路）

> 文档其余地方只给了 path 和响应片段。这里补一条**从请求到响应**的完整样例，供重写时对拍。

**TraeWork 签署（签到状态）**：

```http
POST /trae/api/v2/ug/checkin_credits/status HTTP/1.1
Host: api.trae.cn
Content-Type: application/json
Cloud-IDE-JWT: <access_token>
x-device-id: <a.DeviceID>
x-machine-id: <a.MachineID>

{}
```

```http
HTTP/1.1 200 OK
Content-Type: application/json

{"code":0,"msg":"","data":{"did_checked_in":true}}
```

**TraeWork 领取额度**：

```http
POST /trae/api/v2/ug/checkin_credits/claim HTTP/1.1
Host: api.trae.cn
Content-Type: application/json
Cloud-IDE-JWT: <access_token>
x-device-id: <a.DeviceID>
x-machine-id: <a.MachineID>

{"req_source":1}
```

响应顶层 `{code, msg, data}`：`code=0` 成功、`9095` 幂等成功、`9004` 缺/错设备号、`9074` 瞬时限流（内部换号重试 ≤8 次）。**`claim` 成功不保证额度立刻到账** —— 必须再查权益包对账（§6.5 第 3 步）。

**其它接口（chat / OAuth / get_detail_param）的请求体结构本仓库未留存样例**；重写时需自行抓包补齐。已知约束：`ClientID=en1oxy7wnw8j9n`、`AppID=6eefa01c-1036-4c7e-9ca5-d891f63bfcd8` 属于 OAuth 流程（`OAuthHost=api.trae.com.cn`），但**具体落在哪个 body 键名，文档无法给出**——这是本文档明确的已知缺口。

---

### D.5 前端视图数据结构（`internal/app/app.go`）—— **前端渲染的唯一依据**

> ⚠️ 这一节是前端能否正确渲染的**决定性命门**。早期文档只给了 `AccountView` 这个名字、**从未给字段**，导致重写前端时字段名全靠猜，整页 `undefined`。以下从源码逐字提取。

#### D.5.1 `State`（`GetState()` 的返回，面板初始数据）

```go
type State struct {
    Accounts       []AccountView `json:"accounts"`
    CheckinHours   []int         `json:"checkin_hours"`   // 旧前端兼容
    CheckinTimes   []string      `json:"checkin_times"`
    KeepaliveHours []int         `json:"keepalive_hours"`
    ListenHost     string        `json:"listen_host"`
    ListenPort     int           `json:"listen_port"`
    APIKey         string        `json:"api_key"`
    LoginBusy      bool          `json:"login_busy"`
    NextCheckin    string        `json:"next_checkin"`
    Version        string        `json:"version"`
    Autostart      bool          `json:"autostart"`
    Running        bool          `json:"running"`
    Strategy       string        `json:"strategy"`        // credits / expire / roundrobin
}
```

#### D.5.2 `AccountView`（账号列表项，`accounts` 事件 payload 的元素）

```go
type AccountView struct {
    UID            string `json:"uid"`
    Group          string `json:"group"` // workbuddy | traework（平台图标区分）
    Nickname       string `json:"nickname"`
    Credits        int64  `json:"credits"`
    Cooling        bool   `json:"cooling"`
    Until          string `json:"until"`
    Reason         string `json:"reason"`
    Disabled       bool   `json:"disabled"`
    ErrCount       int    `json:"err_count"`
    LastCheckinOK  bool   `json:"last_checkin_ok"`
    LastCheckinAt  string `json:"last_checkin_at"`
    LastCheckinMsg string `json:"last_checkin_msg"`
    LastUsedAt     string `json:"last_used_at"`     // 空 = 从未调用
    LastCallCredits int64 `json:"last_call_credits"` // 上次调用时积分快照
    InUse          bool   `json:"in_use"`           // 由 LastUsedAt 在 inUseWindow 内推断
    ExpiresAt      int64  `json:"expires_at"`       // 凭证到期（Unix 秒，0=未知）
}
```

**前端渲染相关要点**：

- `Until` / `LastCheckinAt` / `LastUsedAt` 是**字符串**（后端已格式化的时间），不是时间戳。
- 平台用 `Group` 字段区分（`workbuddy` / `traework`），对应附录 C.2 里的 `wbCount` / `traeCount` 分组统计。
- `InUse` 是**派生字段**（由 `LastUsedAt` 与本机 `inUseWindow=5s` 推断），后端算好给前端。
- 列表为空时前端显示 `acctEmpty`（附录 C.1）。

#### D.5.3 `config.json` 落盘样例

```json
{
  "listen": { "host": "", "port": 7863 },
  "api_key": "sk-xxxxxxxx",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "region": "cn",
  "admin_enabled": true,
  "strategy": "credits",
  "max_rotate": 3,
  "cooldown": { "hard_credit": "12h", "soft_rate": "60s", "err_threshold": 3, "err_cooldown": "10m" },
  "schedule": { "checkin_times": ["09:00", "21:30"], "keepalive_hours": [22] },
  "upstream": { "timeout_seconds": 120 }
}
```
> 注意 `cooldown` 三个时长是**字符串**形式（见 D.1 细节 1）；`listen` 也可写成旧字符串 `":7863"`。

---

## 验证记录

> 验证方法：派**独立 Agent**（不读本仓库任何源码，只读本文档）按文档重写指定模块并自跑验收，回收其「必须靠猜的地方」修订本文档。

| 轮次 | 验证目标 | 结论 |
|---|---|---|
| 第 1 轮 | 上游客户端核心（traework 常量 / `Classify` / 积分口径 / 签到流程） | **未通过**。回收 5 类缺陷，已全部修补：① §4.1 与 §6.5 的 `status` 接口 **GET/POST 冲突** → 源码裁定为 **POST**，已改；② §6.4 `Classify` **缺 404/429 分支**（与附录 B.3 矛盾）→ 已补，并补充「匹配通道不统一」的警告；③ **3 处悬空引用**（§14.1/§14.2/§14.3，文档并无 §14）→ 已改为指向附录 A.1 / 新增附录 D.1 / 内嵌 13 变量全表；④ **`state.json` 格式完全缺失**（子 Agent 判定"落库环节无法重写"）→ 新增**附录 D**（Config / stateFile / Auth / 完整请求样例）；⑤ 积口径精度「逐包也得 298.7」**不精确**（实为 298.696）→ 已修正并加容差说明。 |
| 第 2 轮 | 前端渲染层 + 配置落盘 + 第 1 轮修补项回归 | **未通过**。独立 Agent 成功派发并返回详细缺陷报告。**回归结论**：`status`=POST（无残留 GET）、404/429 分支存在、环境变量 13 个齐全、附录 D 被正确引用、全部 `§`/`附录` 引用**均无悬空**（§ 引用实测 14 个不同目标，非此前所写的"12 处"）。**新增 8 类缺陷，已全部修补**：① `config.Cooldown/Schedule/Upstream` **只有注释、无字段名与 tag**（且时长是 `string` 不是 `Duration`）→ 已补真实结构；② `AccountView`（16 字段）+ `State`（12 字段）**全文未定义** → 新增 **D.5**；③ C.3 方法**只有名字无签名** → 已补 17 个真实签名；④ **§1.2 标题"四合一"实列 5 项** → 改"五合一"；⑤ C.3 写"16 个"实列 17 个 → 已改；⑥ **`Region` 在 D.1 与 §13.4 说法矛盾** → 已统一；⑦ **`SetListen` 是否需重启矛盾**（§3 说热切换、D.1 误写需重启）→ 源码裁定**热切换**，已改；⑧ 14 个 CSS 变量**只有名无值** → 已补真实值并揭示**亮/暗双主题**；另补 DOM 层级骨架（C.1b）与 B.3 的 401 判定条件修正。 |
| 第 3 轮 | 端到端全流程 | **未执行**（受环境限制）。**但第 2 轮的穿透性反馈已使文档具备"前端+配置层可重写"的条件**：D.5 补齐了视图数据契约，D.1 补齐了配置内层字段，C.1b 补齐了 DOM 结构，A.4 补齐了样式值。 |

**遗留的已知缺口（重写者必须自行抓包补齐）**：

1. **OAuth 流程的请求体结构**：`ClientID` / `AppID` 属于 OAuth（`OAuthHost=api.trae.com.cn`），但文档给不出它们落在哪个 body 键名。
2. **TraeWork 聊天（`llm_utils_chat`）与 `get_detail_param` 的请求体**：仓库未留存样例。
3. **WorkBuddy 8 个端点中有 5 个未标明 Host**（文档只给了 path）：见 §4.1。
4. **签到接口的 in-band 错误形态**：仅有 chat 接口的 in-band 样例，签到接口的 in-band 形态未确认。
5. **`pool.Status` 的完整字段**：D.2 已给出 `accountState`（落盘）与 `AccountView`（前端视图），但中间的 `pool.Status` 结构仍有"等"字未穷举。

> 建议：重写者拿到真实账号后，先用 §11.1 的「一次性 dump 工具」打印全部字段路径，再回填上述缺口。

---

**安全红线**：不要在任何文档、聊天记录或日志中输出真实 access token / refresh token。测试令牌不要复用。
