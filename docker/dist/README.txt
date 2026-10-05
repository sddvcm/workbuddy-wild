# workbuddy-wild · 飞牛 NAS 部署指南

> 本文回答两件事：**① 怎么安装部署；② 离线镜像怎么用（不用联网拉取）**

---

## 一、你拿到的是什么

`docker/dist/` 目录下的文件：

| 文件 | 大小 | 说明 |
|---|---|---|
| `workbuddy-wild-v0.8.0-linux-amd64.tar` | 35 MB | **离线镜像包**，`docker load` 直接导入，无需任何网络 |
| `load-image.sh` | 1.4 KB | 一键导入脚本（自动校验 SHA256 + 导入 + 打印下一步） |
| `SHA256SUMS` | 96 B | 校验值 |

另外还需 **`fnos-compose.yml`**（在 `docker/` 目录）用于启动容器。

**镜像里已包含：** alpine 3.20 基础系统 + 三个 Go 二进制（服务端 / WorkBuddy 登录助手 / Trae 登录助手）+ 三个 shell 脚本 + CA 根证书。
**不需要联网**：不含任何 `apk add`、不 build、不拉基础镜像。

---

## 二、安装部署（5 步）

### 步骤 1 · 建数据目录

飞牛 NAS 上用 SSH 或终端：

```sh
mkdir -p /vol1/appdata/workbuddy-wild
```

> 如果你的存储池不是 `/vol1`，换成实际路径（飞牛默认第二个存储空间是 `/vol2`）。

### 步骤 2 · 上传两个文件

把 `workbuddy-wild-v0.8.0-linux-amd64.tar` 和 `load-image.sh` 上传到刚建的目录：

```
/vol1/appdata/workbuddy-wild/
   ├── workbuddy-wild-v0.8.0-linux-amd64.tar
   └── load-image.sh
```

飞牛 NAS 可用「文件管理」直接拖拽上传，或用 SMB 共享拷贝。

### 步骤 3 · 导入镜像

```sh
cd /vol1/appdata/workbuddy-wild
sh load-image.sh
```

看到 `Loaded image: workbuddy-wild:latest` 就成功了。验证：

```sh
docker images | grep workbuddy-wild
```

> **手动导入**（不用脚本时）：
> ```sh
> sha256sum -c SHA256SUMS      # 校验（可选）
> docker load -i workbuddy-wild-v0.8.0-linux-amd64.tar
> ```

### 步骤 4 · 启动容器

**方式 A：飞牛图形界面（推荐）**

1. 打开「Docker」→「Compose」→「新增项目」
2. 项目名填 `workbuddy-wild`
3. 把 `fnos-compose.yml` 内容整段粘贴进去
4. **改 3 处**（文件里标了 `★必改`）：
   - `WB2A_API_KEY`：换成强随机密钥
   - `volumes` 左侧路径：确认为 `/vol1/appdata/workbuddy-wild`
   - `ports` 左侧 `7863`：被占用时改成别的（如 `17863`）
5. 点「启动」

**方式 B：命令行**

```sh
cd /vol1/appdata/workbuddy-wild
# 把 fnos-compose.yml 也传上来
docker compose -f fnos-compose.yml up -d
```

生成 API Key 的方法：

```sh
head -c 24 /dev/urandom | base64 | tr -d '/+='
```

### 步骤 5 · 验证

```sh
# 健康检查（无需鉴权）
curl http://127.0.0.1:7863/healthz
# → ok

# 查看启动日志
docker logs -f workbuddy-wild
```

---

## 三、添加账号（登录）

镜像已启动但还没有账号，需要登录一次把凭证写进 `/data/auths/`。

### WorkBuddy 账号

```sh
cd /vol1/appdata/workbuddy-wild
docker compose -f fnos-compose.yml run --rm workbuddy-wild /app/login.sh
```

按提示：
1. 脚本打印一个授权 URL
2. 复制到**你电脑的浏览器**打开，登录 CodeBuddy
3. 回到终端按回车
4. 自动写入 `/data/auths/workbuddy-<uid>.json`

### TraeWork 账号

**前提**：需要一个 16 位设备号（Trae 服务端按注册指纹校验，随机值会导致签到失败返回 9074）。

在**装过 Trae 客户端的 Windows 电脑**上取：

1. 打开目录 `%APPDATA%\TRAE SOLO CN\User\globalStorage\`
2. 记事本打开 `storage.json`，搜索 `iCubeAuthInfo://icube-dc:`
3. 冒号后面那串 16 位数字就是设备号（如 `4484256452647802`）

然后：

