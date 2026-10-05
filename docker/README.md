# WorkBuddy-Wild · 飞牛 NAS (fnOS) Docker 部署

把 WorkBuddy-Wild 的**无头服务端**用 Docker Compose 部署到飞牛 NAS，得到一个 OpenAI 兼容 API 网关：
多账号轮询 / 自动签到 / 保活 / 按模型前缀双平台路由（workbuddy + traework）。

> 部署的是**服务端**（`cmd/server` 无头模式），不是桌面 GUI。桌面端是 Windows 程序，不能进容器。

---

## 一、这个版本修了什么（演进史）

| # | 旧版现象 | 根因 | 现状 |
|---|----------|------|------|
| 1 | 镜像永远构建不出，`go mod download` 报 `i/o timeout` | NAS 构建环境访问不到 `proxy.golang.org` | **开发机预编译**静态 `linux/amd64` 二进制，镜像只做 `COPY`。NAS 端零 Go 工具链 |
| 2 | 容器一启动就 `Exited: 0` | compose 把 `./config.json` **单文件**挂载成了**空目录**，程序解析 JSON 失败 → `log.Fatalf` | 由 entrypoint 用 `WB2A_*` 环境变量在容器内生成 `/app/config.json`，**永不 bind 挂载**；只挂目录 `./data:/data` |
| 3 | **`traework/*` 模型全部 404** | `cmd/server` 停留在单平台版本（只 `LoadDir` workbuddy），而桌面端 `main.go` 早已是双平台 | ✅ **已修**：`cmd/server` 重构为双平台（两个 pool + 两个 scheduler + `Runtimes` 路由） |
| 4 | **账号文件拷过去全部失效** | Windows 桌面端用 **DPAPI 加密** token（`dpapi:` 前缀），密钥绑定 Windows 用户；Linux 无法解密，只能返回空串 | ✅ 本版策略：**在 NAS 上重新登录**（见第四节）。这是设计使然，不是 bug |
| 5 | traework 无法在容器内登录 | ① 无登录入口 ② 登录依赖 `127.0.0.1` 本地回调 ③ 签到设备号从 Windows 客户端读 | ✅ **已修**：新增 `login-trae.sh`（两段式手工回调）+ `--device-id` 参数 |
| 6 | traework 签到恒返 `9074` | 设备号是随机的，服务端按「注册指纹」校验 | ✅ 用 `TRAE_DEVICE_ID` 传入客户端真实设备号（见第四节 B） |

---

## 二、文件清单

把整个 `docker/` 目录传到 NAS：

```
docker/
├── Dockerfile              # 极简 alpine，只 COPY 预编译二进制
├── docker-compose.yml      # 命令行版（在 docker/ 目录内 docker compose up -d --build）
├── fnos-compose.yml        # ★ 飞牛图形界面版（粘贴到「Docker → Compose」）
├── build-on-nas.sh         # ★ 在 NAS 上构建镜像的一键脚本
├── entrypoint.sh           # 建数据目录 + 由环境变量生成 config.json 并启动
├── login.sh                # WorkBuddy 登录助手
├── login-trae.sh           # TraeWork 登录助手（需 TRAE_DEVICE_ID）
├── workbuddy-wild-server   # 预编译 linux/amd64（服务端，双平台）
├── workbuddy-login         # 预编译 linux/amd64（WorkBuddy 登录助手）
├── workbuddy-login-trae    # 预编译 linux/amd64（TraeWork 登录助手）
├── .env.example            # 环境变量模板（可选）
└── auth.example.json       # 账号文件格式参考
```

> 三个二进制均在开发机用 **Go 1.26.7** 交叉编译为 `linux/amd64`、`CGO_ENABLED=0` 静态二进制，可直接在任意 x86_64 Linux（含飞牛 NAS）运行。

### 两个 compose 文件怎么选

| | `docker-compose.yml` | `fnos-compose.yml` |
|---|---|---|
| 适用 | SSH / 终端操作 | 飞牛「Docker → Compose」图形界面 |
| 镜像 | `build:` 现场构建 | `image:` 用已有镜像 |
| 挂载 | 相对路径 `./data` | 绝对路径 `/vol1/appdata/...` |
| 额外 | — | 日志轮转（防写满磁盘） |

