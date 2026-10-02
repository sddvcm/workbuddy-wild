#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
全量模型可用性探测 v2 —— 抓正文，识别「HTTP 200 + 错误文本」的伪成功。

背景（实测）:
  * workbuddy 上游对不存在的模型返回 HTTP 400 + code=11102 → 服务转为 400 model_not_available。
  * traework 上游对不存在的模型返回 **HTTP 200**，把错误塞进正文:
        "solo error code=4001 msg=We're sorry, the param is invalid..."
    → 只看状态码会把这种模型误判为「可用」。
  * 外部供应商（buddy.ai-cybernetics.com）余额耗尽时同样返回 HTTP 200 + 充值提示文本。

因此判定必须基于正文内容。

用法:
  python probe_models.py [--models <models.json>] [--out <report.json>] [--only workbuddy|traework]
"""
import argparse
import json
import re
import socket
import time
import urllib.error
import urllib.request

ENDPOINT = "http://127.0.0.1:7863/v1/chat/completions"
STATUS = "http://127.0.0.1:7863/status"
DEFAULT_KEY = "WorkBuddy2API"
PROMPT = "Reply with exactly one word: pong"

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# 正文里出现这些片段 → 不是有效回答
BIZ_ERROR_PATTERNS = [
    (re.compile(r"solo\s+error", re.I), "traework 参数/模型无效"),
    (re.compile(r"the param is invalid", re.I), "traework 参数无效"),
    (re.compile(r"积分不足|余额不足|积分余额不足|额度不足|额度用尽|积分用完"), "余额/权益不足"),
    (re.compile(r"insufficient\s+(credit|balance|quota)", re.I), "余额/权益不足"),
    (re.compile(r"请(前往|到).{0,20}充值"), "要求充值"),
]


def api_key_from(models_path):
    try:
        with open(models_path, encoding="utf-8") as f:
            for m in json.load(f):
                if m.get("apiKey") and str(m.get("url", "")).startswith("http://127.0.0.1"):
                    return m["apiKey"]
    except Exception:
        pass
    return DEFAULT_KEY


def pool_health():
    try:
        req = urllib.request.Request(STATUS, headers={"Authorization": "Bearer " + KEY})
        j = json.load(OPENER.open(req, timeout=15))
    except Exception as e:
        return True, "status-error:%s" % e
    alive, cooling = 0, []
    for plat, accts in j.get("accounts", {}).items():
        for a in accts:
            if a.get("cooling") or a.get("disabled"):
                cooling.append("%s/%s" % (plat, a.get("nickname") or a["uid"][:6]))
            else:
                alive += 1
    return alive > 0, "alive=%d cooling=%d %s" % (alive, len(cooling), ",".join(cooling[:4]))


def read_sse_content(r, timeout):
    """读 SSE，返回 (content, reasoning, raw_snippet)。"""
    content, reasoning, raw = "", "", ""
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            chunk = r.read(2048)
        except Exception:
            break
        if not chunk:
            break
        raw += chunk.decode("utf-8", "replace")
        if len(raw) > 20000:
            raw = raw[-20000:]
        for seg in raw.split("data:"):
            seg = seg.strip()
            if not seg or seg == "[DONE]":
                continue
            try:
                o = json.loads(seg)
            except Exception:
                continue
            for ch in o.get("choices", []):
                d = ch.get("delta") or {}
                if isinstance(d.get("content"), str):
                    content += d["content"]
                if isinstance(d.get("reasoning_content"), str):
                    reasoning += d["reasoning_content"]
        if "[DONE]" in raw and (content or reasoning):
            break
        if content and len(content) > 200:
            break
    return content, reasoning, raw[:400].replace("\n", " ")


def classify_content(content, reasoning):
    """返回 (verdict, detail)。"""
    blob = content or reasoning or ""
    if not blob.strip():
        return "fail", "空正文（status 200 但没有任何 delta 内容）"
    for pat, label in BIZ_ERROR_PATTERNS:
        m = pat.search(blob)
        if m:
            return "biz_error", "%s | %s" % (label, blob[:120].replace("\n", " "))
    return "ok", blob[:80].replace("\n", " ")


def probe_one(ep, key, model, timeout=90, tries=3):
    payload = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "stream": True,
        "max_tokens": 32,
    }).encode()

    last = ("fail", "no attempt")
    for _ in range(tries):
        req = urllib.request.Request(ep, data=payload, headers={
            "Authorization": "Bearer " + key,
            "Content-Type": "application/json",
            "Accept": "text/event-stream",
        })
        try:
            r = OPENER.open(req, timeout=timeout)
            status = r.status
            content, reasoning, raw = read_sse_content(r, timeout)
            r.close()
            verdict, detail = classify_content(content, reasoning)
            if verdict == "ok" or verdict == "biz_error":
                return verdict, detail
            last = (verdict, detail)
        except urllib.error.HTTPError as e:
            body = e.read(800).decode("utf-8", "replace")
            code = ""
            try:
                code = json.loads(body)["error"]["code"]
            except Exception:
                pass
            if e.code == 400 and code == "model_not_available":
                return "bad_model", body[:180].replace("\n", " ")
            if e.code == 400 and code == "invalid_model":
                return "bad_model", body[:180].replace("\n", " ")
            if e.code in (401, 403):
                return "auth_error", "http %d %s" % (e.code, body[:140].replace("\n", " "))
            if e.code == 503 or code == "no_healthy_account":
                last = ("fail", "账号池不可用: " + body[:140].replace("\n", " "))
                time.sleep(10)
                continue
            last = ("fail", "http %d code=%s %s" % (e.code, code, body[:140].replace("\n", " ")))
        except (socket.timeout, TimeoutError):
            last = ("fail", "timeout %ss" % timeout)
        except Exception as e:
            last = ("fail", "%s: %s" % (type(e).__name__, e))
        time.sleep(2)
    return last


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--models", default=r"C:/Users/Administrator/Desktop/models.json")
    ap.add_argument("--out", default=r"C:/Users/Administrator/wbsync/tools/model-probe-report.json")
    ap.add_argument("--only", default="")
    args = ap.parse_args()

    global KEY
    KEY = api_key_from(args.models)

    with open(args.models, encoding="utf-8") as f:
        models = json.load(f)

    targets = []
    for m in models:
        mid = m.get("id") or ""
        if not mid:
            continue
        if m.get("url", "").startswith("http://127.0.0.1"):
            if args.only and not mid.startswith(args.only + "/"):
                continue
            ep = ENDPOINT
            key = KEY
        else:
            # 外部供应商：直连它自己的 endpoint
            ep = m["url"].rstrip("/") + "/chat/completions"
            key = m.get("apiKey", "")
        targets.append((mid, ep, key, m))

    print("待测 %d 个模型" % len(targets), flush=True)
    results = []
    for i, (mid, ep, key, m) in enumerate(targets, 1):
        if ep == ENDPOINT:
            ok, summary = pool_health()
            if not ok:
                print("!! 账号池全冷却，等 60s (%s)" % summary, flush=True)
                time.sleep(60)
        t0 = time.time()
        verdict, detail = probe_one(ep, key, mid)
        dt = round(time.time() - t0, 1)
        results.append({"id": mid, "verdict": verdict, "detail": detail, "seconds": dt})
        print("[%2d/%2d] %-42s %-11s %5.1fs  %s" % (i, len(targets), mid, verdict, dt, detail[:90]), flush=True)
        with open(args.out, "w", encoding="utf-8") as f:
            json.dump(results, f, ensure_ascii=False, indent=2)
        time.sleep(0.4)

    print("\n===== 汇总 =====")
    for v in ("ok", "biz_error", "bad_model", "auth_error", "fail"):
        ids = [r["id"] for r in results if r["verdict"] == v]
        if ids:
            print("%-10s %d: %s" % (v, len(ids), ", ".join(ids)))
    print("报告: %s" % args.out)


if __name__ == "__main__":
    main()
