#!/bin/sh
# 服务端启动入口。
#
# 设计：由 WB2A_* 环境变量生成 /app/config.json（在容器内生成，绝不 bind 挂载单文件，
# 根治旧版“config.json 被挂载成空目录→程序崩溃”的问题），再交给服务端读取。
# 用 shell 读取环境变量比依赖 Go 二进制自身读 env 更稳，跨平台一致。
set -e

AUTH_DIR="${WB2A_AUTH_DIR:-/data/auths}"
STATE_FILE="${WB2A_STATE_FILE:-/data/state.json}"
LISTEN="${WB2A_LISTEN:-:7863}"
API_KEY="${WB2A_API_KEY:-WorkBuddy2API}"
REGION="${WB2A_REGION:-cn}"

mkdir -p "$AUTH_DIR"
mkdir -p "$(dirname "$STATE_FILE")"

cat > /app/config.json <<EOF
{
  "listen": "${LISTEN}",
  "api_key": "${API_KEY}",
  "auth_dir": "${AUTH_DIR}",
  "state_file": "${STATE_FILE}",
  "region": "${REGION}"
}
EOF

echo "[entrypoint] generated /app/config.json  listen=${LISTEN}  region=${REGION}  api_key_set=$([ -n "$API_KEY" ] && echo yes || echo no)"
echo "[entrypoint] auth_dir=${AUTH_DIR}  state_file=${STATE_FILE}"

exec /app/workbuddy-wild-server -config /app/config.json
