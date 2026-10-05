#!/bin/sh
# 在飞牛 NAS 上构建 workbuddy-wild 镜像。
#
# 用法（把整个 docker/ 目录传到 NAS 后，在该目录内执行）：
#     sh build-on-nas.sh
#
# 前提：本目录下必须有 Dockerfile 与三个 Linux 二进制：
#     workbuddy-wild-server / workbuddy-login / workbuddy-login-trae
# （这些文件由开发机交叉编译后随 docker/ 目录一起传上来）
set -e

cd "$(dirname "$0")"

IMAGE="${IMAGE:-workbuddy-wild:latest}"

echo "==> 检查所需文件"
missing=0
for f in Dockerfile entrypoint.sh login.sh login-trae.sh \
         workbuddy-wild-server workbuddy-login workbuddy-login-trae; do
    if [ -f "$f" ]; then
        printf "  OK       %s\n" "$f"
    else
        printf "  缺失     %s\n" "$f"
        missing=1
    fi
done
if [ "$missing" = "1" ]; then
    echo ""
    echo "错误：缺少文件，无法构建。请确认整个 docker/ 目录都已上传。"
    exit 1
fi

echo ""
echo "==> 检查二进制是否为 Linux x86-64"
for f in workbuddy-wild-server workbuddy-login workbuddy-login-trae; do
    magic=$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')
    if [ "$magic" != "7f454c46" ]; then
        echo "  警告：$f 不是 ELF 文件（magic=$magic）—— 可能传错或传输损坏"
    else
        echo "  OK       $f (ELF)"
    fi
done

echo ""
echo "==> 构建镜像 $IMAGE"
docker build -t "$IMAGE" .

echo ""
echo "==> 完成。镜像信息："
docker images "$IMAGE"

echo ""
echo "下一步："
echo "  1) 用 fnos-compose.yml 在「Docker → Compose」里创建项目（或直接 docker compose -f fnos-compose.yml up -d）"
echo "  2) 首次启动后登录账号："
echo "       docker compose run --rm workbuddy-wild /app/login.sh"
echo "       docker compose run --rm -e TRAE_DEVICE_ID=<16位设备号> workbuddy-wild /app/login-trae.sh"
echo "  3) docker compose restart workbuddy-wild"
