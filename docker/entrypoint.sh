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

# json_escape: 把字符串里的 \ " 换行 制表 转义，防止路径/密钥含特殊字符时
# 生成出非法 JSON（否则服务端启动即崩，排查成本很高）。
json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' | tr -d '\r\n\t'
}

AUTH_DIR="${WB2A_AUTH_DIR:-/data/auths}"
STATE_FILE="${WB2A_STATE_FILE:-/data/state.json}"
LISTEN="${WB2A_LISTEN:-:7863}"
API_KEY="${WB2A_API_KEY:-}"
REGION="${WB2A_REGION:-cn}"
STRATEGY="${WB2A_STRATEGY:-credits}"
MAX_ROTATE="${WB2A_MAX_ROTATE:-3}"

# max_rotate 必须是非负整数，否则去掉非数字字符；空则回退 3
case "$MAX_ROTATE" in
    ''|*[!0-9]*) MAX_ROTATE=$(printf '%s' "$MAX_ROTATE" | tr -cd '0-9'); [ -z "$MAX_ROTATE" ] && MAX_ROTATE=3 ;;
esac

mkdir -p "$AUTH_DIR"
mkdir -p "$(dirname "$STATE_FILE")"

cat > /app/config.json <<EOF
{
  "listen": "$(json_escape "$LISTEN")",
  "api_key": "$(json_escape "$API_KEY")",
  "auth_dir": "$(json_escape "$AUTH_DIR")",
  "state_file": "$(json_escape "$STATE_FILE")",
  "region": "$(json_escape "$REGION")",
  "strategy": "$(json_escape "$STRATEGY")",
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