---

## 三、部署步骤

### 3.1 先把镜像准备好（二选一）

**路径 A · 在 NAS 上构建**（推荐）：

```bash
# 把整个 docker/ 目录传到 NAS，比如 /vol1/@app/compose/workbuddy-wild/
cd /vol1/@app/compose/workbuddy-wild
sh build-on-nas.sh          # 检查文件 + 构建 workbuddy-wild:latest
```

**路径 B · 从别处导出镜像**（本机没有 Docker 也能用在线构建机）：

```bash
# 在有 Docker 的机器上
docker build -t workbuddy-wild:latest .
docker save workbuddy-wild:latest -o wb.tar
# 传到 NAS 后
docker load -i wb.tar
```

### 3.2 启动

**图形界面**：飞牛「Docker → 容器 → 新增 → Compose 项目」，把 `fnos-compose.yml` 内容粘贴进去，
改好 `WB2A_API_KEY` 与卷路径后启动。

**命令行**：

```bash
cd /vol1/@app/compose/workbuddy-wild
docker compose -f fnos-compose.yml up -d
```

### 3.3 验证

```bash
docker compose -f fnos-compose.yml logs -f workbuddy-wild
```
正常输出类似：
```
[entrypoint] generated /app/config.json
[entrypoint]   listen=:7863  region=cn  strategy=credits  max_rotate=3
[entrypoint]   auth_dir=/data/auths
[entrypoint]   api_key=<set>
config: listen=:7863 auth_dir=/data/auths ... strategy=credits max_rotate=3
loaded accounts: workbuddy=0 cn, traework=0 from /data/auths
⚠️  没有任何账号：请先用 docker/login.sh 登录 ...
workbuddy-wild listening on :7863 (api_key=true)
```
> 首次启动必然提示「没有任何账号」——正常，下一步去登录。

健康检查：
```bash
curl http://127.0.0.1:7863/healthz      # 返回 ok
```

---

## 四、添加账号（★ 必须在 NAS 上重新登录）

### 为什么不能直接拷 Windows 的 auth 文件

Windows 桌面端把 token 加密成 `dpapi:...` 密文（`internal/auth/secure_windows.go`）。
DPAPI 密钥绑定「当前 Windows 用户」，**换机器/换用户都解不开**。
Linux 侧 `secure_other.go` 遇到 `dpapi:` 前缀会返回**空串**（并在日志告警），
而不是拿密文去请求上游 —— 这是刻意设计，避免产生一堆难懂的 401。

**所以跨平台迁移账号的唯一可靠方式是在目标平台重新登录。** 见下。

> 本节命令默认读 `docker-compose.yml`。若你用 `fnos-compose.yml` 部署，
> 每条命令都加 `-f fnos-compose.yml`，例如
> `docker compose -f fnos-compose.yml run --rm workbuddy-wild /app/login.sh`。

---

### A. WorkBuddy 账号

```bash
docker compose run --rm workbuddy-wild /app/login.sh
```

按提示：
1. 脚本打印一个授权 URL → 在你电脑浏览器打开、登录 CodeBuddy
2. 回到终端按回车 → 脚本自动轮询、换 token、写入 `./data/auths/workbuddy-<uid>.json`

> 必须在**同一次 `docker compose run` 会话**内完成（state 文件在容器 `/tmp`）。

---

### B. TraeWork 账号（★ 需要设备号）

```bash
docker compose run --rm \
  -e TRAE_DEVICE_ID=4484256452647802 \
  workbuddy-wild /app/login-trae.sh
```

**设备号怎么取**（不做这步，签到会一直返回 `9074`）：

1. 在**装过 Trae 客户端的 Windows 电脑**上，打开目录：
   ```
   %APPDATA%\TRAE SOLO CN\User\globalStorage\
   ```
   （也可能是 `Trae CN` / `Trae` / `TraeWork`，哪个存在用哪个）
