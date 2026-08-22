# WorkBuddy-Wild · 飞牛 NAS (fnOS) Docker 部署

把 WorkBuddy-Wild 的**无头服务端**（`cmd/server`，OpenAI 兼容 API 代理：自动签到 / 保活 / 多账号轮询 / `/v1/chat/completions`）用 Docker Compose 部署到飞牛 NAS。

> 部署的是**服务端**，不是桌面 GUI。桌面端是 Windows 程序，不能进容器；容器里跑的是同一个仓库里的 `cmd/server` 无头模式。

---

## 一、之前为什么总是部署失败（经验教训）

| # | 旧版失败现象 | 根因 | 本次根治方案 |
|---|--------------|------|--------------|
| 1 | 多阶段 Dockerfile 在容器内 `go mod download` 拉 `proxy.golang.org` → `i/o timeout`，镜像永远构建不出 | NAS 构建环境访问不到 Go 官方代理 | **开发机预编译**静态 `linux/amd64` 二进制，镜像只做 `COPY`。NAS 端零 Go 工具链、零模块下载、**完全不依赖 GOPROXY** |
| 2 | 容器一启动就 `Exited`（日志里镜像 Built 但 `Exited: 0`） | compose 里 `./config.json:/app/config.json` 把**单文件**挂载成了**空目录**；程序以目录当 JSON 解析失败 → `log.Fatalf` 退出 | **由 entrypoint 用 `WB2A_*` 环境变量在容器内生成 `/app/config.json`**，二进制读取该文件。config.json **永不 bind 挂载**，彻底杜绝“挂载成目录”崩溃；账号/状态只挂载**目录** `./data:/data` |
| 3 | 并发构建时 `GOMODCACHE` 锁死 | 多版本 Go 共用缓存 | 预编译一次，镜像构建不再跑 `go build` |

**额外新发现的坑（与部署无直接关系，但很关键）：**
- v0.3.0 桌面端给凭据加了 **Windows DPAPI 加密**。Linux/NAS **无法解密 DPAPI**，且若加密代码无 `//go:build windows` 约束还会导致 Linux 编译失败。
- 当前服务端 checkout（master）是**明文** auth（无 DPAPI），因此账号文件**跨平台通用**：可直接把 Windows 桌面端的 `workbuddy-*.json` 复制进 NAS 的 `auths/` 卷。
- 若以后桌面端重引入 DPAPI，必须：① 用 `//go:build windows` 隔离；② NAS 端账号保持明文。

---

## 二、文件清单（整个 `docker/` 目录传到 NAS 即可）

```
docker/
├── Dockerfile              # 极简 alpine，只 COPY 预编译二进制
├── docker-compose.yml      # fnOS compose 部署
├── entrypoint.sh           # 建数据目录 + 由环境变量启动
├── login.sh                # 交互式登录助手（可选）
├── workbuddy-wild-server   # 预编译 linux/amd64 静态二进制（服务端）
├── workbuddy-login         # 预编译 linux/amd64 静态二进制（登录助手）
├── .env.example            # 环境变量模板（可选）
└── auth.example.json       # 账号文件模板
```

> `workbuddy-wild-server` / `workbuddy-login` 已在开发机用 Go 1.26.7 交叉编译为 `linux/amd64`、`CGO_ENABLED=0` 静态二进制，可在任意 x86_64 Linux（含飞牛 NAS）直接运行。

---

## 三、飞牛 NAS 部署步骤

1. 把本 `docker/` 目录整体传到 NAS（fnOS 的「文件」里建个目录，如 `/vol1/@app/compose/workbuddy-wild/`，把文件传进去；或用 SSH `scp`）。
2. （可选）SSH 进 NAS，或从 fnOS「终端」进入该目录：
   ```bash
   cd /vol1/@app/compose/workbuddy-wild
   ```
3. **改密钥**：编辑 `docker-compose.yml`，把 `WB2A_API_KEY: "CHANGE_ME_strong_api_key"` 换成强随机值（这是接口 Bearer 鉴权密钥）。
4. 启动：
   ```bash
   docker compose up -d --build
   ```
   首次会从 `Dockerfile` 构建镜像（只复制二进制，几秒完成），随后容器常驻。
5. 看日志确认没崩：
   ```bash
   docker compose logs -f workbuddy-wild
   ```
   正常会看到：`workbuddy-wild listening on :7863 (api_key=true)`。
6. 健康检查：
   ```bash
   curl http://127.0.0.1:7863/healthz   # 返回 ok
   ```

> fnOS 图形界面部署：在「Docker → Compose」新建项目，把 `docker-compose.yml` 内容贴进去（或上传目录），「环境变量」里把 `WB2A_API_KEY` 设好，启动即可。

---

## 四、添加账号（二选一）

服务端本身**没有**账号管理网页，账号以 JSON 文件形式放在 `auths/` 目录（卷已挂到 `./data/auths`）。

