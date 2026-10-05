#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
严格校验生成的镜像 tar 是否符合 docker load 的解析要求。

docker load 的核心校验（libimage / docker/daemon 的 image/tarexport/load.go）：
  1. tar 内必须有 manifest.json（新式）或 repositories（旧式）
  2. manifest[i].Config 指向的 <hash>.json 必须存在且能被解析为 image config
  3. config.rootfs.diff_ids 数量 必须 == manifest[i].Layers 数量
  4. 每个 layer 文件的 sha256（未压缩）必须 == 对应 diff_id
  5. config 必须含 architecture / os，否则平台识别失败
  6. 安全：manifest[i].Config / Layers 不能含 .. 或绝对路径（防目录穿越）
"""
import hashlib
import json
import os
import sys
import tarfile

TAR = "C:/Users/Administrator/wbsync/docker/_imgbuild/workbuddy-wild-v0.8.0-linux-amd64.tar"

ok = True


def fail(msg):
    global ok
    ok = False
    print("  ✗ " + msg)


def good(msg):
    print("  ✓ " + msg)


print("=" * 68)
print("镜像 tar 严格校验: " + os.path.basename(TAR))
print("=" * 68)

if not os.path.isfile(TAR):
    print("文件不存在"); sys.exit(1)

size = os.path.getsize(TAR)
h = hashlib.sha256()
with open(TAR, "rb") as f:
    for c in iter(lambda: f.read(1 << 20), b""):
        h.update(c)
tar_sha = h.hexdigest()
print("大小   : %.2f MB (%d bytes)" % (size / 1024 / 1024, size))
print("SHA256 : " + tar_sha)
print("")

# ---------- 打开 tar，列出顶层条目 ----------
with tarfile.open(TAR, "r") as tf:
    names = tf.getnames()
    print("[1] tar 顶层条目 (%d):" % len(names))
    for n in sorted(set(n.split("/")[0] for n in names)):
        print("      " + n)
    print("")

    # ---------- 2. manifest.json ----------
    print("[2] manifest.json")
    if "manifest.json" not in names:
        fail("缺少 manifest.json")
        sys.exit(1)
    mf = json.loads(tf.extractfile("manifest.json").read())
    if not isinstance(mf, list) or not mf:
        fail("manifest 不是非空数组")
        sys.exit(1)
    m = mf[0]
    good("manifest 是数组，长度 %d" % len(mf))
    for key in ("Config", "Layers"):
        if key not in m:
            fail("manifest[0] 缺 %s" % key)
    if "RepoTags" not in m:
        fail("manifest[0] 缺 RepoTags（docker load 靠它恢复 tag）")
    elif ":" not in m["RepoTags"][0]:
        fail("RepoTags[0] 不含 tag: %s" % m["RepoTags"][0])
    else:
        good("RepoTags = %s" % m["RepoTags"])
    if "Repositories" in m:
        fail("manifest[0] 含旧的 Repositories 字段（应改用 RepoTags）")
    cfg_path = m.get("Config", "")
    layers = m.get("Layers", [])
    print("      Config = %s" % cfg_path)
    print("      Layers = %s" % layers)

    # ---------- 3. 路径穿越检查 ----------
    print("")
    print("[3] 路径安全（防目录穿越）")
    for p in [cfg_path] + list(layers):
        if p.startswith("/") or ".." in p.split("/"):
            fail("危险路径: %s" % p)
    if ok:
        good("Config/Layers 路径均安全")

    # ---------- 4. config json ----------
    print("")
    print("[4] image config")
    if cfg_path not in names:
        fail("config 文件 %s 不在 tar 内" % cfg_path)
        sys.exit(1)
    cfg = json.loads(tf.extractfile(cfg_path).read())
    for key in ("architecture", "os", "config", "rootfs"):
        if key not in cfg:
            fail("config 缺 %s" % key)
    if ok:
        good("config 含必要字段")
    print("      architecture = %s" % cfg.get("architecture"))
    print("      os           = %s" % cfg.get("os"))
    print("      Entrypoint   = %s" % cfg.get("config", {}).get("Entrypoint"))
    print("      WorkingDir   = %s" % cfg.get("config", {}).get("WorkingDir"))
    print("      Env          = %s" % cfg.get("config", {}).get("Env"))
    print("      ExposedPorts = %s" % cfg.get("config", {}).get("ExposedPorts"))
    print("      Volumes      = %s" % cfg.get("config", {}).get("Volumes"))

    diff_ids = cfg.get("rootfs", {}).get("diff_ids", [])
    print("      diff_ids     = %s" % diff_ids)

    # ---------- 5. diff_ids 数量匹配 ----------
    print("")
    print("[5] diff_ids 与 Layers 数量匹配")
    if len(diff_ids) != len(layers):
        fail("diff_ids=%d 但 Layers=%d" % (len(diff_ids), len(layers)))
    else:
        good("数量一致 (%d)" % len(diff_ids))

    # ---------- 6. 逐层 sha256 校验 ----------
    print("")
    print("[6] 逐层 sha256(未压缩) == diff_id")
    for i, (lp, did) in enumerate(zip(layers, diff_ids)):
        if lp not in names:
            fail("layer 文件不存在: %s" % lp)
            continue
        raw = tf.extractfile(lp).read()
        actual = "sha256:" + hashlib.sha256(raw).hexdigest()
        if actual != did:
            fail("layer[%d] 哈希不匹配\n      期望 %s\n      实际 %s" % (i, did, actual))
        else:
            good("layer[%d] %s  (%d bytes) -> %s" % (i, lp, len(raw), actual[:19] + "..."))

    # ---------- 7. 层内文件清单抽查 ----------
    print("")
    print("[7] 层内关键文件检查")
    import io
    layer0 = tf.extractfile(layers[0]).read()
    with tarfile.open(fileobj=io.BytesIO(layer0), mode="r:") as lt:
        # 内层 tar 名字是 "./app/xxx"，建一张 去前缀 的映射表
        name_map = {}
        for mem in lt.getmembers():
            key = mem.name.lstrip("./")
            name_map[key] = mem
        lnames = set(name_map.keys())

        def read_inner(path):
            mem = name_map.get(path)
            if mem is None:
                return None
            f = lt.extractfile(mem)
            return f.read() if f else None

        need = [
            "app/workbuddy-wild-server",
            "app/workbuddy-login",
            "app/workbuddy-login-trae",
            "app/entrypoint.sh",
            "app/login.sh",
            "app/login-trae.sh",
            "etc/ssl/certs/ca-certificates.crt",
            "bin/busybox",
            "bin/sh",
            "usr/bin/wget",
            "etc/passwd",
        ]
        for n in need:
            if n in lnames:
                good("存在 " + n)
            else:
                fail("缺失 " + n)

        # shell 脚本必须是 LF 行尾
        print("")
        print("[8] shell 脚本行尾检查（必须 LF）")
        for s in ("app/entrypoint.sh", "app/login.sh", "app/login-trae.sh"):
            data = read_inner(s)
            if data is None:
                fail("读不到 " + s); continue
            if b"\r\n" in data:
                fail("%s 含 CRLF（容器内会 bad interpreter）" % s)
            else:
                good("%s 为 LF" % s)
            if data[:2] != b"#!":
                fail("%s 缺 shebang" % s)

        # 二进制 ELF + 权限
        print("")
        print("[9] 二进制 ELF magic 与权限")
        for b in ("app/workbuddy-wild-server", "app/workbuddy-login", "app/workbuddy-login-trae"):
            ti = name_map[b]
            data = read_inner(b)
            magic = data[:4]
            perm = oct(ti.mode & 0o777)
            if magic != b"\x7fELF":
                fail("%s 非 ELF" % b)
            if perm != "0o755":
                fail("%s 权限为 %s（应为 0o755）" % (b, perm))
            if magic == b"\x7fELF" and perm == "0o755":
                good("%s ELF 且 0o755" % b)

        # 管理面板资源是否已内嵌进二进制（搜特征字符串）
        print("")
        print("[10] 管理面板资源内嵌检查")
        srv = read_inner("app/workbuddy-wild-server")
        for needle, label in [
            (b"workbuddy-wild", "面板 HTML 标题"),
            (b"/admin/", "面板路由路径"),
            (b"window.__CSRF__", "面板 CSRF 注入点"),
            (b"/admin/style.css", "面板样式表引用"),
            (b"/admin/favicon.svg", "面板图标引用"),
            (b"X-Admin-Token", "面板鉴权头"),
            (b"/login/workbuddy/start", "WorkBuddy 登录接口"),
            (b"/login/traework/start", "TraeWork 登录接口"),
        ]:
            if needle in srv:
                good("内嵌 " + label)
            else:
                fail("二进制里找不到 " + label + " (" + needle.decode() + ")")

print("")
print("=" * 68)
if ok:
    print("结论：✅ 全部通过，该 tar 可被 docker load 直接导入")
else:
    print("结论：❌ 存在问题，需修复")
print("=" * 68)
sys.exit(0 if ok else 1)