2. 用记事本打开 `storage.json`，搜索：`iCubeAuthInfo://icube-dc:`
3. 冒号后面那串 **16 位数字**就是设备号，例如 `4484256452647802`

**登录流程**（两段式，因为容器里的 `127.0.0.1` 你访问不到）：

1. 脚本打印授权 URL → 在你电脑浏览器打开、登录 Trae
2. 浏览器会跳到一个**打不开的页面**（容器内的 `127.0.0.1:18080`）—— 这是**正常的**
3. 复制地址栏里的**完整 URL 整段**，粘贴回终端回车
4. 脚本解析 `refreshToken` → 换 access token → 写入 `./data/auths/trae-<uid>.json`

> 同一账号多个设备号时，可用不同 `TRAE_DEVICE_ID` 各登录一次，便于日志区分。
> 但注意：**多账号不需要多设备号**——去重键是「账号+设备」，同一设备号下多账号本就能各签一次。

---

### 登录完成后

```bash
docker compose restart workbuddy-wild
docker compose logs --tail=30 workbuddy-wild   # 确认 loaded accounts 数量正确
```

---

## 五、账号文件格式（手建时参考）

```jsonc
{
  "auth": {
    "accessToken": "明文 token（Linux 侧不加密）",
    "refreshToken": "明文 refresh token",
    "expiresAt": 1792332001,          // Unix 秒
    "domain": "",                     // WorkBuddy: 空=国内, workbuddy.ai=国际
                                      // TraeWork: 固定 "trae.cn"
    "apiHost": "https://api.trae.com.cn",   // 仅 TraeWork
    "machineId": "fc564da81dea87d0b816ce4c97ed08c0",  // 仅 TraeWork（32 位 hex）
    "deviceId": "4484256452647802"                    // 仅 TraeWork（★ 签到必需）
  },
  "account": { "uid": "...", "enterpriseId": "", "nickname": "备注名" }
}
```

- 文件名必须是 `workbuddy-<uid>.json` 或 `trae-<uid>.json`（前缀决定平台路由）。
- 也支持扁平形 `{"accessToken":..., "uid":...}`。
- `WB2A_REGION=cn` 时，`domain` 含 `workbuddy.ai` 的账号会被跳过（那是国际账号）。

---

## 六、对接与使用

服务暴露 OpenAI 兼容接口：

| 端点 | 说明 |
|------|------|
| `POST /v1/chat/completions` | **模型名必须带前缀**：`workbuddy/glm-5.2` 或 `traework/glm-5.2` |
| `GET /v1/models` | 列出两平台全部可用模型（各 10 个，共 20 个） |
| `GET /status` | 账号池状态（按平台分组：`accounts.workbuddy` / `accounts.traework`） |
| `GET /healthz` | 健康检查（无需鉴权） |

```bash
curl http://<NAS_IP>:7863/v1/models -H "Authorization: Bearer <WB2A_API_KEY>"
```

在 OpenAI 客户端填：
- Base URL：`http://<NAS_IP>:7863/v1`
- API Key：你的 `WB2A_API_KEY`
- 模型：`workbuddy/glm-5.2`、`traework/glm-5.2` 等

**可用模型**（静态列表，实际以 `/v1/models` 为准）：

```
workbuddy/glm-5.2          traework/glm-5.2
workbuddy/glm-5.1          traework/glm-5-turbo
workbuddy/glm-5v-turbo     traework/glm-5
workbuddy/kimi-k2.7        traework/DeepSeek-V4-Pro
workbuddy/minimax-m3       traework/DeepSeek-V4-Flash
workbuddy/hy3              traework/kimi-k2.6
workbuddy/hy3-preview      traework/...
workbuddy/hy3-preview-agent
workbuddy/deepseek-v4-pro
workbuddy/deepseek-v4-flash
```

---

## 七、环境变量速查

