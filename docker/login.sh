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

echo "==> 步骤2：轮询登录结果并直接落盘"
# login auth <authDir> 内部完成「换 token → 查 account → 写 auth 嵌套形文件」，
# stdout 只输出写入的绝对路径。这样容器里就不需要 jq 了。
DEST=$(/app/workbuddy-login auth "$AUTH_DIR") || { echo "登录未完成或失败。"; exit 1; }
echo "==> 已写入账号文件：$DEST"
echo "==> 重启服务以加载新账号：docker compose restart workbuddy-wild"
