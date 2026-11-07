"use strict";
// ---------------- first-run guide ----------------
// Simple path (default): 用 Google 账号开始 → paste the code → 启动 Colab (3 clicks to a starting GPU).
// Full wizard (我有自己的网关 / 设置 / ? menu): 7 steps. Progress is kept in the config (onboarding.go).
// Relies on app.js / ai.js globals: $, $$, esc, api, toast, show, page, gst, profiles, loadProfiles, pollStatus,
// connectEvents, loadTemplates, wake, renderStatus, STATE_CN, BUSY, appCfg, loadSettings, isLocal, AI.

const OB = { st: null, path: "simple", step: 1, net: null, netBusy: false, ccu: null, mini: false, coachOn: false, clip: "" };
const OB_FULL = ["欢迎", "检查网络", "连接方式", "登录", "基本设置", "第一次试用", "完成"];
const OB_PROFILE = "realvisxl-v5", OB_GPU = "L4";
const OB_SAMPLE = "a cozy cafe by the window on a rainy afternoon, steam rising from a cup of coffee, soft natural light, 35mm photo, shallow depth of field";
const OB_LINKS = {
  cu: "https://research.google.com/colaboratory/faq.html#compute-units",
  buy: "https://colab.research.google.com/signup",
  readme: "https://github.com/a3974892-cmd/wakeloom#readme",
  local: "https://github.com/a3974892-cmd/wakeloom#%EF%B8%8F-%E6%9C%AC%E5%9C%B0%E6%A8%A1%E5%BC%8F%E8%AF%B4%E6%98%8E",
  gateway: "https://github.com/a3974892-cmd/wakeloom/blob/main/gateway/README.md",
  guide: "https://github.com/a3974892-cmd/wakeloom/blob/main/docs/GETTING_STARTED.md",
};
const OB_CHECK = { network: "检查网络", login: "登录 Google（或连接网关）", wake: "第一次唤醒 GPU", generate: "生成第一张图 / 第一次对话", stop: "停止 GPU" };

const obOpenURL = u => api("/api/open-url", { json: { url: u } }).catch(() => window.open(u));
async function obLoad() { try { OB.st = await api("/api/onboarding"); } catch { OB.st = null; } return OB.st; }
async function obSave(o) { try { OB.st = await api("/api/onboarding", { json: o }); } catch { } obRenderChecklist(); return OB.st; }
function obMark(item) { if (OB.st && OB.st.checklist && !OB.st.checklist.find(c => c.id === item && c.done)) obSave({ mark: item }); }

function obRate(acc) { const r = OB.st && OB.st.rates && OB.st.rates[(acc || "").toUpperCase()]; return r ? r.rate : 0; }
function obProfile() { return profiles && profiles.profiles && profiles.profiles.find(p => p.id === OB_PROFILE); }
function obCombo() {
  const p = obProfile(), acc = (p && p.accelerator) || OB_GPU, rate = obRate(acc);
  return { title: "RealVisXL 生图", acc, rate, size: p ? p.size_gb_default : 6.94, have: !!p };
}

// mode switch, the same way the 网关/本地 switch in the top bar does it
async function obSetMode(m) {
  if ((appCfg.mode || "gateway") === m) return;
  const c = await api("/api/config"); const cfg2 = c.config; cfg2.mode = m;
  await api("/api/config", { json: cfg2 });
  appCfg.mode = m; if (typeof renderMode === "function") renderMode();
  gst = null; await pollStatus(); await loadProfiles(); connectEvents();
}
async function obPatchConfig(patch) {
  const c = await api("/api/config"); await api("/api/config", { json: { ...c.config, ...patch } });
  await loadSettings();
}

// ---- open / close ----
function obOpen(path, step) {
  OB.path = path; OB.step = step || 1;
  $("#ob").hidden = false; document.body.classList.add("ob-open");
  obSave({ step: `${path}:${OB.step}` });
  obRender();
  if (!OB.net && !OB.netBusy) obNet(); // runs in the background; the simple path only shows failures
}
function obGo(step) { OB.step = step; obSave({ step: `${OB.path}:${step}` }); obRender(); }
function obClose(done) {
  $("#ob").hidden = true; document.body.classList.remove("ob-open");
  if (done) obSave({ onboarded: true, step: OB.st && OB.st.step === "coach" ? "coach" : "" });
}