| 变量 | 默认 | 说明 |
|------|------|------|
| `WB2A_LISTEN` | `:7863` | 监听地址。容器内**必须**是 `:7863`（不能 `127.0.0.1`） |
| `WB2A_API_KEY` | — | Bearer 鉴权密钥。**留空=不鉴权**，仅限内网 |
| `WB2A_AUTH_DIR` | `/data/auths` | 账号目录 |
| `WB2A_STATE_FILE` | `/data/state.json` | 状态文件（同目录会派生 `state-workbuddy.json` / `state-traework.json`） |
| `WB2A_REGION` | `cn` | `cn` 或 `global` |
| `WB2A_STRATEGY` | `credits` | 选号策略：`credits` / `expire` / `roundrobin` |
| `WB2A_MAX_ROTATE` | `3` | 单请求最多试几个账号 |
| `WB2A_HARD_CREDIT` | `12h` | 余额不足冷却时长 |
| `WB2A_SOFT_RATE` | `60s` | 限流冷却时长 |
| `WB2A_ERR_THRESHOLD` | `3` | 累计错误几次进入冷却 |
| `WB2A_ERR_COOLDOWN` | `10m` | 错误冷却时长 |
| `WB2A_TIMEOUT_SECONDS` | `120` | 上游超时 |

---

## 八、升级

1. 在开发机上重新交叉编译（在源码目录执行）：
   ```bash
   export GOROOT="<你的 Go 安装目录>"
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GOROOT/bin/go.exe" build -o docker/workbuddy-wild-server ./cmd/server
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GOROOT/bin/go.exe" build -o docker/workbuddy-login ./cmd/login
   GOOS=linux GOARCH=amd64 CGO_ENABLED=0 "$GOROOT/bin/go.exe" build -o docker/workbuddy-login-trae ./cmd/login_trae
   ```
2. 把新二进制传到 NAS 覆盖对应文件
3. 重新构建镜像并重启：
   ```bash
   sh build-on-nas.sh                              # 或 docker build -t workbuddy-wild:latest .
   docker compose up -d                            # 用 fnos-compose.yml 则加 -f fnos-compose.yml
   ```
   > 用 `fnos-compose.yml`（`image:` 模式）时，光覆盖二进制不生效 —— 必须先重建镜像。

`./data` 卷里的账号与状态不受影响，无需重新登录。

---

## 九、排错

| 现象 | 排查 |
|------|------|
| 容器 `Exited` / 起不来 | `docker compose logs workbuddy-wild`；确认没有去挂载 `config.json` 单文件；确认 `./data` 有写权限 |
| `curl /healthz` 不通 | 端口映射 `7863:7863`；`WB2A_LISTEN` 必须是 `:7863` |
| `/v1/models` 只有一半模型 | 对应平台的 `auths/` 里没账号文件。看启动日志 `loaded accounts: workbuddy=N, traework=M` |
| 日志出现 `检测到 Windows DPAPI 密文` | 你把 Windows 的 auth 文件直接拷过来了 → 在 NAS 上重新登录（第四节） |
| traework 签到恒返 `9074` | 设备号不对/随机。用 `TRAE_DEVICE_ID` 传真实设备号重新登录 |
| traework 报 `9095` | **不是错误**：= 今日已签到。服务端视为成功 |
| `9074` 偶发（非恒定） | 瞬时限流。客户端会自动等 8s 重试一次，属正常 |
| 接口 401 | `WB2A_API_KEY` 与请求里的 Bearer 不一致 |
| `no_healthy_account` | 账号 token 过期/被冷却。查 `/status` 与日志里的 refresh 结果 |
| 容器时间不对导致签到错过 | 确认 `TZ=Asia/Shanghai` |

---

## 十、安全提醒

- `WB2A_API_KEY` 留空 = **接口完全无鉴权**，任何能访问该端口的人都能消耗你的账号额度。生产环境务必设置。
- 账号 token 在 Linux 侧以**明文**存在 `/data/auths/*.json`，安全性完全依赖：
  1. 宿主机文件权限（建议 `chmod 700 data/auths` 且非 root 运行）
  2. 容器隔离边界（**不要把 `/data` 挂到共享目录**）
- 如需外网访问，用飞牛「Lucky」或 fnOS 反代并启用 HTTPS。
