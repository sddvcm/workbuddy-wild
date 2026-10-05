#!/bin/sh
# ============================================================================
# workbuddy-wild 离线镜像一键导入脚本（在飞牛 NAS 或任意 Linux 上执行）
#
# 用法：
#   sh load-image.sh                     # 用当前目录下的 tar
#   sh load-image.sh /path/to/xxx.tar    # 指定 tar 路径
#
# 本脚本做的事：
#   1. 校验 tar 的 SHA256（若同目录有 SHA256SUMS）
#   2. docker load 导入镜像
#   3. 打印镜像信息与下一步操作
# ============================================================================
set -e

TAR="${1:-workbuddy-wild-v0.8.0-linux-amd64.tar}"
SUMS="SHA256SUMS"

if [ ! -f "$TAR" ]; then
    echo "✗ 找不到镜像文件：$TAR"
    echo "  请把 workbuddy-wild-v0.8.0-linux-amd64.tar 放到当前目录，或作为第一个参数指定路径"
    exit 1
fi

echo "==> 镜像文件：$TAR ($(du -h "$TAR" | cut -f1))"

# ---------- 1. 校验 SHA256 ----------
if [ -f "$SUMS" ]; then
    echo "==> 校验 SHA256 ..."
    if command -v sha256sum >/dev/null 2>&1; then
        if sha256sum -c "$SUMS" 2>/dev/null; then
            echo "    ✓ 校验通过"
        else
            echo "    ✗ 校验失败！文件可能损坏或被篡改，请重新拷贝"
            exit 1
        fi
    else
        echo "    (无 sha256sum，跳过校验)"
    fi
else
    echo "==> 未找到 $SUMS，跳过校验"
fi

# ---------- 2. 导入 ----------
echo "==> docker load 导入镜像（约需 10~30 秒）..."
docker load -i "$TAR"

# ---------- 3. 结果 ----------
echo ""
echo "============================================================"
docker images | grep -E "REPOSITORY|workbuddy-wild" || true
echo "============================================================"
echo ""
echo "✓ 导入完成。接下来："
echo ""
echo "  1) 准备数据目录："
echo "       mkdir -p /vol1/appdata/workbuddy-wild"
echo ""
echo "  2) 把 fnos-compose.yml 里的路径按需改好后，启动："
echo "       docker compose -f fnos-compose.yml up -d"
echo ""
echo "  3) 查看日志："
echo "       docker logs -f workbuddy-wild"
echo ""
echo "  4) 登录添加账号（WorkBuddy）："
echo "       docker compose -f fnos-compose.yml run --rm workbuddy-wild /app/login.sh"
echo ""
echo "  5) 登录添加账号（TraeWork，需先准备 16 位设备号）："
echo "       docker compose -f fnos-compose.yml run --rm \\"
echo "         -e TRAE_DEVICE_ID=你的设备号 workbuddy-wild /app/login-trae.sh"
echo ""
echo "  6) 验证："
echo "       curl http://127.0.0.1:7863/healthz"
echo ""