async function obNet() {
  OB.netBusy = true; obRender();
  try { OB.net = await api("/api/netcheck"); } catch (e) { OB.net = { ok: false, items: [], advice: e.message, proxy: "" }; }
  OB.netBusy = false; obRender(); obLoad().then(obRenderChecklist);
}

// ---- rendering ----
function obRender() {
  const box = $("#ob"); if (box.hidden) return;
  const full = OB.path === "full";
  const body = full ? obFullStep() : obSimpleStep();
  box.innerHTML = `<div class="ob-card ${full ? "full" : "simple"}">
    <button class="ob-x" id="obX" title="关闭（下次从这里继续）">×</button>
    ${full ? `<ol class="ob-prog">${OB_FULL.map((n, i) => `<li class="${i + 1 < OB.step ? "done" : i + 1 === OB.step ? "on" : ""}"><i>${i + 1 < OB.step ? "✓" : i + 1}</i><span>${n}</span></li>`).join("")}</ol>` : ""}
    <div class="ob-body">${body}</div>
    ${obFooter()}
  </div>`;
  $("#obX").onclick = () => obClose(false);
  obBind();
}

function obFooter() {
  if (OB.path === "simple") {
    if (OB.step === 1) return `<div class="ob-foot"><a href="#" id="obSkipAll" class="muted small">跳过引导，我自己看看</a></div>`;
    return `<div class="ob-foot"><button class="btn ghost sm" id="obBack">上一步</button><span class="grow"></span><a href="#" id="obSkipAll" class="muted small">跳过引导</a></div>`;
  }
  const last = OB.step === OB_FULL.length;
  return `<div class="ob-foot">${OB.step > 1 ? `<button class="btn ghost sm" id="obBack">上一步</button>` : ""}<span class="grow"></span>
    ${last ? "" : `<button class="btn ghost sm" id="obSkip">跳过这一步</button>`}
    ${OB.step === 3 || OB.step === 6 ? "" : `<button class="btn primary sm" id="obNext">${last ? "完成" : "下一步"}</button>`}</div>`;
}

function obNetHTML(compact) {
  if (OB.netBusy) return `<div class="ob-net"><span class="spin"></span> 正在检测网络…</div>`;
  const n = OB.net; if (!n) return "";
  if (compact) return n.ok ? "" : `<p class="warn-note">网络检测：${n.items.filter(i => !i.ok).map(i => esc(i.name)).join("、")} 连不上。${esc(n.advice)} <a href="#" data-obnet>重新检测</a></p>`;
  return `<div class="ob-net"><div class="muted small">当前线路：${esc(n.proxy || "—")}</div>
    ${n.items.map(i => `<div class="ob-net-row ${i.ok ? "ok" : "bad"}"><b>${i.ok ? "✓" : "✗"}</b><span class="mono">${esc(i.name)}</span><span class="muted small">${esc(i.why)}</span><span class="grow"></span><span class="mono small">${i.ok ? i.ms + " ms" : esc(i.error || "")}</span></div>`).join("")}
    ${n.ok ? `<p class="ok-note">网络正常。</p>` : `<p class="warn-note">${esc(n.advice)}</p>`}
    <button class="btn sm" data-obnet>重新检测</button></div>`;
}

function obLoginHTML() {
  const st = OB.st || {};
  if (st.signed_in) return `<div class="ok-note">已登录 ${esc(st.email || "Google")}</div>${obCCUHTML()}`;
  return `<ol class="ob-howto">
      <li>浏览器里已打开 Google 授权页：登录你的 Google 账号，点「允许」。</li>
      <li>页面会显示一串<b>授权码</b>，点它旁边的「复制」。</li>
      <li>回到这里，点下面的按钮。</li></ol>
    <button class="btn primary xl" id="obPaste">${OB.clip ? "检测到授权码 · 一键登录" : "粘贴授权码并登录"}</button>
    <details class="sub" ${OB.clipDenied ? "open" : ""}><summary>手动粘贴</summary>
      <div class="row gap"><input id="obCode" class="grow mono" placeholder="在这里按 Ctrl+V 粘贴授权码" autocomplete="off"><button class="btn sm" id="obCodeGo">登录</button></div></details>
    <p class="hint"><a href="#" id="obReopen">浏览器没打开？再打开一次</a></p>
    <p class="err" id="obErr"></p>`;
}

