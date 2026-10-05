#!/bin/sh
# WorkBuddy 交互式登录助手（在 NAS 上使用）。
#
# 用法（在 docker-compose.yml 所在目录）：
#   docker compose run --rm workbuddy-wild /app/login.sh
#
# 说明：登录 state 文件落在 /tmp，必须在「同一个容器会话」里一气呵成，
# 因此不能拆成两次 docker compose run。请按提示在本会话内完成。
set -e

AUTH_DIR="${WB2A_AUTH_DIR:-/data/auths}"
mkdir -p "$AUTH_DIR"

echo "==> 步骤1：获取授权 URL"
URL=$(/app/workbuddy-login url)
echo ""
echo "================================================================"
echo "请在浏览器打开以下地址并完成登录："
echo ""
echo "$URL"
echo "================================================================"
echo ""
printf "登录完成后，回到这里按回车继续..."
read _

echo "==> 步骤2：轮询登录结果"
OUT=$(/app/workbuddy-login poll) || { echo "登录未完成或被取消。"; exit 1; }
echo "登录返回：$OUT"

NOW=$(date +%s)
# 把 cmd/login 的 snake_case 输出转换为服务端 auth 嵌套形，并换算 expiresAt = now + expires_in
echo "$OUT" | jq -r --arg now "$NOW" '
  . as $in
  | ($in.expires_in // 0 | tonumber) as $exp
  | ($now | tonumber) as $nown
  | {
      auth: {
        accessToken:  ($in.access_token // ""),
        refreshToken: ($in.refresh_token // ""),
        expiresAt:    (($nown + $exp) | tonumber),
        domain:       ($in.domain // ""),
        apiHost:      "",
        machineId:    "",
        deviceId:     ""
      },
      account: {
        uid:          ($in.uid // ""),
        enterpriseId: ($in.enterprise_id // ""),
        nickname:     ($in.nickname // "")
      }
    }' > /tmp/wb_auth.json

UID_VAL=$(echo "$OUT" | jq -r '.uid // empty')
if [ -z "$UID_VAL" ]; then
  echo "错误：未获取到 uid，登录可能未完成。"
  rm -f /tmp/wb_auth.json
  exit 1
fi

DEST="$AUTH_DIR/workbuddy-$UID_VAL.json"
mv /tmp/wb_auth.json "$DEST"
echo "==> 已写入账号文件：$DEST"
echo "==> 重启服务以加载新账号：docker compose restart workbuddy-wild"
