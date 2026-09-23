// WorkBuddy-Wild 托盘面板逻辑（原生 JS，无构建步骤）
/* global window */
"use strict";

const Go = window.go.app.App;
const rt = window.runtime;

let state = null;
let loginTimer = null;
let loginURL = "";
let liveTimer = null;

const $ = (id) => document.getElementById(id);

function esc(s) {
  return String(s ?? "").replace(/[&<>"']/g, (c) => (
    { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]
  ));
}

// 监听主机默认值（host 为空 = 全部接口，展示为本机可达地址）
function displayHost(host) {
  if (!host || host === "0.0.0.0" || host === "::") return "127.0.0.1";
  return host;
}

// ---------------------------------------------------------------------------
// 初始加载与渲染
// ---------------------------------------------------------------------------

async function load() {
  try {
    state = await Go.GetState();
  } catch (e) {
    console.error("GetState failed", e);
    return;
  }
  render();
}

function render() {
  $("ver").textContent = state.version;
  $("serverLine").textContent =
    `http://${displayHost(state.listen_host)}:${state.listen_port} · ${state.running ? "运行中" : "未监听"}`;
  $("acctCount").textContent = `(${state.accounts.length})`;
  $("nextCheckin").textContent = state.next_checkin || "-";
  $("acctEmpty").classList.toggle("hidden", state.accounts.length > 0);
  $("inPort").value = state.listen_port;
  $("chkAutostart").checked = state.autostart;
  $("selStrategy").value = state.strategy || "credits";
  renderStrategyDesc();
  renderKey();
  renderHostSelect();
  renderAccounts();
  renderHours();
}

const STRATEGY_DESC = {
  credits: "使用剩余积分最多的账号，尽量摊薄消耗。",
  expire: "优先消耗最快过期的凭证，减少 token 到期的浪费。",
  roundrobin: "按顺序轮流使用每个账号，调用次数均衡分配。",
};

function renderStrategyDesc() {
  const s = $("selStrategy").value;
  $("strategyDesc").textContent = STRATEGY_DESC[s] || "";
  $("strategyHint").textContent = "策略：" + strategyLabel(s);
}

function strategyLabel(s) {
  return { credits: "优先积分", expire: "优先过期", roundrobin: "负载均衡" }[s] || "优先积分";
}

function renderKey() {
  const v = state.api_key;
  $("keyVal").textContent = v ? v : "（未设置）";
  $("keyVal").classList.toggle("muted", !v);
  $("keyBox").classList.remove("hidden");
  $("keyEdit").classList.add("hidden");
}

function editKey() {
  $("keyInput").value = state.api_key || "";
  $("keyInput").dataset.orig = state.api_key || "";
  $("keyBox").classList.add("hidden");
  $("keyEdit").classList.remove("hidden");
  $("keyInput").focus();
}

function commitKey() {
  const key = $("keyInput").value.trim();
  if (key === $("keyInput").dataset.orig) { renderKey(); return; }
  Go.SetAPIKey(key).then(() => {
    state.api_key = key;
    renderKey();
    toast("API-Key 已保存");
  }).catch((e) => { toast("保存失败：" + e); renderKey(); });
}

function cancelKey() { renderKey(); }

function renderHostSelect() {
  const sel = $("selHost");
  const host = state.listen_host || "";
  if (host === "127.0.0.1") sel.value = "127.0.0.1";
  else if (host === "0.0.0.0" || host === "") sel.value = "0.0.0.0";
  else {
    sel.value = "__custom__";
    $("inHost").value = host;
    $("customHostRow").classList.remove("hidden");
  }
}