function obCCUHTML() {
  const c = OB.ccu;
  if (!c) return `<p class="muted small" id="obCCU"><span class="spin"></span> 正在查询 CU 余额…</p>`;
  if (c.error) return `<p class="muted small">CU 余额暂时查不到：${esc(c.error)}</p>`;
  const r = obRate("G4"), hours = r ? c.balance / r : 0;
  if (c.balance < 5) return `<p class="warn-note">CU 余额 ${c.balance.toFixed(1)}，不够开机。<a href="#" data-url="${OB_LINKS.buy}">去购买 Colab 算力 →</a></p>`;
  return `<p class="muted">CU 余额 <b>${c.balance.toFixed(1)}</b>${OB.path === "full" ? ` · 约可用 ${hours.toFixed(0)} 小时（按 G4 估算）` : ""}</p>`;
}

function obSimpleStep() {
  const s = OB.step;
  if (s === 1) return `<div class="ob-hero">
      <div class="ob-logo">▶</div>
      <h1>开始使用 Wakeloom</h1>
      <p class="ob-lead">用你自己的 Colab 算力，一键生成视频、图片，和大模型对话</p>
      <button class="btn primary xl" id="obStart">用 Google 账号开始</button>
      <p class="muted small">需要 Colab 付费算力（CU）：Colab Pro / Pro+ 或按量购买的 CU</p>
      <a href="#" id="obGateway" class="small">我有自己的网关 →</a>
      ${obNetHTML(true)}</div>`;
  if (s === 2) return `<div class="ob-hero left"><h2>登录 Google</h2>${obLoginHTML()}${obNetHTML(true)}</div>`;
  const c = obCombo();
  return `<div class="ob-hero"><h2>一键开机</h2>
      <div class="ob-combo"><b>${c.title} · ${esc(c.acc)}</b>
        <span>约 ${c.rate ? c.rate.toFixed(1) : "?"} CU/小时 · 5～8 分钟开机</span>
        <span class="muted small">第一次要下载约 ${c.size ?? "?"} GB 模型 · 闲置 30 分钟自动停机</span></div>
      ${obCCUHTML()}
      ${c.have ? "" : `<p class="warn-note">没找到 RealVisXL 启动配置。网关模式需要先给网关打 gate-ai.py 补丁。</p>`}
      <button class="btn primary xl" id="obLaunch" ${OB.ccu && OB.ccu.balance < 1 ? "disabled" : ""}>启动 Colab</button>
      <p class="muted small">点了就开始计费，用完记得「停止」</p></div>`;
}

