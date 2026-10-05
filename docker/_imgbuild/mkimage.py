#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
把 alpine minirootfs + 应用文件 手工打包成 docker load 可导入的镜像 tar。

为什么手工构造：
  本机无 docker / WSL / podman，无法 docker build。但 docker load 需要的 tar
  结构非常简单稳定：
    <layer_id>/layer.tar        —— 文件系统层（tar，路径以 ./ 开头更保险）
    <config_hash>.json          —— 镜像 config（含 rootfs.diff_ids、config 字段）
    manifest.json               —— 指向 Config 与 Layers
    repositories                —— 旧式仓库名映射（docker load 也认）

  layer.tar 的 diff_id = sha256(未压缩的 layer.tar)，image_id = sha256(config json)。
"""
import hashlib
import io
import json
import os
import shutil
import stat as _stat
import sys
import tarfile
import time

ROOT = "C:/Users/Administrator/wbsync/docker/_imgbuild"
ROOTFS = os.path.join(ROOT, "rootfs")
ALPINE_TGZ = os.path.join(ROOT, "alpine.tar.gz")
APP = "C:/Users/Administrator/wbsync/docker"
OUT = os.path.join(ROOT, "out")
IMAGE_TAG = "workbuddy-wild:latest"
IMAGE_NAME = "workbuddy-wild"
IMAGE_TAG_PART = "latest"


def sha256_bytes(b: bytes) -> str:
    return hashlib.sha256(b).hexdigest()


def sha256_file(path: str) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def add_file(tf: tarfile.TarFile, arcname: str, data: bytes = None, src: str = None,
             mode: int = 0o644, uid=0, gid=0):
    """往 layer tar 里加一个普通文件。arcname 形如 './app/xxx'。"""
    if data is None:
        with open(src, "rb") as f:
            data = f.read()
    ti = tarfile.TarInfo(name=arcname)
    ti.size = len(data)
    ti.mode = mode
    ti.uid = uid
    ti.gid = gid
    ti.uname = "root"
    ti.gname = "root"
    ti.mtime = int(time.time())
    tf.addfile(ti, io.BytesIO(data))


def add_symlink(tf: tarfile.TarFile, arcname: str, target: str, mode: int = 0o777):
    ti = tarfile.TarInfo(name=arcname)
    ti.type = tarfile.SYMTYPE
    ti.linkname = target
    ti.mode = mode
    ti.uid = 0
    ti.gid = 0
    ti.uname = "root"
    ti.gname = "root"
    ti.mtime = int(time.time())
    tf.addfile(ti)


def add_dir(tf: tarfile.TarFile, arcname: str, mode: int = 0o755):
    ti = tarfile.TarInfo(name=arcname)
    ti.type = tarfile.DIRTYPE
    ti.mode = mode
    ti.uid = 0
    ti.gid = 0
    ti.uname = "root"
    ti.gname = "root"
    ti.mtime = int(time.time())
    tf.addfile(ti)


def main():
    if os.path.isdir(OUT):
        shutil.rmtree(OUT)
    os.makedirs(OUT)

    layer_tar_path = os.path.join(OUT, "layer.tar")

    # ---------- 1. 构造文件系统层 ----------
    print("[1/5] 构造 layer.tar ...")
    n = 0
    with tarfile.open(layer_tar_path, "w") as tf:
        # ★ 关键：直接从 alpine.tar.gz 流式读取原始条目，而不是从 Windows 解包目录读。
        #   原因：Windows 文件系统没有 Unix 执行位，从解包目录读 mode 会全部变成 0666，
        #   导致容器内 /bin/sh、/bin/busybox、/usr/bin/wget 全部不可执行 → 容器起不来。
        #   只有原始 tar 里才保留了正确的 0755。
        print("      读取 alpine 原始 tar（保留权限位）...")
        skip_top = {"dev", "proc", "sys", "run"}
        with tarfile.open(ALPINE_TGZ, "r:gz") as src:
            for m in src.getmembers():
                nm = m.name
                if nm in (".", "./"):
                    continue
                # nm 形如 "./bin/busybox" 或 "bin/busybox"，统一成 "./xxx"
                if not nm.startswith("./"):
                    nm = "./" + nm.lstrip("/")
                rel = nm[2:]
                top = rel.split("/")[0]
                if top in skip_top:
                    continue
                if m.isdir():
                    add_dir(tf, nm, mode=m.mode & 0o7777)
                elif m.issym():
                    add_symlink(tf, nm, m.linkname, mode=m.mode & 0o7777 or 0o777)
                elif m.islnk():
                    ti = tarfile.TarInfo(name=nm)
                    ti.type = tarfile.LNKTYPE
                    ti.linkname = m.linkname
                    ti.mode = m.mode & 0o7777
                    ti.uid = ti.gid = 0
                    ti.uname = ti.gname = "root"
                    ti.mtime = int(time.time())
                    tf.addfile(ti)
                elif m.isfile():
                    f = src.extractfile(m)
                    data = f.read() if f else b""
                    add_file(tf, nm, data=data, mode=m.mode & 0o7777)
                else:
                    continue
                n += 1
        print("      alpine 条目: %d" % n)

        # 1b. 补运行时挂载点目录（alpine 原始 tar 里已含 ./dev ./proc ./sys ./run，
        #     去重后只补 app/data 这两个我们自己用的目录）
        existing = set()
        for m in tf.getmembers():
            existing.add(m.name.rstrip("/"))
        for d in ("dev", "proc", "sys", "run", "app", "data"):
            if ("./" + d) in existing:
                continue
            add_dir(tf, "./" + d)
            existing.add("./" + d)
            n += 1
        if "./data/auths" not in existing:
            add_dir(tf, "./data/auths", mode=0o700)
            n += 1

        # 1c. 应用文件
        print("      叠加应用文件 ...")
        app_files = [
            ("workbuddy-wild-server", "/app/workbuddy-wild-server", 0o755),
            ("workbuddy-login", "/app/workbuddy-login", 0o755),
            ("workbuddy-login-trae", "/app/workbuddy-login-trae", 0o755),
            ("entrypoint.sh", "/app/entrypoint.sh", 0o755),
            ("login.sh", "/app/login.sh", 0o755),
            ("login-trae.sh", "/app/login-trae.sh", 0o755),
        ]
        for src_name, dst, mode in app_files:
            src = os.path.join(APP, src_name)
            if not os.path.isfile(src):
                print("  !! 缺失: " + src, file=sys.stderr)
                sys.exit(1)
            # 关键：shell 脚本必须 LF 行尾
            if src_name.endswith(".sh"):
                with open(src, "rb") as f:
                    data = f.read()
                if b"\r\n" in data:
                    data = data.replace(b"\r\n", b"\n")
                    print("      [fix] %s CRLF -> LF" % src_name)
                add_file(tf, "." + dst, data=data, mode=mode)
            else:
                add_file(tf, "." + dst, src=src, mode=mode)
            n += 1
        print("      layer 条目数: %d" % n)

    # ---------- 2. 计算 diff_id ----------
    print("[2/5] 计算 layer diff_id ...")
    diff_id = "sha256:" + sha256_file(layer_tar_path)
    layer_size = os.path.getsize(layer_tar_path)
    print("      diff_id=%s  size=%d" % (diff_id, layer_size))

    # ---------- 3. 构造 image config ----------
    print("[3/5] 构造 image config ...")
    config = {
        "architecture": "amd64",
        "os": "linux",
        "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "config": {
            "Hostname": "",
            "Domainname": "",
            "User": "",
            "AttachStdin": False,
            "AttachStdout": False,
            "AttachStderr": False,
            "Tty": False,
            "OpenStdin": False,
            "StdinOnce": False,
            "Env": [
                "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
                "WB2A_LISTEN=:7863",
                "WB2A_AUTH_DIR=/data/auths",
                "WB2A_STATE_FILE=/data/state.json",
                "WB2A_REGION=cn",
                "TZ=Asia/Shanghai",
            ],
            "Cmd": None,
            "Entrypoint": ["/app/entrypoint.sh"],
            "WorkingDir": "/app",
            "Labels": {},
            "ExposedPorts": {"7863/tcp": {}},
            "Volumes": {"/data": {}},
        },
        "container_config": {
            "Hostname": "",
            "Env": [
                "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            ],
            "Cmd": None,
            "Entrypoint": ["/app/entrypoint.sh"],
            "WorkingDir": "/app",
        },
        "rootfs": {
            "type": "layers",
            "diff_ids": [diff_id],
        },
        "history": [
            {
                "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "created_by": "ADD alpine-minirootfs-3.20.3-x86_64.tar.gz in /",
                "comment": "alpine:3.20 base",
            },
            {
                "created": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "created_by": "COPY workbuddy-wild-server workbuddy-login workbuddy-login-trae *.sh /app/",
                "comment": "workbuddy-wild offline bundle",
            },
        ],
    }
    config_bytes = json.dumps(config, separators=(",", ":"), sort_keys=True).encode()
    config_hash = sha256_bytes(config_bytes)
    config_name = config_hash + ".json"
    with open(os.path.join(OUT, config_name), "wb") as f:
        f.write(config_bytes)
    print("      config=%s" % config_name)

    # ---------- 4. layer 目录改名 & manifest ----------
    print("[4/5] 组装 manifest ...")
    layer_dir = os.path.join(OUT, diff_id.replace("sha256:", ""))
    os.makedirs(layer_dir)
    final_layer = os.path.join(layer_dir, "layer.tar")
    shutil.move(layer_tar_path, final_layer)

    manifest = [{
        # Config 必须是 <image_id>.json，image_id = sha256(config json)
        "Config": config_name,
        # ★ RepoTags 才是现代 docker load 用来恢复 tag 的字段（docker save 实际输出）。
        #   千万不要写成 Repositories —— 那是 v1.10 之前的老格式，现代 load 会忽略它。
        "RepoTags": [IMAGE_TAG],
        # Layers 是层目录 + /layer.tar，顺序必须与 rootfs.diff_ids 一致
        "Layers": ["%s/layer.tar" % diff_id.replace("sha256:", "")],
    }]
    with open(os.path.join(OUT, "manifest.json"), "w") as f:
        json.dump(manifest, f)

    # repositories 是 docker save 的历史遗留配套文件，值指向 layer 目录名。
    # 现代 docker load 主要靠 manifest.RepoTags；但带上它可兼容旧版 docker/podman。
    repositories = {IMAGE_NAME: {IMAGE_TAG_PART: diff_id.replace("sha256:", "")}}
    with open(os.path.join(OUT, "repositories"), "w") as f:
        json.dump(repositories, f)

    # ---------- 5. 打包最终镜像 tar ----------
    print("[5/5] 打包镜像 tar ...")
    tag_safe = IMAGE_TAG.replace(":", "-").replace("/", "-")
    final_tar = os.path.join(ROOT, "workbuddy-wild-v0.8.0-linux-amd64.tar")
    if os.path.exists(final_tar):
        os.remove(final_tar)
    with tarfile.open(final_tar, "w") as tf:
        for name in sorted(os.listdir(OUT)):
            tf.add(os.path.join(OUT, name), arcname=name)
    size = os.path.getsize(final_tar)
    print("")
    print("=" * 64)
    print("输出: %s" % final_tar)
    print("大小: %.2f MB" % (size / 1024 / 1024))
    print("SHA256: %s" % sha256_file(final_tar))
    print("=" * 64)


if __name__ == "__main__":
    main()
