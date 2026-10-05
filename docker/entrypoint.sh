#!/bin/sh
# 服务端启动入口。
#
# 设计：由 WB2A_* 环境变量生成 /app/config.json（在容器内生成，绝不 bind 挂载单文件，
# 根治旧版"config.json 被挂载成空目录→程序崩溃"的问题），再交给服务端读取。
# 用 shell 读取环境变量比依赖 Go 二进制自身读 env 更稳，跨平台一致。
#
# 注意：服务端自身也支持 WB2A_* 直读，这里的 config.json 只是多一层显式落盘，
# 便于 `docker exec cat /app/config.json` 排查「实际生效的配置到底是什么」。
set -e

AUTH_DIR="${WB2A_AUTH_DIR:-/data/auths}"
STATE_FILE="${WB2A_STATE_FILE:-/data/state.json}"
LISTEN="${WB2A_LISTEN:-:7863}"
API_KEY="${WB2A_API_KEY:-}"
REGION="${WB2A_REGION:-cn}"
STRATEGY="${WB2A_STRATEGY:-credits}"
MAX_ROTATE="${WB2A_MAX_ROTATE:-3}"

mkdir -p "$AUTH_DIR"
mkdir -p "$(dirname "$STATE_FILE")"

cat > /app/config.json <<EOF
{
  "listen": "${LISTEN}",
  "api_key": "${API_KEY}",
  "auth_dir": "${AUTH_DIR}",
  "state_file": "${STATE_FILE}",
  "region": "${REGION}",
  "strategy": "${STRATEGY}",
  "max_rotate": ${MAX_ROTATE}
}
EOF

echo "[entrypoint] generated /app/config.json"
echo "[entrypoint]   listen=${LISTEN}  region=${REGION}  strategy=${STRATEGY}  max_rotate=${MAX_ROTATE}"
echo "[entrypoint]   auth_dir=${AUTH_DIR}"
echo "[entrypoint]   state_file=${STATE_FILE}"
if [ -z "$API_KEY" ]; then
    echo "[entrypoint]   ⚠️  API_KEY 为空：接口无鉴权！仅限本机/内网使用，切勿暴露公网"
else
    echo "[entrypoint]   api_key=<set>"
fi

exec /app/workbuddy-wild-server -config /app/config.json