function obFullStep() {
  const st = OB.st || {}, local = (appCfg.mode || "gateway") === "local";
  switch (OB.step) {
    case 1: return `<h2>欢迎</h2><p class="ob-lead">用你自己的 Colab 算力，一键生成视频、图片，和大模型对话。</p>
      <h3>需要准备</h3><ul class="ob-list">
        <li><b>Google 账号，并且有 Colab 付费算力（CU）</b>：Colab Pro / Pro+ 或按量购买的 CU。免费版只有 T4，跑不了大模型。</li>
        <li><b>能访问 Google、Hugging Face、GitHub 的网络</b>。国内需要开代理，软件会自动走系统代理。</li></ul>
      <p><a href="#" data-url="${OB_LINKS.cu}">什么是 CU？</a> · <a href="#" data-url="${OB_LINKS.guide}">新手指南</a></p>`;
    case 2: return `<h2>检查网络</h2><p class="muted">检测能否连上 Google、Colab、Hugging Face 和 GitHub。检测失败也可以继续，但对应功能可能用不了。</p>${obNetHTML(false)}`;
    case 3: return `<h2>选择连接方式</h2><div class="ob-modes">
        <button class="ob-mode ${local ? "on" : ""}" data-mode="local"><span class="pill sm">推荐新用户</span><b>本地模式</b>
          <span>不需要服务器，用 Google 账号直接开关 Colab。</span>
          <span class="muted small">实验性：用的是 Colab 的非公开接口，可能随时变化，也有账号被限制的风险。<a href="#" data-url="${OB_LINKS.local}">了解风险</a></span></button>
        <button class="ob-mode ${!local && st.gate_url ? "on" : ""}" data-mode="gateway"><b>网关模式</b>
          <span>已经在 VPS 上部署了 comfy-colab-kit 网关的用户选这个，支持多人共用。</span>
          <span class="muted small">网关需要打补丁才能用全部功能。<a href="#" data-url="${OB_LINKS.gateway}">网关说明</a></span></button></div>`;
    case 4: return local ? `<h2>登录 Google</h2>${obLoginHTML()}
        ${st.signed_in ? `<div class="ob-smoke"><button class="btn sm" id="obSmoke">测试开机</button><span class="muted small">开一台 T4 跑几行代码再关掉，会花约 1～2 分钟和少量 CU</span><pre class="g-log" id="obSmokeLog" hidden></pre></div>` : ""}`
      : `<h2>连接网关</h2>
        <label class="fld">网关地址<input id="obGate" value="${esc(st.gate_url || "")}" placeholder="https://comfy.你的域名"><span class="hint-in">你部署的 comfy-colab-kit 网关的网址</span></label>
        <label class="fld">Agent 令牌<input id="obTok" type="password" autocomplete="off" placeholder="${appCfg.token ? "已保存（不改就留空）" : ""}"><span class="hint-in">网关网页右上角「复制 AI Agent 接入指令」里，或 VPS 上的 secrets/agent_token</span></label>
        <details class="sub"><summary>Cloudflare Access 服务令牌（可选）</summary>
          <label class="fld">CF-Access-Client-Id<input id="obCfId" value="${esc(appCfg.cf_client_id || "")}"></label>
          <label class="fld">CF-Access-Client-Secret<input id="obCfSec" type="password" autocomplete="off"><span class="hint-in">网关域名在 Access 后面时才需要</span></label></details>
        <div class="row gap"><button class="btn sm" id="obGateTest">测试连接</button><span class="small" id="obGateMsg"></span></div>
        <p class="hint">网关需要打补丁才支持选 GPU、AI 页和添加模型：<a href="#" data-url="${OB_LINKS.gateway}">gateway/README.md</a></p>`;
    case 5: return `<h2>基本设置</h2>
        <label class="fld">结果保存位置<input id="obOut" value="${esc(st.out_dir || st.default_out_dir || "")}"><span class="hint-in">生成的视频、图片会自动下载到这里</span></label>
        ${local ? `<label class="fld narrow">空闲多久自动停机（分钟）<input id="obIdle" type="number" min="5" max="600" value="${st.local_idle_min || 30}"></label>
          <p class="hint" style="margin-top:-6px">GPU 开着就一直扣 CU。没人用时自动停机，省 CU。推荐 30 分钟。</p>`
        : `<p class="hint">网关模式的空闲停机在网关上设置（.env 的 IDLE_MINUTES 等）。</p>`}
        <p class="hint">「AI 写提示词」要用的大模型 API 可以跳过，以后在「设置 → 大模型 API」里添加。</p>`;
    case 6: { const c = obCombo(); return `<h2>第一次试用</h2>
        <p>推荐最省钱的组合：</p>
        <div class="ob-combo"><b>RealVisXL V5.0 · ${esc(c.acc)}</b><span>约 5～8 分钟开机 · 每小时约 ${c.rate ? c.rate.toFixed(1) : "?"} CU</span><span class="muted small">第一次要下载约 ${c.size ?? "?"} GB 模型</span></div>
        ${c.have ? "" : `<p class="warn-note">没找到 RealVisXL 启动配置：网关需要先打 gate-ai.py 补丁。也可以先跳过。</p>`}
        <div class="row gap center"><button class="btn primary" id="obLaunch" ${c.have ? "" : "disabled"}>唤醒并试一张图</button><button class="btn" id="obLater">先跳过，我自己看看</button></div>`; }
    default: return `<h2>完成</h2><p class="ob-lead">准备好了。接下来带你认识一下界面。</p>
        <p class="muted">用完记得停止 GPU，不然会一直扣 CU（空闲也会自动停机）。</p>
        <button class="btn" id="obTour">开始导览</button>`;
  }
}

