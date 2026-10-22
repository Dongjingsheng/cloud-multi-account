// ──────────────────────────────────────────────────────────────
// QwenPaw 云端多账号插件 v1.6（纯 Go 桥配适 · 玻璃质感 · 完善布局）
// 设计：总览条 + 账号卡片网格 + 明细区 + 统一弹窗 + 反馈增强
// ──────────────────────────────────────────────────────────────
(function () {
  "use strict";

  var PLUGIN_ID = "cloud-multi-account";
  var ROUTE_ID = PLUGIN_ID + ".settings";
  var BRIDGE = "http://127.0.0.1:59000";
  var CREDS_KEY = "cloudMaCredsList";
  var refreshTimer = null;
  var TOKEN_FULL_MIN = 120; // access_token 全量有效期（分钟），用于进度条

  var state = {
    bridge: null, status: null, instances: [], busy: false,
    message: null, messageOk: false, loading: true,
    expanded: null,         // 展开详情的账号（oauth_file）
    menuOpen: null,         // 打开操作菜单的账号
    autoRelogin: localStorage.getItem("cloudMaAutoRelogin") !== "0"
  };

  // ── 工具 ──────────────────────────────────────────────
  function api(path, opts) {
    return fetch(BRIDGE + path, Object.assign({ method: "GET" }, opts || {}))
      .then(function (r) { return r.json().catch(function () { return {}; }); });
  }
  function post(path, body) {
    return api(path, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body || {}) });
  }
  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }
  function on(el, ev, fn) { if (el) el.addEventListener(ev, fn); }
  function loadCreds() { try { return JSON.parse(localStorage.getItem(CREDS_KEY) || "[]"); } catch (e) { return []; } }
  function saveCreds(list) { try { localStorage.setItem(CREDS_KEY, JSON.stringify(list)); } catch (e) {} }
  function clearCreds() { try { localStorage.removeItem(CREDS_KEY); } catch (e) {} }
  function upsertCred(entry) {
    var list = loadCreds(), key = entry.oauth_file || ("u:" + entry.username), idx = -1;
    for (var i = 0; i < list.length; i++) {
      var k = list[i].oauth_file || ("u:" + list[i].username);
      if (k === key) { idx = i; break; }
    }
    if (idx >= 0) list[idx] = entry; else list.push(entry);
    saveCreds(list);
  }
  function findCred(i) {
    var list = loadCreds();
    for (var j = 0; j < list.length; j++) {
      var c = list[j];
      if (i.oauth_file && c.oauth_file === i.oauth_file) return c;
      if (i.name && c.name === i.name) return c;
    }
    return list.length === 1 ? list[0] : null;
  }
  function fmtMin(m) {
    if (m == null || m < 0) return "--";
    if (m < 1) return Math.round(m * 60) + " 秒";
    return Math.round(m) + " 分钟";
  }
  function toast(ok, text) {
    var p = document.getElementById(PLUGIN_ID + "-panel");
    if (!p) return;
    var t = document.getElementById(PLUGIN_ID + "-toast");
    if (!t) {
      t = document.createElement("div");
      t.id = PLUGIN_ID + "-toast";
      t.style.cssText = "position:fixed;right:24px;bottom:24px;z-index:99999;background:rgba(30,41,59,.92);border:1px solid rgba(148,163,184,.25);color:#e2e8f0;padding:10px 16px;border-radius:12px;font-size:13px;backdrop-filter:blur(14px);box-shadow:0 8px 30px rgba(0,0,0,.35);opacity:0;transform:translateY(8px);transition:all .25s ease;max-width:340px";
      document.body.appendChild(t);
    }
    t.textContent = (ok ? "✅ " : "⚠️ ") + text;
    t.style.borderLeft = (ok ? "3px solid #34d399" : "3px solid #f87171");
    t.style.opacity = "1"; t.style.transform = "none";
    clearTimeout(t._t);
    t._t = setTimeout(function () { t.style.opacity = "0"; t.style.transform = "translateY(8px)"; }, 3200);
  }

  // ── 样式（玻璃质感 + 完善布局）────────────────────────────
  function injectStyle() {
    var id = PLUGIN_ID + "-style";
    if (document.getElementById(id)) return;
    var st = document.createElement("style");
    st.id = id;
    st.textContent = "\
@keyframes " + PLUGIN_ID + "fade{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:none}}\
@keyframes " + PLUGIN_ID + "pulse{0%,100%{opacity:1}50%{opacity:.45}}\n\
.cm-wrap{display:flex;flex-direction:column;gap:14px;max-width:980px;margin:0 auto;padding:18px 16px 40px;font-family:-apple-system,'Segoe UI',Roboto,'PingFang SC','Microsoft YaHei',sans-serif;color:#e2e8f0;animation:" + PLUGIN_ID + "fade .35s ease}\n\
.cm-head{display:flex;align-items:center;gap:10px;flex-wrap:wrap}\n\
.cm-head .cm-logo{width:34px;height:34px;border-radius:10px;background:linear-gradient(135deg,#22d3ee23,#818cf829);border:1px solid rgba(148,163,184,.22);display:flex;align-items:center;justify-content:center;font-size:17px}\n\
.cm-head h2{margin:0;font-size:16px;font-weight:600;letter-spacing:.2px}\n\
.cm-head .cm-sub{font-size:11.5px;color:#94a3b8;margin-top:2px}\n\
.cm-spacer{flex:1}\n\
.cm-btn{border:1px solid rgba(148,163,184,.25);background:rgba(148,163,184,.08);color:#e2e8f0;border-radius:10px;padding:7px 14px;font-size:12.5px;cursor:pointer;transition:all .2s ease;display:inline-flex;align-items:center;gap:6px}\n\
.cm-btn:hover{background:rgba(148,163,184,.18)}\n\
.cm-btn:disabled{opacity:.5;cursor:not-allowed}\n\
.cm-btn.primary{background:linear-gradient(135deg,#0ea5e9,#6366f1);border-color:transparent;color:#fff}\n\
.cm-btn.primary:hover{filter:brightness(1.12)}\n\
.cm-btn.danger{color:#fca5a5;border-color:rgba(248,113,113,.35);background:rgba(248,113,113,.08)}\n\
.cm-glass{background:rgba(148,163,184,.07);border:1px solid rgba(148,163,184,.15);border-radius:16px;backdrop-filter:blur(16px);-webkit-backdrop-filter:blur(16px)}\n\
.cm-overview{display:flex;align-items:center;gap:8px;padding:12px 16px;flex-wrap:wrap}\n\
.cm-ov-item{display:inline-flex;align-items:center;gap:6px;font-size:12px;color:#cbd5e1;padding:4px 10px;border-radius:999px;background:rgba(148,163,184,.1);border:1px solid rgba(148,163,184,.16)}\n\
.cm-ov-item b{font-weight:600}\n\
.cm-dot{width:8px;height:8px;border-radius:50%;display:inline-block}\n\
.cm-bridge-ok{background:#34d399;box-shadow:0 0 8px rgba(52,211,153,.7)}\n\
.cm-bridge-down{background:#f87171;box-shadow:0 0 8px rgba(248,113,113,.7);animation:" + PLUGIN_ID + "pulse 1.4s infinite}\n\
.cm-grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(280px,1fr));gap:14px}\n\
.cm-card{padding:16px;position:relative;cursor:pointer;transition:transform .18s ease,border-color .18s ease}\n\
.cm-card:hover{transform:translateY(-2px);border-color:rgba(52,211,153,.4)}\n\
.cm-card .cm-card-top{display:flex;align-items:center;gap:10px}\n\
.cm-card .cm-ava{width:36px;height:36px;border-radius:12px;display:flex;align-items:center;justify-content:center;font-size:16px;font-weight:600;background:linear-gradient(135deg,#0ea5e933,#6366f133);border:1px solid rgba(148,163,184,.2)}\n\
.cm-card .cm-cname{font-size:14px;font-weight:600;word-break:break-all}\n\
.cm-card .cm-csub{font-size:11px;color:#94a3b8;margin-top:3px}\n\
.cm-badge{font-size:11px;padding:3px 8px;border-radius:999px;font-weight:500;white-space:nowrap}\n\
.cm-badge.ok{background:rgba(52,211,153,.14);color:#6ee7b7;border:1px solid rgba(52,211,153,.3)}\n\
.cm-badge.warn{background:rgba(251,191,36,.13);color:#fcd34d;border:1px solid rgba(251,191,36,.3)}\n\
.cm-badge.bad{background:rgba(248,113,113,.13);color:#fca5a5;border:1px solid rgba(248,113,113,.3)}\n\
.cm-badge.off{background:rgba(148,163,184,.1);color:#94a3b8;border:1px solid rgba(148,163,184,.2)}\n\
.cm-prog{height:5px;border-radius:99px;background:rgba(148,163,184,.14);margin-top:12px;overflow:hidden}\n\
.cm-prog i{display:block;height:100%;border-radius:99px;transition:width .5s ease}\n\
.cm-menu{position:absolute;right:10px;top:12px}\n\
.cm-dots{background:none;border:none;color:#94a3b8;font-size:18px;cursor:pointer;padding:2px 8px;border-radius:8px}\n\
.cm-dots:hover{background:rgba(148,163,184,.15);color:#e2e8f0}\n\
.cm-menu-pop{position:absolute;right:0;top:34px;min-width:150px;background:rgba(15,23,42,.96);border:1px solid rgba(148,163,184,.2);border-radius:12px;padding:6px;box-shadow:0 12px 40px rgba(0,0,0,.5);z-index:20;animation:" + PLUGIN_ID + "fade .18s ease}\n\
.cm-menu-pop button{display:block;width:100%;text-align:left;background:none;border:none;color:#cbd5e1;font-size:12.5px;padding:8px 12px;border-radius:8px;cursor:pointer}\n\
.cm-menu-pop button:hover{background:rgba(148,163,184,.13);color:#fff}\n\
.cm-menu-pop button.danger{color:#fca5a5}\n\
.cm-detail{margin-top:2px;padding:16px 18px;animation:" + PLUGIN_ID + "fade .25s ease}\n\
.cm-tabs{display:flex;gap:8px;margin-bottom:12px;flex-wrap:wrap}\n\
.cm-tab{font-size:12.5px;padding:6px 14px;border-radius:999px;border:1px solid transparent;color:#94a3b8;cursor:pointer;background:transparent}\n\
.cm-tab.active{color:#e2e8f0;background:rgba(148,163,184,.14);border-color:rgba(148,163,184,.22)}\n\
.cm-session{display:flex;align-items:center;gap:10px;padding:10px 12px;border-radius:10px;cursor:pointer;border:1px solid transparent;transition:background .15s}\n\
.cm-session:hover{background:rgba(148,163,184,.09)}\n\
.cm-session .cm-sname{flex:1;font-size:13px;color:#e2e8f0;overflow:hidden;text-overflow:ellipsis;white-space:nowrap}\n\
.cm-session .cm-stime{font-size:11px;color:#64748b}\n\
.cm-empty{padding:44px 20px;text-align:center;color:#94a3b8;font-size:13px}\n\
.cm-skel{animation:" + PLUGIN_ID + "pulse 1.2s infinite}\n\
.cm-skel .cm-card{min-height:120px}\n\
.cm-log{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;font-size:11.5px;color:#94a3b8;max-height:260px;overflow:auto;padding:0}\n\
.cm-log div{padding:4px 2px;border-bottom:1px solid rgba(148,163,184,.06)}\n\
.cm-input{width:100%;box-sizing:border-box;background:rgba(15,23,42,.6);border:1px solid rgba(148,163,184,.28);color:#e2e8f0;border-radius:10px;padding:10px 12px;font-size:13px;outline:none;margin-top:8px}\n\
.cm-input:focus{border-color:#38bdf8}\n\
.cm-modal-mask{position:fixed;inset:0;background:rgba(2,6,23,.6);backdrop-filter:blur(6px);z-index:1000;display:flex;align-items:center;justify-content:center;animation:" + PLUGIN_ID + "fade .2s ease}\n\
.cm-modal{width:min(380px,92vw);background:rgba(15,23,42,.97);border:1px solid rgba(148,163,184,.22);border-radius:18px;padding:22px;box-shadow:0 20px 60px rgba(0,0,0,.55)}\n\
.cm-modal h3{margin:0 0 4px;font-size:15px}\n\
.cm-modal p{color:#94a3b8;font-size:12px;margin:0 0 10px}\n\
@media (max-width:640px){.cm-grid{grid-template-columns:1fr}}";
    document.head.appendChild(st);
  }

  // ── UI 构建 ──────────────────────────────────────────────
  function el(tag, cls, html) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (html != null) e.innerHTML = html;
    return e;
  }
  function buildPanel() {
    injectStyle();
    var p = document.getElementById(PLUGIN_ID + "-panel");
    if (p) p.remove();
    p = document.createElement("div");
    p.id = PLUGIN_ID + "-panel";
    p.className = "cm-wrap";
    p.innerHTML =
      '<div class="cm-head">' +
      '<div class="cm-logo">☁️</div>' +
      '<div><h2>云端账号</h2><div class="cm-sub">账号密码登录 · 会话续期 · 失效自动重登</div></div>' +
      '<div class="cm-spacer"></div>' +
      '<button class="cm-btn" id="' + PLUGIN_ID + '-refresh" title="刷新">⟳ 刷新</button>' +
      '<button class="cm-btn primary" id="' + PLUGIN_ID + '-add">＋ 添加账号</button>' +
      '</div>' +
      '<div class="cm-overview cm-glass" id="' + PLUGIN_ID + '-overview"></div>' +
      '<div class="cm-grid" id="' + PLUGIN_ID + '-grid"></div>' +
      '<div id="' + PLUGIN_ID + '-detail" style="max-height:65vh;overflow-y:auto"></div>';
    document.querySelector("#" + ROUTE_ID + ", .qwenpaw-page") ? 0 : 0;
    on(document.getElementById(PLUGIN_ID + "-refresh"), "click", function () { loadAll(); toast(true, "正在刷新…"); });
    on(document.getElementById(PLUGIN_ID + "-add"), "click", function () { openLoginModal(); });
    on(p, "click", function (e) { if (state.menuOpen && !e.target.closest(".cm-menu")) state.menuOpen = null; });
    // 挂载到插件页面容器（与旧版一致，不清空宿主）
    var pageContainer = document.querySelector('[id$="-page"]') || document.body;
    pageContainer.appendChild(p);
    return p;
  }

  function renderOverview() {
    var ov = document.getElementById(PLUGIN_ID + "-overview");
    if (!ov) return;
    var insts = state.instances || [];
    var online = insts.filter(function (i) { return i.status === "running"; }).length;
    var bad = insts.filter(function (i) { return i.status === "invalid-token" || i.status === "no-token"; }).length;
    var bridgeOk = state.bridge === true;
    var bridgeHtml = bridgeOk
      ? '<span class="cm-ov-item"><span class="cm-dot cm-bridge-ok"></span>桥 <b>正常</b></span>'
      : '<span class="cm-ov-item"><span class="cm-dot cm-bridge-down"></span>桥 <b>掉线</b></span>';
    // 最短 token 剩余
    var minRemain = null;
    insts.forEach(function (i) { if (i.token_remain_min != null && i.token_remain_min >= 0) { minRemain = minRemain == null ? i.token_remain_min : Math.min(minRemain, i.token_remain_min); } });
    ov.innerHTML =
      '<span class="cm-ov-item">账号 <b>' + insts.length + '</b></span>' +
      '<span class="cm-ov-item">在线 <b style="color:#6ee7b7">' + online + '</b></span>' +
      '<span class="cm-ov-item">失效 <b style="color:#fca5a5">' + bad + '</b></span>' +
      (minRemain != null ? '<span class="cm-ov-item">token 最短剩 <b>' + fmtMin(minRemain) + '</b></span>' : '') +
      bridgeHtml;
  }

  function cardBadge(i) {
    if (!i.app_id) return '<span class="cm-badge off">⚪ 未开通实例</span>';
    if (i.status === "running") return '<span class="cm-badge ok">🟢 已登录' + (i.token_remain_min != null && i.token_remain_min >= 0 ? ' · 剩 ' + fmtMin(i.token_remain_min) : '') + '</span>';
    if (i.status === "invalid-token") return '<span class="cm-badge bad">🔴 token 失效</span>';
    if (i.status === "no-token") return '<span class="cm-badge off">⚪ 未登录</span>';
    return '<span class="cm-badge off">· 未知</span>';
  }
  function cardProg(i) {
    var m = i.token_remain_min;
    if (m == null || m < 0) return '<div class="cm-prog"><i style="width:0;background:rgba(248,113,113,.5)"></i></div>';
    var r = Math.max(0, Math.min(1, m / TOKEN_FULL_MIN));
    var color = m < 20 ? "linear-gradient(90deg,#f87171,#fb923c)" : (m < 60 ? "linear-gradient(90deg,#fbbf24,#f59e0b)" : "linear-gradient(90deg,#34d399,#22d3ee)");
    return '<div class="cm-prog"><i style="width:' + Math.round(r * 100) + '%;background:' + color + '"></i></div>';
  }
  function subExpire(i) {
    if (!i.app_id) return "未开通实例";
    if (i.status === "running") {
      if (i.token_remain_min != null && i.token_remain_min >= 0) return "有效期 " + fmtMin(i.token_remain_min);
      return "已登录";
    }
    if (i.status === "invalid-token") return "token 已失效";
    if (i.status === "no-token") return "未登录";
    return "";
  }
  function accountName(i) {
    var c = findCred(i);
    if (c && c.name) return c.name;
    if (i && i.name) return i.name;
    return ((i && i.oauth_file) || "账号").replace(/^cloud_oauth_/, "").replace(/\.json$/, "");
  }

  function renderGrid() {
    var g = document.getElementById(PLUGIN_ID + "-grid");
    if (!g) return;
    var insts = state.instances || [];
    if (state.loading) {
      g.innerHTML = '<div class="cm-card cm-glass cm-skel" style="grid-column:1/-1;text-align:center;color:#94a3b8">加载中…</div>';
      return;
    }
    if (!insts.length) {
      g.innerHTML = '<div class="cm-card cm-glass" style="grid-column:1/-1">' +
        '<div class="cm-empty"><div style="font-size:30px;margin-bottom:10px">☁️</div>还没有云端账号<br><br>' +
        '<button class="cm-btn primary" id="' + PLUGIN_ID + '-empty-add">＋ 登录第一个账号</button></div></div>';
      on(document.getElementById(PLUGIN_ID + "-empty-add"), "click", function () { openLoginModal(); });
      return;
    }
    var html = "";
    insts.forEach(function (i) {
      var name = accountName(i);
      var menu = state.menuOpen === i.oauth_file
        ? '<div class="cm-menu-pop">' +
          '<button data-act="restart" data-key="' + esc(i.oauth_file) + '">⟳ 重启实例</button>' +
          (i.status === "invalid-token" || i.status === "no-token" ? '<button data-act="relogin" data-key="' + esc(i.oauth_file) + '">🔑 重新登录</button>' : '') +
          '<button class="danger" data-act="remove" data-key="' + esc(i.oauth_file) + '">🗑 删除账号</button>' +
          '</div>'
        : "";
      html += '<div class="cm-card cm-glass" data-key="' + esc(i.oauth_file) + '" title="点击展开详情">' +
        '<div class="cm-card-top"><div class="cm-ava">' + esc((name || "?").slice(0, 1).toUpperCase()) + '</div>' +
        '<div style="flex:1;min-width:0"><div class="cm-cname">' + esc(name) + '</div></div>' +
        '<div class="cm-menu"><button class="cm-dots" data-menu="' + esc(i.oauth_file) + '">⋮</button>' + menu + '</div></div>' +
        '<div style="display:flex;align-items:center;gap:8px;margin-top:10px;flex-wrap:wrap">' + cardBadge(i) +
        '<span class="cm-csub" style="margin-top:0">' + esc(subExpire(i)) + '</span></div>' +
        cardProg(i) + '</div>';
    });
    g.innerHTML = html;
    // 卡片点击展开详情
    g.querySelectorAll(".cm-card").forEach(function (card) {
      on(card, "click", function (e) {
        if (e.target.closest(".cm-menu") || e.target.closest(".cm-btn")) return;
        var key = card.getAttribute("data-key");
        state.expanded = state.expanded === key ? null : key;
        renderDetail();
      });
    });
    // ⋮ 菜单
    g.querySelectorAll(".cm-dots").forEach(function (b) {
      on(b, "click", function (e) {
        e.stopPropagation();
        var key = b.getAttribute("data-menu");
        state.menuOpen = state.menuOpen === key ? null : key;
        renderGrid();
      });
    });
    // 操作按钮
    g.querySelectorAll("[data-act]").forEach(function (b) {
      on(b, "click", function (e) {
        e.stopPropagation();
        var act = b.getAttribute("data-act"), key = b.getAttribute("data-key");
        if (act === "detail") { state.expanded = key; renderDetail(); return; }
        if (act === "open") { openCloud(key); return; }
        if (act === "restart") { actInst(key, "restart"); return; }
        if (act === "relogin") { state.menuOpen = null; openLoginModal(key); return; }
        if (act === "remove") { removeInst(key); return; }
      });
    });
  }

  function renderDetail() {
    var d = document.getElementById(PLUGIN_ID + "-detail");
    if (!d) return;
    var key = state.expanded;
    var inst = null;
    (state.instances || []).forEach(function (i) { if (i.oauth_file === key) inst = i; });
    if (!inst) { d.innerHTML = ""; return; }
    var name = accountName(inst);
    d.innerHTML = '<div class="cm-detail cm-glass">' +
      '<div class="cm-tabs"><button class="cm-tab active" data-tab="chat">💬 会话</button>' +
      '<button class="cm-tab" data-tab="info">ℹ️ 详情</button></div>' +
      '<div id="' + PLUGIN_ID + '-detail-body" style="min-height:40px">加载中…</div></div>';
    var tabs = d.querySelectorAll(".cm-tab");
    tabs.forEach(function (t) {
      on(t, "click", function () {
        tabs.forEach(function (x) { x.classList.remove("active"); });
        t.classList.add("active");
        if (t.getAttribute("data-tab") === "chat") renderDetailSessions(inst);
        else renderDetailInfo(inst);
      });
    });
    renderDetailSessions(inst);
  }
  function renderDetailInfo(inst) {
    var b = document.getElementById(PLUGIN_ID + "-detail-body");
    if (!b) return;
    b.innerHTML = '<div style="font-size:12.5px;line-height:1.9;color:#cbd5e1">' +
      '<div>名称：<b>' + esc(accountName(inst)) + '</b></div>' +
      '<div>oauth：<code style="font-size:11px">' + esc(inst.oauth_file || "—") + '</code></div>' +
      '<div>app_id：<code style="font-size:11px">' + esc(inst.app_id || "未开通实例（登录平台后开通 QwenPaw 实例）") + '</code></div>' +
      '<div>状态：' + cardBadge(inst) + '</div>' +
      '<div>token 剩余：<b>' + fmtMin(inst.token_remain_min) + '</b></div>' +
      '</div>';
  }
  function renderDetailSessions(inst) {
    var b = document.getElementById(PLUGIN_ID + "-detail-body");
    if (!b) return;
    // 空实例账号：没有实例就没有对应会话——不展示其他账号的数据
    if (!inst.app_id) {
      b.innerHTML = '<div class="cm-empty">该账号未开通云端实例，无会话</div>';
      return;
    }
    b.innerHTML = '<div style="color:#94a3b8;font-size:12px">加载智能体…</div>';
    api("/api/agents")
      .then(function (r) {
        var agents = (r && r.agents && r.agents.filter(Boolean)) || [];
        if (!agents.length) agents = [{ id: "default", name: "default" }];
        var tabsHtml = '<div style="display:flex;gap:6px;flex-wrap:wrap;margin-bottom:10px">';
        agents.forEach(function (a, idx) {
          tabsHtml += '<button class="cm-tab' + (idx === 0 ? " active" : "") + '" data-ag="' + esc(a.id) + '">' + esc(a.name || a.id) + '</button>';
        });
        tabsHtml += '</div><div id="' + PLUGIN_ID + '-agbody" style="min-height:30px">加载中…</div>';
        b.innerHTML = tabsHtml;
        var btns = b.querySelectorAll("[data-ag]");
        btns.forEach(function (btn) {
          on(btn, "click", function () {
            btns.forEach(function (x) { x.classList.remove("active"); });
            btn.classList.add("active");
            loadAgentChats(btn.getAttribute("data-ag"), b.querySelector("#" + PLUGIN_ID + "-agbody"));
          });
        });
        if (agents[0]) loadAgentChats(agents[0].id, b.querySelector("#" + PLUGIN_ID + "-agbody"));
      })
      .catch(function () { b.innerHTML = '<div class="cm-empty">加载失败（桥不可达）</div>'; });
  }
  function loadAgentChats(agId, body) {
    if (!body) return;
    body.innerHTML = '<div style="color:#94a3b8;font-size:12px">加载会话…</div>';
    api("/api/chats?archived=false&include_app_owned=false", { headers: { "X-Agent-Id": "cloud-" + agId } })
      .then(function (r) {
        var chats = (r && (r.chats || r.data)) || (Array.isArray(r) ? r : []);
        if (!chats.length) { body.innerHTML = '<div class="cm-empty">暂无会话</div>'; return; }
        var html = "";
        chats.forEach(function (c, idx) {
          if (idx > 49) return;
          var t = c.title || c.name || "未命名会话";
          var ts = c.updated_at || c.created_at || "";
          html += '<div class="cm-session"><span class="cm-sname">' + esc(String(t)) + '</span>' +
            '<span class="cm-stime">' + esc(String(ts).slice(0, 10)) + '</span></div>';
        });
        if (chats.length > 50) html += '<div class="cm-empty">仅显示前 50 条</div>';
        body.innerHTML = html;
      })
      .catch(function () { body.innerHTML = '<div class="cm-empty">加载会话失败（桥不可达）</div>'; });
  }

  function openCloud(key) {
    var inst = null;
    (state.instances || []).forEach(function (i) { if (i.oauth_file === key) inst = i; });
    if (!inst || !inst.app_id) { toast(false, "该账号没有实例地址"); return; }
    window.open("https://" + inst.app_id + ".qwenpaw.platform.agentscope.io", "_blank");
  }
  function actInst(key, act) {
    toast(false, "执行 " + act + "…");
    post("/__instance/" + act, { name: key }).then(function (r) {
      toast(r && r.ok ? true : false, (r && r.message) || (r && r.ok ? "已执行" : "执行失败"));
      loadAll();
    }).catch(function () { toast(false, "请求失败（桥不可达）"); });
  }
  function removeInst(key) {
    var name = (key || "").replace(/^cloud_oauth_/, "").replace(/\.json$/, "");
    if (!confirm("删除账号「" + name + "」？\n（token/凭据将一并移除）")) return;
    toast(false, "删除中…");
    post("/__import/remove", { name: key }).then(function (r) {
      toast(r && r.ok !== false, (r && r.message) || "已删除");
      loadAll();
    }).catch(function () { toast(false, "删除失败"); });
  }

  // ── 登录弹窗（统一）─────────────────────────────────────
  function openLoginModal(prefillKey) {
    var old = document.getElementById(PLUGIN_ID + "-modal");
    if (old) old.remove();
    var mask = el("div", "cm-modal-mask");
    mask.id = PLUGIN_ID + "-modal";
    var pre = null;
    if (prefillKey) {
      var list = loadCreds();
      for (var i = 0; i < list.length; i++) if (list[i].oauth_file === prefillKey) pre = list[i];
    }
    mask.innerHTML = '<div class="cm-modal">' +
      '<h3>🔑 登录云端账号</h3><p>登录后 token 自动保存 · 失效自动用记住的密码重登</p>' +
      '<label style="font-size:12px;color:#94a3b8">账号</label><input class="cm-input" id="' + PLUGIN_ID + '-m-u" placeholder="邮箱 / 手机号" value="' + esc(pre ? pre.username || pre.name || "" : "") + '">' +
      '<label style="font-size:12px;color:#94a3b8">密码</label><input class="cm-input" id="' + PLUGIN_ID + '-m-p" type="password" placeholder="密码" value="">' +
      '<label style="font-size:12px;color:#94a3b8">账号名（可选）</label><input class="cm-input" id="' + PLUGIN_ID + '-m-n" placeholder="给这个账号起个名字" value="' + esc(pre ? pre.name || "" : "") + '">' +
      '<div style="display:flex;gap:10px;margin-top:16px">' +
      '<button class="cm-btn" style="flex:1" id="' + PLUGIN_ID + '-m-cancel">取消</button>' +
      '<button class="cm-btn primary" style="flex:1" id="' + PLUGIN_ID + '-m-ok">登录</button></div></div>';
    document.body.appendChild(mask);
    function close() { var m = document.getElementById(PLUGIN_ID + "-modal"); if (m) m.remove(); }
    on(mask, "click", function (e) { if (e.target === mask) close(); });
    on(document.getElementById(PLUGIN_ID + "-m-cancel"), "click", close);
    on(document.getElementById(PLUGIN_ID + "-m-ok"), "click", function () {
      var u = document.getElementById(PLUGIN_ID + "-m-u").value.trim();
      var p = document.getElementById(PLUGIN_ID + "-m-p").value;
      var n = document.getElementById(PLUGIN_ID + "-m-n").value.trim();
      if (!u || !p) { toast(false, "请输入账号和密码"); return; }
      doLogin(u, p, n, close);
    });
    var pu = document.getElementById(PLUGIN_ID + "-m-u");
    if (pu) { pu.focus(); pu.select(); }
  }

  function doLogin(u, p, n, done) {
    state.busy = true;
    post("/__import/login", { username: u, password: p, name: n || "" })
      .then(function (r) {
        state.busy = false;
        if (r && r.ok) {
          upsertCred({ oauth_file: r.oauth_file || "", username: u, password: p, name: n || r.name || "" });
          // 登录成功 → 立即关闭弹窗（智能体校验异步进行，不阻塞）
          if (done) done();
          if (r.duplicate) {
            toast(true, (r.message) || "该账号已存在，已更新 ✅");
            loadAll();
            return;
          }
          toast(true, "登录成功 ✅ 校验智能体同步中…");
          verifyAgents(null, 6);
        } else {
          toast(false, (r && r.error) || "登录失败（账号或密码错误）");
        }
      })
      .catch(function () { state.busy = false; toast(false, "登录请求失败（桥不可达）"); });
  }
  // 登录成功后检测云端智能体是否通过桥同步成功（避免"智能体管理没加载出来"）
  function verifyAgents(done, maxRetry) {
    var retry = 0;
    function check() {
      api("/api/agents")
        .then(function (r) {
          var ags = (r && r.agents) || [];
          if (ags.length) {
            toast(true, "登录成功 ✅ 智能体已同步（" + ags.length + " 个）");
            if (done) done();
            loadAll();
          } else if (retry < maxRetry) {
            retry++;
            setTimeout(check, 2000);
          } else {
            toast(false, "登录成功，但智能体列表暂时为空，请刷新智能体管理页");
            if (done) done();
            loadAll();
          }
        })
        .catch(function () {
          if (retry < maxRetry) { retry++; setTimeout(check, 2000); }
          else { toast(false, "登录成功，但桥暂时连不上智能体，请稍后刷新"); if (done) done(); loadAll(); }
        });
    }
    check();
  }

  // ── 数据加载 ──────────────────────────────────────────────
  function loadStatus() {
    api("/__bridge_status").then(function (r) {
      state.bridge = true;
      state.status = r;
      renderOverview();
    }).catch(function () { state.bridge = false; renderOverview(); });
  }
  function loadInstances(allowAuto) {
    api("/__instances").then(function (r) {
      state.instances = (r && r.instances) || [];
      state.bridge = true;
      state.loading = false;
      renderOverview();
      renderGrid();
      if (state.expanded) renderDetail();
      loadStatus();
      // 自动重登
      var bad = state.instances.filter(function (i) { return i.status === "invalid-token" || i.status === "no-token"; });
      if (allowAuto !== false && state.autoRelogin && bad.length) {
        var cred = findCred(bad[0]);
        if (cred && cred.password) {
          state.busy = true;
          post("/__import/login", { username: cred.username || cred.name, password: cred.password, name: cred.name || "" })
            .then(function (res) {
              state.busy = false;
              if (res && res.ok) toast(true, "已自动重登 ✅ 会话已续期");
              else toast(false, "自动重登失败（密码可能已变），请手动登录");
              loadInstances(false);
            })
            .catch(function () { state.busy = false; toast(false, "自动重登请求失败"); loadInstances(false); });
        }
      }
    }).catch(function () {
      state.bridge = false;
      state.loading = false;
      renderOverview();
      var g = document.getElementById(PLUGIN_ID + "-grid");
      if (g) g.innerHTML = '<div class="cm-card cm-glass" style="grid-column:1/-1;border-color:rgba(248,113,113,.3)">' +
        '<div class="cm-empty">🔌 云端桥不可达（127.0.0.1:59000）<br><br>' +
        '<button class="cm-btn" id="' + PLUGIN_ID + '-retry">⟳ 重试</button></div></div>';
      var rb = document.getElementById(PLUGIN_ID + "-retry");
      if (rb) on(rb, "click", function () { state.loading = true; loadAll(); toast(true, "重试中…"); });
      if (state.loading !== false) { state.loading = true; setTimeout(function () { loadInstances(); }, 2500); }
    });
  }
  function loadAll() { loadInstances(true); }

  // ── 插件注册（React 组件——QwenPaw 路由要求）────────────────
  function registerAPI() {
    try {
      var React = window.QwenPaw.host.React;
      var SettingsPage = function () {
        React.useEffect(function () {
          buildPanel();
          loadAll();
          if (refreshTimer) clearInterval(refreshTimer);
          refreshTimer = setInterval(function () { if (!state.busy) loadInstances(false); }, 20000);
          return function () {
            if (refreshTimer) { clearInterval(refreshTimer); refreshTimer = null; }
            var p = document.getElementById(PLUGIN_ID + "-panel"); if (p) p.remove();
            var m = document.getElementById(PLUGIN_ID + "-modal"); if (m) m.remove();
          };
        }, []);
        return React.createElement("div", { id: PLUGIN_ID + "-page" });
      };
      window.QwenPaw.route.add(PLUGIN_ID, { id: ROUTE_ID, path: "/" + PLUGIN_ID + "/settings", component: SettingsPage });
      window.QwenPaw.menu.add(PLUGIN_ID, { id: PLUGIN_ID + ".menu", label: "云端账号", route: ROUTE_ID, parentId: "plugins-group", location: "primary.settings", order: 1 });
      console.log("[CloudMultiAccount] Menu+route registered (v1.7 玻璃质感·完善布局)");
    } catch (e) { console.warn("[CloudMultiAccount] registerAPI failed:", e); }
  }

  function init() {
    var st = document.createElement("style");
    st.id = PLUGIN_ID + "-anim";
    st.textContent = "@keyframes " + PLUGIN_ID + "fade{from{opacity:0;transform:translateY(8px)}to{opacity:1;transform:none}}";
    if (!document.getElementById(PLUGIN_ID + "-anim")) document.head.appendChild(st);
    function defer() {
      if (typeof window.QwenPaw !== "undefined") registerAPI();
      else setTimeout(function () { if (typeof window.QwenPaw !== "undefined") registerAPI(); }, 3000);
    }
    if (window.requestIdleCallback) requestIdleCallback(defer, { timeout: 2000 });
    else setTimeout(defer, 2000);
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", init);
  else init();

  window["__" + PLUGIN_ID + "_cleanup"] = function () {
    if (refreshTimer) clearInterval(refreshTimer);
    var p = document.getElementById(PLUGIN_ID + "-panel"); if (p) p.remove();
    var m = document.getElementById(PLUGIN_ID + "-modal"); if (m) m.remove();
    var a = document.getElementById(PLUGIN_ID + "-anim"); if (a) a.remove();
    var s = document.getElementById(PLUGIN_ID + "-style"); if (s) s.remove();
    delete window["__" + PLUGIN_ID + "_cleanup"];
  };
})();