function renderAccounts() {
  const box = $("acctList");
  if (!state || !state.accounts) return;
  renderTotal();

  // 排序：正在调用 > 最近用过 > 其余（按积分降序），让活跃账号浮到顶部。
  const accts = state.accounts.slice().sort((a, b) => {
    if (a.in_use !== b.in_use) return a.in_use ? -1 : 1;
    const au = a.last_used_at ? 1 : 0, bu = b.last_used_at ? 1 : 0;
    if (au !== bu) return bu - au;
    return (b.credits || 0) - (a.credits || 0);
  });
  box.innerHTML = accts.map((a) => {
    const name = a.nickname || a.uid;
    const credits = a.credits ? Number(a.credits).toLocaleString() : "0";
    const status = accountStatus(a);
    const group = a.group || "workbuddy";
    const icon = group === "traework" ? "T" : "W";
    const iconCls = group === "traework" ? "icon-trae" : "icon-wb";
    const live = a.in_use
      ? `<span class="live-badge"><span class="dot"></span>调用中</span>`
      : (a.last_used_at ? `<span class="used-badge" title="最近调用 ${esc(a.last_used_at)}">最近用过</span>` : "");
    return `<div class="acct${a.in_use ? " in-use" : ""}">
      <div class="acct-row1">
        <span class="acct-icon ${iconCls}" title="${group === "traework" ? "TraeWork" : "WorkBuddy"}">${icon}</span>
        <span class="acct-name" title="${esc(name)}">${esc(name)}</span>
        ${live}
        <span class="acct-credits">${credits}</span>
      </div>
      <div class="acct-row2">
        <span class="acct-status ${status.cls}">${esc(status.txt)}</span>
        <button class="icon-del" data-action="remove" data-uid="${esc(a.uid)}" title="删除账号">
          <svg viewBox="0 0 24 24" width="13" height="13" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/></svg>
        </button>
      </div>
    </div>`;
  }).join("");
}

// renderTotal 计算所有账号积分总和（实时更新）。
// 变化时给数字加一次性动画，让"实时更新"看得见。
let lastTotal = null;
function renderTotal() {
  const el = $("totalCredits");
  const subEl = $("totalSub");
  if (!el || !state || !state.accounts) return;
  const accts = state.accounts;
  const sum = accts.reduce((acc, a) => acc + (Number(a.credits) || 0), 0);
  const shown = sum.toLocaleString();
  if (el.textContent !== shown) {
    el.textContent = shown;
    if (lastTotal !== null && sum > lastTotal) {
      el.classList.remove("bump");
      void el.offsetWidth; // 强制重排以重启动画
      el.classList.add("bump");
    }
    lastTotal = sum;
  }
  // 副标题：可用账号数 / 总数；有冷却或禁用时明确提示
  const total = accts.length;
  const usable = accts.filter((a) => !a.disabled && !a.cooling).length;
  if (total === 0) {
    subEl.textContent = "";
  } else if (usable === total) {
    subEl.textContent = `${total} 个账号`;
  } else {
    subEl.textContent = `${usable}/${total} 个可用`;
  }
}

function accountStatus(a) {
  if (a.disabled) return { cls: "err", txt: "已禁用（session 失效，需重新登录）" };
  if (a.cooling) {
    const until = a.until ? ` 至 ${a.until}` : "";
    return { cls: "warn", txt: `冷却中（${a.reason || "余额不足"}${until}）` };
  }
  const used = a.last_used_at ? ` · 调用 ${a.last_used_at}` : "";
  if (a.last_checkin_at) {
    const icon = a.last_checkin_ok ? "✓" : "✗";
    const cls = a.last_checkin_ok ? "ok" : "err";
    return { cls, txt: `${icon} 签到 ${a.last_checkin_at}${used}` };
  }
  return { cls: "", txt: "尚未签到" + used };
}

function renderHours() {
  const box = $("hoursBox");
  // 新格式支持分钟；旧后端只返回 checkin_hours 时仍兼容整点显示。
  const times = state.checkin_times || (state.checkin_hours || []).map((h) => String(h).padStart(2, "0") + ":00");
  state.checkin_times = times.slice();
  box.innerHTML = times.map((v, i) => {
    return `<span class="hour-chip">
      <input type="time" value="${v}" step="60">
      <button class="del" data-i="${i}" title="删除">×</button>
    </span>`;
  }).join("");
  // 变更即生效（无保存按钮）
  box.querySelectorAll("input[type=time]").forEach((inp) => {
    inp.onchange = commitHours;
  });
  box.querySelectorAll(".del").forEach((btn) => {
    btn.onclick = () => {
      state.checkin_times.splice(Number(btn.dataset.i), 1);
      renderHours();
      commitHours();
    };
  });
}

function collectHours() {
  const out = [];
  $("hoursBox").querySelectorAll("input[type=time]").forEach((inp) => {
    const m = /^(\d{1,2}):(\d{2})/.exec(inp.value || "");
    if (m) out.push(`${String(parseInt(m[1], 10)).padStart(2, "0")}:${m[2]}`);
  });
  return [...new Set(out)].sort();
}