// ---- events ----
function obBind() {
  $$("#ob [data-url]").forEach(a => a.onclick = e => { e.preventDefault(); obOpenURL(a.dataset.url); });
  $$("#ob [data-obnet]").forEach(a => a.onclick = e => { e.preventDefault(); obNet(); });
  const on = (id, f) => { const el = $("#" + id); if (el) el.onclick = e => { e.preventDefault(); f(); }; };
  on("obSkipAll", () => obClose(true));
  on("obBack", () => obGo(Math.max(1, OB.step - 1)));
  on("obSkip", () => obGo(OB.step + 1));
  on("obNext", obNext);
  on("obGateway", () => obOpen("full", 1));
  on("obStart", obStart);
  on("obPaste", obPaste);
  on("obCodeGo", () => obLogin($("#obCode").value));
  on("obReopen", () => api("/api/local/login", { json: {} }).catch(e => obErr(e.message)));
  on("obLaunch", obLaunch);
  on("obLater", () => obGo(7));
  on("obTour", () => { obClose(true); obCoach(OB_TOUR_FULL); });
  on("obGateTest", obGateTest);
  on("obSmoke", obSmoke);
  const code = $("#obCode"); if (code) code.onpaste = () => setTimeout(() => obLogin(code.value), 30);
  $$("#ob [data-mode]").forEach(b => b.onclick = async () => { await obSetMode(b.dataset.mode); obGo(4); });
  if (OB.step === 2 && OB.path === "full" && !OB.net && !OB.netBusy) obNet();
  if ((OB.step === 3 && OB.path === "simple" || OB.step === 4 && OB.path === "full") && OB.st && OB.st.signed_in && !OB.ccu) obCCU();
}
function obErr(m) { const e = $("#obErr"); if (e) e.textContent = m; else toast(m); }

async function obNext() {
  if (OB.path === "full" && OB.step === 5) {
    const patch = { out_dir: ($("#obOut").value || "").trim() };
    if ($("#obIdle")) patch.local_idle_min = parseInt($("#obIdle").value) || 30;
    try { await obPatchConfig(patch); } catch (e) { return toast("保存失败：" + e.message); }
  }
  if (OB.step === OB_FULL.length) { obClose(true); return; }
  obGo(OB.step + 1);
}

async function obStart() { // click 1: local mode + Google sign-in page
  try {
    await obSetMode("local");
    await obLoad();
    if (!OB.st.signed_in) await api("/api/local/login", { json: {} });
  } catch (e) { return toast(e.message); }
  obGo(OB.st.signed_in ? 3 : 2);
}

// Google's code looks like 4/0Ab…; offer it as soon as the user comes back from the browser
async function obPeekClipboard() {
  if ($("#ob").hidden || !$("#obPaste") || !navigator.clipboard || !navigator.clipboard.readText) return;
  try {
    const t = (await navigator.clipboard.readText()).trim();
    if (/^4\/[\w-]{20,}$/.test(t) || /[?&]code=4(%2F|\/)/.test(t)) { if (OB.clip !== t) { OB.clip = t; obRender(); } }
  } catch { }
}
window.addEventListener("focus", () => setTimeout(obPeekClipboard, 150));

async function obPaste() { // click 2
  let t = OB.clip;
  if (!t) {
    try { t = (await navigator.clipboard.readText()).trim(); } catch { OB.clipDenied = true; obRender(); const c = $("#obCode"); if (c) c.focus(); return obErr("没能读取剪贴板，请在下面的框里按 Ctrl+V 粘贴授权码"); }
  }
  if (!t) return obErr("剪贴板是空的：先在授权页点「复制」");
  obLogin(t);
}

async function obLogin(code) {
  code = (code || "").trim(); if (!code) return;
  const b = $("#obPaste"); if (b) { b.disabled = true; b.textContent = "登录中…"; }
  try {
    await api("/api/local/code", { json: { code } });
    OB.clip = ""; await obLoad(); toast("已登录 " + (OB.st.email || ""));
    if (typeof loadLocal === "function") loadLocal();
    OB.ccu = null;
    if (OB.path === "simple") obGo(3); else obRender();
  } catch (e) { OB.clip = ""; obRender(); obErr("登录失败：" + e.message + "。请重新复制授权码再试"); }
}

async function obCCU() {
  OB.ccu = null;
  try { const r = await api("/api/local/ccu", { json: {} }); OB.ccu = { balance: r.balance || 0, per_hour: r.per_hour }; }
  catch (e) { OB.ccu = { error: e.message, balance: 99 }; }
  obRender();
}

