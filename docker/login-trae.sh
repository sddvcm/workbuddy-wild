#!/bin/sh
# TraeWork 交互式登录助手（在 NAS 上使用）。
#
# 用法（确保 ./data 已挂载，且已 cd 到 docker-compose.yml 所在目录）：
#   docker compose run --rm \
#     -e TRAE_DEVICE_ID=4484256452647802 \
#     workbuddy-wild /app/login-trae.sh
#
# ★ 为什么必须提供 TRAE_DEVICE_ID
#   Trae 服务端按「注册指纹」校验 device_id：随机值会让每日签到恒返 9074 失败。
#   容器里读不到你 Windows 上 Trae 客户端的 storage.json，所以要手动传进来。
#   取值方法（在装过 Trae 客户端的 Windows 电脑上）：
#     1) 打开目录 %APPDATA%\TRAE SOLO CN\User\globalStorage\
#     2) 用记事本打开 storage.json，搜索  iCubeAuthInfo://icube-dc:
#     3) 冒号后面那串 16 位数字就是设备号，例如 4484256452647802
#   （试用版 TRAE SOLO CN / Trae CN / Trae 都可能在，哪个存在用哪个）
#
# 为什么是「两段式」而不是全自动
#   桌面端能全自动是因为浏览器和程序在同一台机器、能访问 127.0.0.1 回调。
#   容器里的 127.0.0.1 你访问不到，所以改成：生成 URL → 你浏览器登录 →
#   复制地址栏里那个「打不开的页面」的完整 URL → 粘贴回来换 token。
set -e

AUTH_DIR="${WB2A_AUTH_DIR:-/data/auths}"
mkdir -p "$AUTH_DIR"

DEV_ID="${TRAE_DEVICE_ID:-}"
if [ -z "$DEV_ID" ]; then
    echo "⚠️  未设置 TRAE_DEVICE_ID —— 签到可能因设备校验失败返回 9074。"
    echo "    取值方法见本脚本头部注释；如暂时不签到，可忽略继续。"
    echo ""
    printf "是否继续？[y/N] "
    read _cont
    case "$_cont" in
        y|Y) ;;
        *) echo "已取消。"; exit 1 ;;
    esac
fi

echo "==> 步骤1：生成授权 URL"
if [ -n "$DEV_ID" ]; then
    URL=$(/app/workbuddy-login-trae -auth-dir "$AUTH_DIR" -device-id "$DEV_ID" url)
else
    URL=$(/app/workbuddy-login-trae -auth-dir "$AUTH_DIR" url)
fi

echo ""
echo "================================================================"
echo "请在你电脑的浏览器里打开下面的地址，并完成 Trae 登录："
echo ""
echo "$URL"
echo "================================================================"
echo ""
echo "登录完成后，浏览器会跳到一个打不开的页面（容器内的 127.0.0.1）。"
echo "这是正常的 —— 请把地址栏里的**完整 URL 整段复制**下来。"
echo ""
printf "粘贴完整回调 URL 后按回车："
read CB_URL

if [ -z "$CB_URL" ]; then
    echo "错误：回调 URL 为空。"
    exit 1
fi

echo "==> 步骤2：用回调 URL 换取凭证"
/app/workbuddy-login-trae -auth-dir "$AUTH_DIR" poll "$CB_URL"

echo ""
echo "==> 重启服务以加载新账号："
echo "    docker compose restart workbuddy-wild"