```sh
docker compose -f fnos-compose.yml run --rm \
  -e TRAE_DEVICE_ID=你的16位设备号 \
  workbuddy-wild /app/login-trae.sh
```

脚本打印授权 URL → 浏览器登录 → **登录后浏览器会跳到一个打不开的页面**（因为回调地址是容器内的 127.0.0.1，这是正常的）→ 把地址栏里那个完整 URL 整段复制 → 粘贴回终端回车。

### 加完账号重启

```sh
docker compose -f fnos-compose.yml restart workbuddy-wild
```

日志里应该出现 `loaded accounts: workbuddy=N cn, traework=M`。

---

## 四、对接使用

服务端是 **OpenAI 兼容接口**，把以下配置填进任意支持自定义 base_url 的客户端：

| 项 | 值 |
|---|---|
| Base URL | `http://<NAS的IP>:7863/v1` |
| API Key | 你设的 `WB2A_API_KEY` |
| 模型 | 任意，请求 `/v1/models` 拿列表 |

常用端点：

```sh
curl http://<NAS_IP>:7863/healthz                       # 健康检查
curl http://<NAS_IP>:7863/status                        # 账号状态
curl -H "Authorization: Bearer <你的KEY>" \
     http://<NAS_IP>:7863/v1/models                     # 模型列表
```

---

## 五、离线包是怎么造的（原理）

本机没有 Docker，无法 `docker build`。离线包是**手工构造 docker load 格式的 tar**：

```
workbuddy-wild-v0.8.0-linux-amd64.tar
├── manifest.json                    # Config / RepoTags / Layers
├── <image_id>.json                  # 镜像 config（含 env、entrypoint、diff_ids）
├── repositories                     # 旧式仓库映射
└── <diff_id>/layer.tar              # 文件系统层（alpine + 应用）
```

关键点（踩过的坑）：

| 坑 | 后果 | 解决 |
|---|---|---|
| 从 Windows 解包目录读权限 | 执行位全丢，`bin/sh` 变 0666 → 容器起不来 | **直接从 alpine.tar.gz 流式读原始 mode**（0755） |
| `manifest.json` 用 `Repositories` 字段 | 现代 docker load 忽略，tag 恢复不了 | 改用 `RepoTags` |
| shell 脚本 CRLF 行尾 | `/bin/sh^M: bad interpreter` | 打包时强制 `\r\n`→`\n`，并加 `.gitattributes` |
| 依赖 `jq` 转 JSON | 需 `apk add`，破坏离线 | 给 `workbuddy-login` 加 `auth` 子命令，直出 auth 格式 |

**镜像内零 apk 依赖**：`ca-certificates` 用 minirootfs 自带的、`sh`/`wget` 用 busybox、`jq` 已消除、`tzdata` 已消除。

重新打包（改代码后）：

```sh
# 1. 交叉编译
cd wbsync
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o docker/workbuddy-wild-server ./cmd/server/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o docker/workbuddy-login   ./cmd/login/
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o docker/workbuddy-login-trae ./cmd/login_trae/

# 2. 打包 + 校验
python docker/_imgbuild/mkimage.py
python docker/_imgbuild/verify.py
```

---

## 六、常见问题

| 现象 | 原因 | 处理 |
|---|---|---|
| `docker load` 报 `invalid manifest` | tar 损坏 | 重新传输，先 `sha256sum -c SHA256SUMS` |
| 容器起来就 `Exited (0)` | 配置问题 | `docker logs workbuddy-wild` 看具体报错 |
| 8443 端口访问不了 | 只监听 127.0.0.1 | 确认 `WB2A_LISTEN=:7863`（前面有个冒号） |
| 日志 `loaded accounts: 0` | 还没登录 | 执行第三步的 login 脚本 |
| 签到失败返回 9074 | TraeWork 设备号不对 | 用真实 16 位设备号重新登录 |
| `/healthz` 通但 `/v1/models` 401 | API Key 不对 | 检查 Bearer token 与 `WB2A_API_KEY` 一致 |
| 宿主机访问不到 7863 | 端口被占用 | 改 compose 里 ports 左侧端口 |

---

## 七、安全提醒

- **`WB2A_API_KEY` 绝不能留空**，留空等于接口完全无鉴权，任何人可消耗你的账号额度
- 不要把 7863 端口直接暴露到公网；需要外网访问请走 Lucky 反向代理 + HTTPS + 额外鉴权
- `/data` 目录里的 auth 文件是明文凭证，做好目录权限（默认 `700`）与备份
- 容器默认以 root 运行；如需更安全可取消 compose 里 `user: "1000:1000"` 的注释（但要先 `chown` 数据目录）