function commitHours() {
  const times = collectHours();
  if (times.length === 0) { toast("至少保留一个时间"); renderHours(); return; }
  Go.SetCheckinTimes(times).then(() => {
    state.checkin_times = times;
    toast("签到时间已保存");
  }).catch((e) => { toast("保存失败：" + e); renderHours(); });
}

// 监听地址（主机 + 端口）变更即生效
function commitListen() {
  let host = $("selHost").value;
  if (host === "__custom__") {
    host = $("inHost").value.trim();
    if (!host) { toast("请输入自定义主机"); render(); return; }
  }
  const port = parseInt($("inPort").value, 10);
  if (!port || port < 1 || port > 65535) { toast("端口无效"); render(); return; }
  if (host === state.listen_host && port === state.listen_port) return; // 未变化
  Go.SetListen(host, port).then(() => {
    state.listen_host = host;
    state.listen_port = port;
    state.running = true;
    render();
    toast("监听已生效");
  }).catch((e) => {
    toast("切换失败：" + e);
    render(); // 回退到当前状态
  });
}

// ---------------------------------------------------------------------------
// Toast
// ---------------------------------------------------------------------------

let toastTimer = null;
function toast(msg) {
  const t = $("toast");
  t.textContent = msg;
  t.classList.remove("hidden");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.add("hidden"), 2600);
}

// ---------------------------------------------------------------------------
// 登录流程
// ---------------------------------------------------------------------------

async function startLogin(kind) {
  let url = "";
  try {
    url = await Go.StartLoginFor(kind || "workbuddy");
  } catch (e) {
    toast("发起登录失败：" + e);
    return;
  }
  loginURL = url;
  $("loginOverlay").classList.remove("hidden");
  $("loginMsg").textContent = "已打开无痕浏览器，请在浏览器中完成 " + (kind === "traework" ? "TraeWork" : "WorkBuddy") + " 登录…";
  let remain = 300;
  $("loginCountdown").textContent = `剩余 ${Math.floor(remain / 60)}:${String(remain % 60).padStart(2, "0")}`;
  clearInterval(loginTimer);
  loginTimer = setInterval(() => {
    remain -= 1;
    if (remain < 0) { clearInterval(loginTimer); return; }
    $("loginCountdown").textContent = `剩余 ${Math.floor(remain / 60)}:${String(remain % 60).padStart(2, "0")}`;
  }, 1000);
}

function closeLoginOverlay() {
  clearInterval(loginTimer);
  loginTimer = null;
  $("loginOverlay").classList.add("hidden");
}

function handleLoginEvent(e) {
  $("loginMsg").textContent = e.msg || "";
  if (e.phase === "success" || e.phase === "failed" || e.phase === "cancelled") {
    closeLoginOverlay();
    if (e.phase === "success") toast(e.msg);
    load();
  }
}

async function copyLoginURL() {
  if (!loginURL) return;
  try {
    await rt.ClipboardSetText(loginURL);
    toast("授权链接已复制");
  } catch (e) {
    try {
      await navigator.clipboard.writeText(loginURL);
      toast("授权链接已复制");
    } catch (e2) {
      toast("复制失败：" + e2);
    }
  }
}

// ---------------------------------------------------------------------------
// 事件绑定
// ---------------------------------------------------------------------------

