# 极简运行镜像：只 COPY 预编译好的静态二进制，容器内不编译、不拉 Go 模块。
# 根治旧版在 NAS 上 go mod download 访问 proxy.golang.org 超时（i/o timeout）的问题。
#
# 三个二进制均由开发机交叉编译（GOOS=linux GOARCH=amd64 CGO_ENABLED=0）：
#   workbuddy-wild-server  服务端（双平台：workbuddy + traework）
#   workbuddy-login        WorkBuddy 登录助手
#   workbuddy-login-trae   TraeWork 登录助手
FROM alpine:3.20

# ca-certificates: 服务端要对外发起 HTTPS（copilot.tencent.com / api.trae.cn 等上游）
# tzdata: 定时签到/保活按本地时区
# wget:   healthcheck 探测 /healthz
# jq:     登录助手把 snake_case 输出转成 auth 嵌套形
RUN apk add --no-cache ca-certificates tzdata wget jq

WORKDIR /app

COPY workbuddy-wild-server /app/workbuddy-wild-server
COPY workbuddy-login /app/workbuddy-login
COPY workbuddy-login-trae /app/workbuddy-login-trae
COPY entrypoint.sh /app/entrypoint.sh
COPY login.sh /app/login.sh
COPY login-trae.sh /app/login-trae.sh
RUN chmod +x /app/workbuddy-wild-server /app/workbuddy-login /app/workbuddy-login-trae \
             /app/entrypoint.sh /app/login.sh /app/login-trae.sh

# 只挂载目录，绝不挂载可能不存在的单文件（旧版 config.json 被挂载成空目录导致崩溃）
VOLUME ["/data"]
EXPOSE 7863

# 全部用环境变量配置，服务端原生支持 WB2A_* 并回退默认值，无需 config.json
ENV WB2A_LISTEN=":7863" \
    WB2A_AUTH_DIR="/data/auths" \
    WB2A_STATE_FILE="/data/state.json" \
    WB2A_REGION="cn" \
    TZ="Asia/Shanghai"

ENTRYPOINT ["/app/entrypoint.sh"]