async function obLaunch() { // click 3: wake RealVisXL on L4, go to the AI page's image panel
  const b = $("#obLaunch"); if (b) { b.disabled = true; b.textContent = "正在启动…"; }
  try {
    if (!profiles) await loadProfiles();
    const p = obProfile();
    if (!p) throw new Error("没找到 RealVisXL 启动配置");
    $("#wakeProfile").value = p.id; $("#wakeProfile").dispatchEvent(new Event("change"));
    const acc = $("#wakeAcc");
    if (acc && !acc.disabled && [...acc.options].some(o => o.value === p.accelerator)) { acc.value = p.accelerator || OB_GPU; acc.dataset.touched = "1"; }
    await wake();
  } catch (e) { if (b) { b.disabled = false; b.textContent = "启动 Colab"; } return toast("启动失败：" + e.message); }
  await obSave({ onboarded: true, step: "coach" });
  obClose(false);
  OB.mini = true; OB.prefill = true;
  show("ai"); if (typeof AI !== "undefined") AI.key = "";
  obMiniRender();
}

async function obGateTest() {
  const msg = $("#obGateMsg"); msg.textContent = "测试中…";
  const patch = { mode: "gateway", gate_url: $("#obGate").value.trim(), cf_client_id: $("#obCfId").value.trim() };
  if ($("#obTok").value.trim()) patch.token = $("#obTok").value.trim();
  if ($("#obCfSec").value.trim()) patch.cf_client_secret = $("#obCfSec").value.trim();
  try {
    await obPatchConfig(patch);
    await api("/api/gate/test", { json: {} });
    await loadProfiles();
    const n = profiles && profiles.profiles ? profiles.profiles.length : 0;
    msg.innerHTML = `<span class="ok-text">✓ 连接成功 · 网关上有 ${n} 个启动配置</span>`;
    obLoad();
  } catch (e) { msg.innerHTML = `<span class="err-text">✗ ${esc(e.message)}</span>`; }
}

async function obSmoke() {
  const log = $("#obSmokeLog"); log.hidden = false; log.textContent = ""; $("#obSmoke").disabled = true;
  try {
    const r = await fetch("/api/local/smoke", { method: "POST", headers: { "X-Key": KEY, "Content-Type": "application/json" }, body: JSON.stringify({ accelerator: "T4" }) });
    const rd = r.body.getReader(), dec = new TextDecoder(); let buf = "";
    for (; ;) {
      const { value, done } = await rd.read(); if (done) break;
      buf += dec.decode(value, { stream: true });
      let i; while ((i = buf.indexOf("\n\n")) >= 0) {
        const ev = buf.slice(0, i); buf = buf.slice(i + 2);
        const d = (ev.match(/^data: (.*)$/m) || [])[1]; if (d == null) continue;
        let v = d; try { v = JSON.parse(d); } catch { }
        log.textContent += (/^event: fail/m.test(ev) ? "✗ " : /^event: ok/m.test(ev) ? "✓ " : "") + v + "\n"; log.scrollTop = log.scrollHeight;
      }
    }
  } catch (e) { log.textContent += "✗ " + e.message; }
  $("#obSmoke").disabled = false;
}

// ---- floating mini window while the first GPU boots ----
function obMiniRender() {
  const box = $("#obMini");
  const s = gst && gst.state;
  if (!OB.mini || !s || s === "sleeping") { box.hidden = true; return; }
  if (s === "ready") {
    box.hidden = true; OB.mini = false;
    if (OB.st && OB.st.step === "coach") obCoach(OB_TOUR_SIMPLE);
    return;
  }
  if (s === "failed") { box.hidden = false; box.innerHTML = `<b class="err-text">开机失败</b><span class="small">${esc((gst.reason || "").slice(0, 120))}</span><span class="small">看控制台的启动日志。</span><button class="btn xs" id="obMiniX">关闭</button>`; $("#obMiniX").onclick = () => { OB.mini = false; box.hidden = true; }; return; }
  const d = gst.progress && gst.progress.download;
  box.hidden = false;
  box.innerHTML = `<div class="row gap"><span class="spin"></span><b>${esc(STATE_CN[s] || s)}</b><span class="grow"></span><button class="ob-x sm" id="obMiniX" title="收起">×</button></div>
    ${d && d.total_gb ? `<div class="bar"><div style="width:${Math.min(100, (d.done_gb / d.total_gb) * 100).toFixed(1)}%"></div></div><span class="small mono">${(d.done_gb || 0).toFixed(1)} / ${d.total_gb.toFixed(1)} GB</span>` : ""}
    <span class="small muted">首次要下载模型，请耐心等待（约 5～8 分钟）。就绪后就能在这里生成。</span>`;
  $("#obMiniX").onclick = () => { OB.mini = false; box.hidden = true; };
}