function bind() {
  $("btnMin").onclick = () => Go.HidePanel();
  // 右上角关闭 + 底部退出：都先询问（关闭后自动签到停止）
  const confirmQuit = () => askConfirm("关闭程序", "确定关闭 WorkBuddy-Wild？\n关闭后自动签到将停止。", () => Go.QuitAll());
  $("btnClose").onclick = confirmQuit;
  $("btnQuit").onclick = confirmQuit;
  $("btnAddWB").onclick = () => startLogin("workbuddy");
  $("btnAddTrae").onclick = () => startLogin("traework");
  $("btnCopyUrl").onclick = copyLoginURL;
  $("btnCancelLogin").onclick = async () => {
    await Go.CancelLogin();
    closeLoginOverlay();
  };
  $("btnLog").onclick = async () => {
    try {
      await Go.OpenLogFile();
      toast("已用记事本打开日志");
    } catch (e) { toast("打开日志失败：" + e); }
  };
  $("btnAbout").onclick = () => {
    $("aboutVersion").textContent = state?.version || "-";
    $("aboutOverlay").classList.remove("hidden");
  };
  $("btnAboutClose").onclick = () => $("aboutOverlay").classList.add("hidden");
  $("aboutOverlay").onclick = (e) => {
    if (e.target === $("aboutOverlay")) $("aboutOverlay").classList.add("hidden");
  };
  $("aboutProject").onclick = (e) => {
    e.preventDefault();
    rt.BrowserOpenURL("https://github.com/sddvcm/workbuddy-wild");
  };

  // 深色/浅色手动切换（localStorage 记忆；默认浅色，不跟随系统）
  $("btnTheme").onclick = toggleTheme;

  // 积分策略：选择即生效（后端写入 config.json）
  $("selStrategy").onchange = async () => {
    const val = $("selStrategy").value;
    try {
      await Go.SetStrategy(val);
      state.strategy = val;
      renderStrategyDesc();
      toast("积分策略已切换：" + strategyLabel(val));
    } catch (e) {
      toast("切换失败：" + e);
      $("selStrategy").value = state.strategy || "credits";
    }
  };

  // API-Key：点击明文值进入编辑，blur/Enter 即生效，Esc 取消
  $("keyVal").onclick = editKey;
  $("keyInput").addEventListener("blur", commitKey);
  $("keyInput").addEventListener("keydown", (e) => {
    if (e.key === "Enter") { e.preventDefault(); commitKey(); }
    if (e.key === "Escape") cancelKey();
  });

  // 自动签到时间：变更即生效；增加时间
  $("btnAddHour").onclick = () => {
    if ((state.checkin_times || []).length >= 4) { toast("最多 4 个时间"); return; }
    state.checkin_times.push("09:00");
    renderHours();
    commitHours();
  };

  // API 监听：选择/失焦即生效
  $("selHost").onchange = () => {
    $("customHostRow").classList.toggle("hidden", $("selHost").value !== "__custom__");
    if ($("selHost").value === "__custom__") {
      $("inHost").focus();
    } else {
      commitListen();
    }
  };
  $("inPort").addEventListener("change", commitListen);
  $("inPort").addEventListener("blur", commitListen);
  $("inHost").addEventListener("change", commitListen);

  $("chkAutostart").onchange = async (e) => {
    try { await Go.SetAutostart(e.target.checked); }
    catch (err) { toast("设置失败：" + err); e.target.checked = !e.target.checked; }
  };

  $("btnCheckinAll").onclick = async () => {
    $("btnCheckinAll").disabled = true;
    try {
      const res = await Go.CheckinAll();
      const ok = res.filter((r) => r.ok).length;
      const failed = res.filter((r) => !r.ok);
      const detail = failed.length ? `；失败：${failed.map((r) => `${r.uid}: ${r.msg || "未知错误"}`).join("；")}` : "";
      toast(`签到完成：成功 ${ok}/${res.length}${detail}`);
    } catch (e) { toast("签到失败：" + e); }
    finally { $("btnCheckinAll").disabled = false; }
  };

  $("btnRefreshAll").onclick = async () => {
    $("btnRefreshAll").disabled = true;
    try { await Go.RefreshAll(); toast("积分已刷新"); }
    finally { $("btnRefreshAll").disabled = false; }
  };

  $("acctList").onclick = (e) => {
    const btn = e.target.closest("button[data-action]");
    if (!btn) return;
    const uid = btn.dataset.uid;
    const action = btn.dataset.action;
    if (action === "remove") {
      const name = uid.length > 20 ? uid.slice(0, 20) + "…" : uid;
      askConfirm("删除账号", "确定删除账号 " + name + "？\n（auth 文件将一并删除）", async () => {
        await Go.RemoveAccount(uid);
        toast("账号已删除");
        load();
      });
    }
  };

  // 通用确认弹层（删除账号 / 关闭程序）
  const btnConfirmOk = $("btnConfirmOk");
  const btnConfirmCancel = $("btnConfirmCancel");
  let pendingConfirm = null;

  function askConfirm(title, msg, okFn) {
    $("confirmTitle").textContent = title;
    $("confirmMsg").textContent = msg;
    pendingConfirm = okFn;
    $("confirmOverlay").classList.remove("hidden");
  }

  function closeConfirm() {
    pendingConfirm = null;
    $("confirmOverlay").classList.add("hidden");
  }

  btnConfirmOk.onclick = async () => {
    const fn = pendingConfirm;
    closeConfirm();
    if (fn) {
      try {
        await fn();
      } catch (err) {
        toast("操作失败：" + err);
      }
    }
  };
  btnConfirmCancel.onclick = closeConfirm;

  // 失焦自动隐藏已由后端 focus watchdog 处理（点击窗口外退出面板进程）
  const onShown = () => {
    // 重触发内容上浮动效
    const el = $("app");
    el.classList.remove("pop");
    void el.offsetWidth;
    el.classList.add("pop");
    // 面板打开时立刻拉一次账号状态，避免显示过期数据
    load();
  };
  rt.EventsOn("panel:shown", onShown);
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") Go.HidePanel();
  });

  // 后端事件
  rt.EventsOn("accounts", (accts) => {
    if (!state) return;
    state.accounts = accts;
    renderAccounts();
  });
  rt.EventsOn("checkin", (e) => {
    const prefix = e.platform ? `${e.platform} ${e.uid}: ` : "签到：";
    toast(prefix + (e.ok ? (e.msg || "成功") : `失败：${e.msg || "未知错误"}`));
  });
  rt.EventsOn("refresh", (e) => {
    toast(`${e.platform || "账号"} ${e.uid || ""} 令牌刷新失败：${e.msg || "未知错误"}`);
  });
  rt.EventsOn("login", (e) => handleLoginEvent(e));

  // 拖拽结束（mouseup）后延迟读取窗口位置并保存，下次启动恢复。
  // Wails 原生拖拽（WM_NCLBUTTONDOWN+HTCAPTION）期间前端收不到事件，
  // 需等系统拖拽完全结束（延时 350ms）再读取，避免保存中间位置。
  let lastSavedPos = null;
  document.addEventListener("mouseup", () => {
    setTimeout(async () => {
      try {
        const pos = await rt.WindowGetPosition();
        if (!pos || !Number.isFinite(pos.x) || !Number.isFinite(pos.y)) return;
        const x = Math.round(pos.x);
        const y = Math.round(pos.y);
        if (!lastSavedPos || lastSavedPos.x !== x || lastSavedPos.y !== y) {
          lastSavedPos = { x, y };
          Go.SavePanelPos(x, y);
        }
      } catch (e) {
        /* runtime 不可用（如无头模式）时忽略 */
      }
    }, 350);
  });
}