### 方法 A（推荐，最稳）：复制桌面端账号文件
Windows 桌面端会把账号存为 `workbuddy-*.json`（明文）。找到这些文件，直接复制进 NAS 的 `./data/auths/` 目录，文件名保持 `workbuddy-<uid>.json` 即可。重启容器加载：
```bash
docker compose restart workbuddy-wild
```

### 方法 B（高级）：用登录助手现场登录
在 NAS 上交互式完成 OAuth（必须在**同一次容器会话**内走完 url→浏览器登录→poll）：
```bash
docker compose run --rm workbuddy-wild /app/login.sh
```
按提示在浏览器打开授权 URL、登录，回车后自动把 token 转成 auth 文件写入 `./data/auths/`。

> 字段格式（手动新建 `workbuddy-<uid>.json` 时参考 `auth.example.json`）：
> 嵌套形 `{ "auth": {accessToken, refreshToken, expiresAt, domain, ...}, "account": {uid, ...} }` 或扁平形 `{accessToken, uid, ...}` 都支持。
> `domain` 留空 = 国内账号（cn）；含 `workbuddy.ai` = 国际账号（global），需与 `WB2A_REGION` 对应。

---

## 五、对接与使用

服务暴露 OpenAI 兼容接口：

- `POST /v1/chat/completions` —— 模型名需带前缀：`workbuddy/glm-5.2`、`workbuddy/hy3` 等
- `GET  /v1/models` —— 列出已接入账号可用的模型
- `GET  /status`   —— 查看账号池
- `GET  /healthz`  —— 健康检查

示例（本地或局域网内）：
```bash
curl http://<NAS_IP>:7863/v1/models \
  -H "Authorization: Bearer <你的WB2A_API_KEY>"
```

在 OpenAI 客户端 / 聊天前端里填：
- Base URL：`http://<NAS_IP>:7863/v1`
- API Key：你的 `WB2A_API_KEY`
- 模型：`workbuddy/glm-5.2` 等

如需外网访问，用飞牛「Lucky」或 fnOS 反代到该端口并加 HTTPS（接口有 Bearer 鉴权，但建议仍走 HTTPS）。

---

## 六、升级

重新在开发机交叉编译新二进制覆盖 `workbuddy-wild-server` / `workbuddy-login`，传到 NAS 后：
```bash
docker compose up -d --build
```
`./data` 卷里的账号与状态不受影响，无需重新添加。

---

## 七、排错

| 现象 | 排查 |
|------|------|
| 容器 `Exited` / 起不来 | `docker compose logs workbuddy-wild`；确认没有去挂载 `config.json` 单文件；确认 `./data` 目录有写权限 |
| `curl /healthz` 不通 | 容器端口映射 `7863:7863` 是否成功；`WB2A_LISTEN` 必须是 `:7863`（监听全部），不能是 `127.0.0.1:7863` |
| `/v1/models` 返回空 | `auths/` 里没有账号文件，或 `domain`/`WB2A_REGION` 不匹配；`/status` 看加载了几条 |
| 接口 401 | `WB2A_API_KEY` 与请求里的 Bearer 不一致 |
| 模型报错 no_healthy_account | 账号 token 过期/被冷却，检查 `/status` 与日志里的 refresh 结果 |

---

## 八、客户端接入：WorkBuddy 桌面端「添加自定义模型」

在 WorkBuddy 客户端里「添加模型 → 自定义 / Custom」，按下面填，即可把对话路由到本服务：

| 对话框字段 | 填写值 | 说明 |
|------------|--------|------|
| 接口地址 | `http://127.0.0.1:7863/v1/chat/completions` | 本机跑服务填这个；服务在 NAS 上则改成 `http://<NAS_IP>:7863/v1/chat/completions` |
| API Key | 你的 `WB2A_API_KEY` | 默认 `WorkBuddy2API`；在 compose 里改过就填改后的值 |
| 模型名称 | `workbuddy/glm-5.2` | **必须带 `workbuddy/` 前缀**，见下方可用列表 |
| 工具调用 | 勾选 | 服务端支持 |
| 图片输入 | 按需 | 部分模型支持 |
| 推理模式 | 按需 | deepseek-v4 系列可开 |

> 每个模型单独添加一条（点一次「保存」），对话时即可切换不同模型。

**可用模型名称**（服务端 `internal/server/handler.go` 的静态列表，前缀均为 `workbuddy/`）：

```
workbuddy/glm-5.2
workbuddy/glm-5.1
workbuddy/glm-5v-turbo
workbuddy/kimi-k2.7
workbuddy/minimax-m3
workbuddy/hy3
workbuddy/hy3-preview
workbuddy/hy3-preview-agent
workbuddy/deepseek-v4-pro
workbuddy/deepseek-v4-flash
```

> 若账号 refresh 后服务端从上游拉到了动态模型列表，以 `/v1/models` 返回的为准（需带 `Bearer` 鉴权）：
> ```bash
> curl -H "Authorization: Bearer <你的WB2A_API_KEY>" http://<NAS_IP>:7863/v1/models
> ```