function obPrefill() {
  if (!OB.prefill) return;
  const t = $("#aiPrompt");
  if (t && !$("#aiImg").hidden) { if (!t.value.trim()) t.value = OB_SAMPLE; OB.prefill = false; }
}

// ---- coach marks ----
const OB_TOUR_SIMPLE = [
  { sel: "#aiGen", text: "试试点「生成」：几十秒就能出第一张图，结果在下面和「结果」页。", page: "ai" },
  { sel: 'nav button[data-page="gen"]', text: "生成：导入 ComfyUI 工作流，批量生成视频和图片。" },
  { sel: 'nav button[data-page="ai"]', text: "AI：内置生图和大模型对话，「＋ 添加模型」可以加 Hugging Face 上的模型。" },
  { sel: "#btnStopTop", text: "用完记得停止，不然会一直扣 CU（空闲也会自动停机）。" },
];
const OB_TOUR_FULL = [
  { sel: 'nav button[data-page="console"]', text: "控制台：唤醒 / 停止 GPU，看启动日志、计时和 CU。" },
  { sel: 'nav button[data-page="gen"]', text: "生成：导入 ComfyUI API 工作流，填参数，排队生成。" },
  { sel: 'nav button[data-page="ai"]', text: "AI：内置生图和大模型对话，也能添加自己的模型。" },
  { sel: 'nav button[data-page="queue"]', text: "队列：任务按顺序执行，可以取消、重试。" },
  { sel: 'nav button[data-page="results"]', text: "结果：生成的文件会自动下载到你的电脑。" },
  { sel: 'nav button[data-page="settings"]', text: "设置：连接方式、保存位置、自动停机、自定义模型。" },
  { sel: "#btnStopTop, #btnWakeTop", text: "用完记得停止，不然会一直扣 CU（空闲也会自动停机）。" },
];
function obCoach(list) {
  OB.coachList = list.filter(x => x); OB.coachI = 0; OB.coachOn = true; obCoachShow();
}
function obCoachShow() {
  const box = $("#obCoach");
  $$(".ob-hl").forEach(e => e.classList.remove("ob-hl"));
  const it = OB.coachList[OB.coachI];
  if (!it) { box.hidden = true; box.innerHTML = ""; OB.coachOn = false; if (OB.st && OB.st.step === "coach") obSave({ step: "" }); return; }
  if (it.page && page !== it.page) show(it.page);
  const el = $$(it.sel).find(e => e.offsetParent !== null);
  if (!el) { OB.coachI++; return obCoachShow(); }
  el.classList.add("ob-hl"); el.scrollIntoView({ block: "nearest" });
  const r = el.getBoundingClientRect(), last = OB.coachI === OB.coachList.length - 1;
  box.hidden = false;
  box.innerHTML = `<p>${esc(it.text)}</p><div class="row gap"><span class="muted small">${OB.coachI + 1} / ${OB.coachList.length}</span><span class="grow"></span>
    ${last ? "" : `<button class="btn ghost xs" id="obCoachSkip">跳过</button>`}<button class="btn primary xs" id="obCoachNext">${last ? "知道了" : "下一个"}</button></div>`;
  const bw = box.offsetWidth, bh = box.offsetHeight, vw = innerWidth, vh = innerHeight;
  let x = r.right + 12, y = r.top + r.height / 2 - bh / 2;
  if (x + bw > vw - 12) { x = Math.min(vw - bw - 12, Math.max(12, r.left + r.width / 2 - bw / 2)); y = r.bottom + 10 + bh > vh ? r.top - bh - 10 : r.bottom + 10; }
  box.style.left = Math.max(12, x) + "px"; box.style.top = Math.max(12, Math.min(vh - bh - 12, y)) + "px";
  box.dataset.side = x > r.right ? "left" : "top";
  $("#obCoachNext").onclick = () => { OB.coachI++; obCoachShow(); };
  const sk = $("#obCoachSkip"); if (sk) sk.onclick = () => { OB.coachI = OB.coachList.length; obCoachShow(); };
}
addEventListener("resize", () => { if (OB.coachOn) obCoachShow(); });

