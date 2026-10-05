/* workbuddy-wild 账号管理面板前端逻辑（零依赖 vanilla JS） */
(function () {
  'use strict';

  var CSRF = window.__CSRF__ || '';
  var AUTH_REQ = window.__AUTH_REQ__ === true;
  var TOKEN_KEY = 'wb2a_admin_token';

  var state = {
    token: localStorage.getItem(TOKEN_KEY) || '',
    wbFlow: null,
    trFlow: null,
    accounts: {},
    timer: null
  };

  /* ---------- 工具 ---------- */
  function $(id) { return document.getElementById(id); }

  function toast(msg, kind) {
    var el = $('toast');
    el.textContent = msg;
    el.className = 'toast' + (kind ? ' ' + kind : '');
    clearTimeout(el._t);
    el._t = setTimeout(function () { el.className = 'toast hidden'; }, 3200);
  }

  function setMsg(id, text, kind) {
    var el = $(id);
    el.textContent = text || '';
    el.className = 'msg' + (kind ? ' ' + kind : '');
  }

  function api(path, body) {
    var opt = { method: body === undefined ? 'GET' : 'POST', headers: {} };
    if (state.token) opt.headers['X-Admin-Token'] = state.token;
    if (body !== undefined) {
      opt.headers['X-CSRF'] = CSRF;
      opt.headers['Content-Type'] = 'application/json';
      opt.body = JSON.stringify(body);
    }
    return fetch('/admin/api' + path, opt).then(function (res) {
      return res.json().catch(function () { return { ok: false, error: 'HTTP ' + res.status }; });
    }).catch(function (e) {
      return { ok: false, error: '网络错误：' + e.message };
    });
  }

  function copyText(text, btn) {
    function done() {
      var old = btn.textContent;
      btn.textContent = '已复制';
      setTimeout(function () { btn.textContent = old; }, 1400);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () { fallback(); });
    } else { fallback(); }
    function fallback() {
      var ta = document.createElement('textarea');
      ta.value = text; document.body.appendChild(ta); ta.select();
      try { document.execCommand('copy'); done(); } catch (e) { toast('复制失败，请手动选择', 'err'); }
      document.body.removeChild(ta);
    }
  }

  function fmtTime(iso) {
    if (!iso) return '';
    var d = new Date(iso);
    if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '';
    var p = function (n) { return n < 10 ? '0' + n : '' + n; };
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) +
           ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
      .replace(/"/g, '&quot;');
  }

  /* ---------- 渲染账号列表 ---------- */
  function statusBadge(a) {
    if (a.disabled) return '<span class="badge off">已禁用</span>';
    if (a.cooling) {
      var until = fmtTime(a.until);
      return '<span class="badge cool" title="' + esc(a.reason) + '">冷却中' + (until ? ' 至 ' + until : '') + '</span>';
    }
    if (!a.last_used_at && !a.last_checkin_at) return '<span class="badge never">未使用</span>';
    return '<span class="badge ok">可用</span>';
  }

  function renderGroup(title, kind, list) {
    var html = '<div class="acc-group"><h3>' + esc(title) + '（' + list.length + '）</h3>';
    if (!list.length) {
      html += '<div class="empty">还没有账号，请在上方「添加账号」里登录</div></div>';
      return html;
    }
    html += '<table class="acc-table"><thead><tr>' +
      '<th>昵称</th><th>UID</th><th>积分</th><th>状态</th><th>最近签到</th><th></th>' +
      '</tr></thead><tbody>';
    list.forEach(function (a) {
      var nick = a.nickname || '（无昵称）';
      var lastCheckin = a.last_checkin_at
        ? (a.last_checkin_ok ? '✓ ' : '✗ ') + fmtTime(a.last_checkin_at)
        : '—';
      var msg = a.last_checkin_msg ? ' title="' + esc(a.last_checkin_msg) + '"' : '';
      html += '<tr>' +
        '<td>' + esc(nick) + '</td>' +
        '<td class="mono">' + esc(a.uid) + '</td>' +
        '<td class="credits">' + (a.credits || 0) + '</td>' +
        '<td>' + statusBadge(a) + '</td>' +
        '<td class="mono"' + msg + '>' + lastCheckin + '</td>' +
        '<td style="text-align:right">' +
          '<button class="btn btn-danger" data-del-kind="' + esc(kind) + '" data-del-uid="' + esc(a.uid) + '" data-del-nick="' + esc(nick) + '">删除</button>' +
        '</td>' +
      '</tr>';
    });
    html += '</tbody></table></div>';
    return html;
  }

  function render() {
    var wb = state.accounts.workbuddy || [];
    var tr = state.accounts.traework || [];
    var all = wb.concat(tr);

    $('n-wb').textContent = wb.length;
    $('n-tr').textContent = tr.length;
    $('n-healthy').textContent = all.filter(function (a) { return !a.disabled && !a.cooling; }).length;
    $('n-credits').textContent = all.reduce(function (s, a) { return s + (a.credits || 0); }, 0);

    var box = $('accounts');
    var html = renderGroup('WorkBuddy', 'workbuddy', wb) + renderGroup('TraeWork', 'traework', tr);
    box.innerHTML = html;

    // 绑定删除按钮
    box.querySelectorAll('[data-del-uid]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var kind = btn.getAttribute('data-del-kind');
        var uid = btn.getAttribute('data-del-uid');
        var nick = btn.getAttribute('data-del-nick');
        if (!confirm('确定删除账号「' + nick + '」？\n\n该操作会删除 /data/auths 下对应的凭证文件，不可恢复。')) return;
        btn.disabled = true;
        api('/accounts/delete', { kind: kind, uid: uid }).then(function (r) {
          if (r.ok) {
            toast('已删除「' + nick + '」', 'ok');
            load();
          } else {
            btn.disabled = false;
            toast(r.error || '删除失败', 'err');
          }
        });
      });
    });

    var base = location.origin;
    $('base-url').textContent = base + '/v1';
    $('models-url').textContent = base + '/v1/models';
    $('list-hint').textContent = '共 ' + all.length + ' 个账号 · 面板时间 ' + (state.server_tz || '');
  }

  /* ---------- 加载 ---------- */
  function load() {
    return api('/state').then(function (r) {
      if (!r.ok) {
        $('status-dot').className = 'dot off';
        if (AUTH_REQ && !state.token) { showAuth(); return; }
        toast(r.error || '加载失败', 'err');
        if (AUTH_REQ) showAuth(r.error);
        return;
      }
      $('status-dot').className = 'dot on';
      state.accounts = r.accounts || {};
      state.server_tz = r.server_tz;
      $('authbox').classList.add('hidden');
      $('main').classList.remove('hidden');
      render();
    });
  }

  function showAuth(msg) {
    $('authbox').classList.remove('hidden');
    $('main').classList.add('hidden');
    $('token').value = state.token;
    if (msg) { $('auth-msg').textContent = msg; $('auth-msg').className = 'msg err'; }
  }

  /* ---------- WorkBuddy 登录 ---------- */
  function wbReset() {
    $('wb-step2').classList.add('hidden');
    setMsg('wb-msg', '');
    $('wb-start').disabled = false;
    state.wbFlow = null;
  }

  function wbStart() {
    $('wb-start').disabled = true;
    setMsg('wb-msg', '正在获取授权链接…', 'info');
    api('/login/workbuddy/start', {}).then(function (r) {
      if (!r.ok) {
        setMsg('wb-msg', r.error || '获取失败', 'err');
        $('wb-start').disabled = false;
        return;
      }
      state.wbFlow = r.flow_id;
      var a = $('wb-link');
      a.href = r.url; a.textContent = r.url;
      $('wb-step2').classList.remove('hidden');
      setMsg('wb-msg', '');
      window.open(r.url, '_blank', 'noopener');
      // 自动探测登录完成
      pollWbAuto();
    });
  }

  function pollWbAuto() {
    clearTimeout(state.timer);
    state.timer = setTimeout(function () {
      if (!state.wbFlow) return;
      api('/login/workbuddy/poll', { flow_id: state.wbFlow }).then(function (r) {
        if (r.ok && r.done) {
          setMsg('wb-msg', '✓ 已添加「' + (r.nickname || r.uid) + '」', 'ok');
          toast('账号添加成功', 'ok');
          wbReset();
          load();
          return;
        }
        if (r.ok && r.pending) { pollWbAuto(); return; }
        if (!r.ok) { setMsg('wb-msg', r.error || '轮询失败', 'err'); }
      });
    }, 3000);
  }

  function wbPoll() {
    if (!state.wbFlow) { toast('请先获取授权链接', 'err'); return; }
    setMsg('wb-msg', '正在检查登录状态…', 'info');
    api('/login/workbuddy/poll', { flow_id: state.wbFlow }).then(function (r) {
      if (r.ok && r.done) {
        setMsg('wb-msg', '✓ 已添加「' + (r.nickname || r.uid) + '」', 'ok');
        toast('账号添加成功', 'ok');
        wbReset();
        load();
      } else if (r.ok && r.pending) {
        setMsg('wb-msg', '还没检测到登录完成，请确认已在浏览器登录后再点一次', 'err');
        pollWbAuto();
      } else {
        setMsg('wb-msg', r.error || '失败', 'err');
      }
    });
  }

  /* ---------- TraeWork 登录 ---------- */
  function trReset() {
    $('tr-step2').classList.add('hidden');
    $('tr-callback').value = '';
    setMsg('tr-msg', '');
    $('tr-start').disabled = false;
    state.trFlow = null;
  }

  function trStart() {
    var dev = $('tr-device').value.trim();
    $('tr-start').disabled = true;
    setMsg('tr-msg', '正在生成授权链接…', 'info');
    api('/login/traework/start', { device_id: dev }).then(function (r) {
      if (!r.ok) {
        setMsg('tr-msg', r.error || '生成失败', 'err');
        $('tr-start').disabled = false;
        return;
      }
      state.trFlow = r.flow_id;
      var a = $('tr-link');
      a.href = r.url; a.textContent = r.url;
      $('tr-step2').classList.remove('hidden');
      setMsg('tr-msg', r.warning || '', r.warning ? 'err' : 'info');
      window.open(r.url, '_blank', 'noopener');
    });
  }

  function trPoll() {
    if (!state.trFlow) { toast('请先获取授权链接', 'err'); return; }
    var cb = $('tr-callback').value.trim();
    if (!cb) { setMsg('tr-msg', '请粘贴回调 URL', 'err'); return; }
    $('tr-poll').disabled = true;
    setMsg('tr-msg', '正在兑换凭证…', 'info');
    api('/login/traework/poll', { flow_id: state.trFlow, callback_url: cb }).then(function (r) {
      $('tr-poll').disabled = false;
      if (r.ok && r.done) {
        setMsg('tr-msg', '✓ 已添加「' + (r.nickname || r.uid) + '」', 'ok');
        toast('账号添加成功', 'ok');
        trReset();
        load();
      } else {
        setMsg('tr-msg', r.error || '失败', 'err');
      }
    });
  }

  /* ---------- 事件绑定 ---------- */
  function bind() {
    $('btn-refresh').addEventListener('click', function () { load(); toast('已刷新'); });

    $('btn-token').addEventListener('click', function () {
      var t = $('token').value.trim();
      state.token = t;
      localStorage.setItem(TOKEN_KEY, t);
      load();
    });
    $('token').addEventListener('keydown', function (e) {
      if (e.key === 'Enter') $('btn-token').click();
    });

    // tabs
    document.querySelectorAll('.tab').forEach(function (tab) {
      tab.addEventListener('click', function () {
        document.querySelectorAll('.tab').forEach(function (t) { t.classList.remove('active'); });
        tab.classList.add('active');
        var name = tab.getAttribute('data-tab');
        $('tab-wb').classList.toggle('hidden', name !== 'wb');
        $('tab-tr').classList.toggle('hidden', name !== 'tr');
      });
    });

    // WorkBuddy
    $('wb-start').addEventListener('click', wbStart);
    $('wb-poll').addEventListener('click', wbPoll);
    $('wb-copy').addEventListener('click', function () {
      var a = $('wb-link');
      if (a && a.href && a.textContent !== '（等待生成…）') copyText(a.href, this);
    });

    // TraeWork
    $('tr-start').addEventListener('click', trStart);
    $('tr-poll').addEventListener('click', trPoll);
    $('tr-copy').addEventListener('click', function () {
      var a = $('tr-link');
      if (a && a.href && a.textContent !== '（等待生成…）') copyText(a.href, this);
    });

    // 重新加载账号
    $('btn-reload').addEventListener('click', function () {
      var btn = this;
      btn.disabled = true;
      api('/accounts/reload', {}).then(function (r) {
        btn.disabled = false;
        if (r.ok) {
          toast('已重新加载：WorkBuddy ' + r.workbuddy + ' · TraeWork ' + r.traework, 'ok');
          load();
        } else {
          toast(r.error || '重新加载失败', 'err');
        }
      });
    });

    // 复制 base url
    document.querySelectorAll('[data-copy]').forEach(function (btn) {
      btn.addEventListener('click', function () {
        var el = $(btn.getAttribute('data-copy'));
        if (el) copyText(el.textContent, btn);
      });
    });

    // 每 15 秒自动刷新列表（仅当面板可见）
    setInterval(function () {
      if (!$('main').classList.contains('hidden') && !document.hidden) load();
    }, 15000);
  }

  /* ---------- 启动 ---------- */
  document.addEventListener('DOMContentLoaded', function () {
    bind();
    if (AUTH_REQ && !state.token) { showAuth(); }
    load();
  });
})();