// 面板可见时轮询账号状态：让「正在调用」标识能实时出现/消失。
// 面板隐藏后停止轮询，避免无谓开销。
function startLivePolling() {
  if (liveTimer) return;
  liveTimer = setInterval(async () => {
    if (document.hidden) return;
    try {
      const accts = await Go.GetAccounts();
      if (state && accts) {
        state.accounts = accts;
        renderAccounts();
      }
    } catch (e) { /* 忽略瞬时失败 */ }
  }, 1500);
}

function stopLivePolling() {
  clearInterval(liveTimer);
  liveTimer = null;
}

document.addEventListener("DOMContentLoaded", () => {
  // 默认浅色：只在用户显式选过深色时才用深色（不再跟随系统，避免系统深色导致看不清）
  applyTheme(localStorage.getItem("wbw_theme") || "light");
  bind();
  load();
  startLivePolling();
});

document.addEventListener("visibilitychange", () => {
  if (document.hidden) stopLivePolling();
  else startLivePolling();
});

// ---------------------------------------------------------------------------
// 深色/浅色手动切换
// data-theme="dark"/"light"：手动强制；默认 light
// ---------------------------------------------------------------------------
function applyTheme(theme) {
  const root = document.documentElement;
  const btn = $("btnTheme");
  if (theme === "dark") {
    root.setAttribute("data-theme", "dark");
    btn.textContent = "☀";
    btn.title = "切换到浅色";
  } else {
    root.setAttribute("data-theme", "light");
    btn.textContent = "🌙";
    btn.title = "切换到深色";
  }
}

function toggleTheme() {
  const cur = document.documentElement.getAttribute("data-theme");
  const next = cur === "dark" ? "light" : "dark";
  localStorage.setItem("wbw_theme", next);
  applyTheme(next);
}