// ---- getting-started checklist (console) ----
function obRenderChecklist() {
  const box = $("#obChecklist"); if (!box) return;
  const st = OB.st;
  if (!st || !st.onboarded || st.all_done) { box.hidden = true; return; }
  const done = st.checklist.filter(c => c.done).length, open = !st.checklist_hidden;
  box.hidden = false;
  box.innerHTML = `<div class="row gap ob-ck-head" id="obCkToggle"><b>入门清单</b><span class="muted small">${done} / ${st.checklist.length}</span>
      <div class="ob-ck-bar"><div style="width:${done / st.checklist.length * 100}%"></div></div><span class="grow"></span><span class="muted small">${open ? "收起 ▴" : "展开 ▾"}</span></div>
    ${open ? `<ul class="ob-ck">${st.checklist.map(c => `<li class="${c.done ? "done" : ""}"><button data-ck="${c.id}" ${c.done ? "disabled" : ""}><i>${c.done ? "✓" : ""}</i>${OB_CHECK[c.id]}</button></li>`).join("")}</ul>` : ""}`;
  $("#obCkToggle").onclick = () => obSave({ checklist_hidden: open });
  $$("[data-ck]", box).forEach(b => b.onclick = () => obCheckAction(b.dataset.ck));
}
function obCheckAction(id) {
  if (id === "network") return obOpen("full", 2);
  if (id === "login") return isLocal() ? obOpen("full", 4) : (show("settings"), setTimeout(() => $("#setForm [name=gate_url]").focus(), 100));
  if (id === "wake") return obOpen(OB.path === "full" ? "full" : "simple", OB.path === "full" ? 6 : 3);
  if (id === "generate") return show("ai");
  if (id === "stop") { show("console"); $("#btnStop").scrollIntoView({ block: "center" }); return toast("GPU 开着时，点「停止并释放」"); }
}

// ---- help menu ----
$("#btnHelp").onclick = e => { e.stopPropagation(); $("#helpMenu").hidden = !$("#helpMenu").hidden; };
document.addEventListener("click", e => { if (!e.target.closest("#helpMenu")) $("#helpMenu").hidden = true; });
$("#helpGuide").onclick = () => { $("#helpMenu").hidden = true; obOpen("full", 1); };
$("#helpReadme").onclick = () => { $("#helpMenu").hidden = true; obOpenURL(OB_LINKS.readme); };
$("#helpNet").onclick = () => { $("#helpMenu").hidden = true; OB.net = null; obOpen("full", 2); };
$("#btnRerunGuide").onclick = () => obOpen("full", 1);

// ---- estimate next to the wake button ----
function obWakeCost() {
  const el = $("#wakeCost"); if (!el) return;
  const sel = $("#wakeAcc"), p = profiles && profiles.profiles && profiles.profiles.find(x => x.id === $("#wakeProfile").value);
  const acc = sel && !sel.disabled && !$("#accWrap").hidden ? sel.value : (p && p.accelerator) || (gst && gst.default_accelerator) || "";
  const r = obRate(acc), m = OB.st && OB.st.rates && OB.st.rates[acc] && OB.st.rates[acc].measured;
  const asleep = !gst || !gst.state || gst.state === "sleeping" || gst.state === "failed";
  el.textContent = asleep && r ? `预计每小时约 ${r.toFixed(1)} CU（${acc}${m ? "，按上次实际" : "，估计值"}）` : "";
}
$("#wakeAcc").addEventListener("change", obWakeCost);
$("#wakeProfile").addEventListener("change", obWakeCost);

// follow status changes: mini window, prefill, checklist, cost
const _obRS = renderStatus;
renderStatus = function () {
  _obRS(); obMiniRender(); obPrefill(); obWakeCost();
  if (gst && gst.state === "ready" && OB.st && OB.st.step === "coach" && !OB.coachOn && !OB.mini) obCoach(OB_TOUR_SIMPLE);
};
setInterval(() => { if (page === "console" && OB.st && OB.st.onboarded && !OB.st.all_done) obLoad().then(obRenderChecklist); }, 20000);

// ---- start ----
(async () => {
  const st = await obLoad();
  if (!st) return;
  if (!st.onboarded) {
    const [p, s] = (st.step || "simple:1").split(":");
    obOpen(p === "full" ? "full" : "simple", Math.max(1, Math.min(p === "full" ? 7 : 3, +s || 1)));
  }
  obRenderChecklist(); obWakeCost();
})();
