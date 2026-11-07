"use strict";
const KEY = new URLSearchParams(location.search).get("k") || "";
const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));

async function api(path, opts = {}) {
  const o = { ...opts, headers: { "X-Key": KEY, ...(opts.headers || {}) } };
  if (o.json !== undefined) { o.body = JSON.stringify(o.json); o.headers["Content-Type"] = "application/json"; o.method = o.method || "POST"; }
  const r = await fetch(path, o);
  let data = null;
  try { data = await r.json(); } catch { }
  if (!r.ok) throw new Error((data && data.error) || ("HTTP " + r.status));
  return data;
}
const fileURL = p => "/api/file?k=" + KEY + "&path=" + encodeURIComponent(p);
function toast(msg) { const t = $("#toast"); t.textContent = msg; t.classList.add("on"); clearTimeout(t._h); t._h = setTimeout(() => t.classList.remove("on"), 2600); }
function fmtDur(s) { if (s == null) return "—"; s = Math.max(0, Math.round(s)); const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60); return h ? `${h} 小时 ${m} 分` : `${m} 分 ${s % 60} 秒`; }
function fmtTime(t) { if (!t) return ""; const d = new Date(t * 1000); return d.toLocaleString("zh-CN", { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", second: "2-digit" }); }
function fmtSize(b) { return b > 1 << 30 ? (b / (1 << 30)).toFixed(2) + " GB" : b > 1 << 20 ? (b / (1 << 20)).toFixed(1) + " MB" : Math.round(b / 1024) + " KB"; }

// ---------------- navigation ----------------
let page = "console";
$$("nav button").forEach(b => b.onclick = () => show(b.dataset.page));
function show(p) {
  page = p;
  $$("nav button").forEach(b => b.classList.toggle("on", b.dataset.page === p));
  $$(".page").forEach(s => s.classList.toggle("on", s.id === "page-" + p));
  if (p === "results") loadFiles();
  if (p === "queue") loadJobs();
  if (p === "lib") loadLib();
  if (p === "settings") loadLocal();
  if (p === "ai" && typeof aiRender === "function") aiRender();
}

// ---------------- gate status ----------------
const STATE_CN = { sleeping: "休眠中", allocating: "分配机器", installing: "安装依赖", downloading: "下载模型", booting: "启动 ComfyUI", smoke_testing: "自检", ready: "就绪", switching: "切换配置", stopping: "停止中", failed: "启动失败" };
const BUSY = new Set(["allocating", "installing", "downloading", "booting", "smoke_testing", "switching", "stopping"]);
const STEPS = [["allocating", "分配机器"], ["installing", "安装依赖"], ["downloading", "下载模型"], ["booting", "启动"], ["smoke_testing", "自检"], ["ready", "就绪"]];
const REASON_CN = { idle: "闲置超时自动停机", max_session: "达到最长会话时长", page_closed: "页面关闭后停机", quiet_hours: "静默时段", daily_budget: "超出每日 CU 预算", monthly_budget: "超出每月 CU 预算", low_balance: "CU 余额过低", boot_timeout: "启动超时", orphan: "清理孤立实例" };
let gst = null, gerr = "", kitReady = null;
const stepSeen = {}; // state -> first local time seen in this session
let stepSession = null;

async function pollStatus() {
  try { gst = await api("/api/gate/status"); gerr = ""; }
  catch (e) { gst = null; gerr = e.message; }
  renderStatus();
  const k = (gst ? "1" : "0") + (gst && gst.default_accelerator || "");
  if (!isLocal() && k !== pollStatus.accKey) { pollStatus.accKey = k; renderExtras(); }
}
function clock(sec) {
  if (sec == null || isNaN(sec)) return "--:--";
  sec = Math.max(0, Math.floor(sec));
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60), s = sec % 60;
  const p = n => String(n).padStart(2, "0");
  return h ? `${h}:${p(m)}:${p(s)}` : `${p(m)}:${p(s)}`;
}
function human(sec) {
  if (sec == null) return "—";
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + " 秒";
  const h = Math.floor(sec / 3600), m = Math.floor(sec % 3600 / 60);
  return h ? `${h} 小时 ${m} 分` : `${m} 分 ${sec % 60} 秒`;
}
function profileTitle(id) {
  const p = profiles && profiles.profiles && profiles.profiles.find(x => x.id === id);
  return p ? (p.title || p.id) : id;
}
function stateClass(s) { return s === "ready" ? "ok" : BUSY.has(s) ? "busy" : s === "failed" ? "bad" : "idle"; }

function renderStatus() {
  const s = gst && gst.state;
  const cls = gst ? stateClass(s) : "bad";
  const label = gst ? (STATE_CN[s] || s) : (gerr ? "未连接" : "—");
  const now = Date.now() / 1000;

  // chips / sidebar
  $("#dot").className = "dot s-" + cls;
  $("#stateChip").innerHTML = `<span class="dot s-${cls}"></span>GPU · ${esc(label)}`;
  $("#sideStateText").textContent = label;
  $("#dotBig").className = "ring " + cls;
  $("#stateBig").textContent = label;

  // subtitle: profile pill (+ stop reason when asleep)
  let sub = "";
  if (gst && gst.profile && s !== "sleeping") sub += `<span class="pill">${esc(profileTitle(gst.profile))}</span>`;
  if (gst && (s === "sleeping" || s === "failed") && gst.reason) {
    const key = Object.keys(REASON_CN).find(k => gst.reason.includes(k));
    sub += `<span class="muted">${s === "failed" ? "失败原因" : "上次停止"}：${esc(key ? REASON_CN[key] : gst.reason)}</span>`;
  }
  if (!gst) sub = `<span class="muted">${esc(gerr.includes("网关地址") ? "先到「设置」填写网关地址" : "连不上网关")}</span>`;
  $("#stateSub").innerHTML = sub;

  // stepper & stage times
  const started = gst && gst.session_started;
  if (started && stepSession !== started) { stepSession = started; for (const k in stepSeen) delete stepSeen[k]; kitReady = kitReady && kitReady.session === started ? kitReady : null; }
  if (s && !stepSeen[s]) stepSeen[s] = s === "ready" && gst.since ? gst.since : now;
  const idx = STEPS.findIndex(x => x[0] === s);
  const showSteps = gst && (BUSY.has(s) || s === "ready" || s === "failed") && s !== "stopping";
  $("#steps").hidden = !showSteps;
  if (showSteps) {
    $("#steps").innerHTML = STEPS.map(([k, n], i) => {
      const st = s === "failed" ? (i < Math.max(idx, 0) ? "done" : "") : i < idx ? "done" : i === idx ? (k === "ready" ? "done" : "cur") : "";
      return `<li class="${st}"><span class="b">${st === "done" ? "✓" : i + 1}</span><span>${n}</span></li>`;
    }).join("");
  }

  // timer
  let tv = "--:--", tl = "唤醒计时", tcls = "";
  if (gst && BUSY.has(s) && s !== "stopping" && started) { tv = clock(now - started); tl = "唤醒中 · 已用时"; tcls = "live"; }
  else if (gst && s === "ready" && started) {
    const boot = (gst.since && gst.since >= started) ? gst.since - started : null;
    tv = boot != null ? clock(boot) : "--:--"; tl = "本次唤醒用时";
    if (boot != null) try { localStorage.setItem("ckwb.lastBoot", String(boot)); } catch { }
  } else {
    let last = null; try { last = localStorage.getItem("ckwb.lastBoot"); } catch { }
    tv = last ? clock(+last) : "--:--"; tl = "上次唤醒用时";
  }
  $("#timerV").textContent = tv; $("#timerL").textContent = tl;
  $("#timerBox").className = "timer " + tcls;

  // download bar
  const dl = gst && gst.progress && gst.progress.download;
  const showDl = !!(dl && dl.total_gb > 0 && BUSY.has(s));
  $("#dlBox").hidden = !showDl;
  if (showDl) {
    $("#dlBar").style.width = Math.min(100, dl.done_gb / dl.total_gb * 100) + "%";
    $("#dlText").textContent = `下载模型 ${dl.done_gb.toFixed(1)} / ${dl.total_gb.toFixed(1)} GB · 文件 ${dl.files_done}/${dl.files_total}`;
    $("#dlEta").textContent = dl.gbps ? (dl.gbps / 8).toFixed(2) + " GB/s" : "";
  }

  // tiles
  const tiles = [];
  if (gst) {
    const running = !!started && s !== "sleeping";
    if (running) tiles.push(["已运行", clock(now - started), "开机于 " + new Date(started * 1000).toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit" })]);
    if (!running) { /* asleep: only CU tiles */ }
    else if (gst.remaining_s != null && s === "ready") {
      const total = gst.remaining_s + (now - started);
      tiles.push(["剩余时长", human(gst.remaining_s), `<div class="mini"><div style="width:${Math.max(0, Math.min(100, gst.remaining_s / total * 100))}%"></div></div>`, gst.warn ? "warn" : ""]);
    } else tiles.push(["剩余时长", "—", ""]);
    if (gst.cu) {
      tiles.push(["CU 余额", gst.cu.balance != null ? gst.cu.balance.toFixed(2) : "—", gst.cu.balance != null && gst.cu.per_hour ? "约可用 " + Math.floor(gst.cu.balance / gst.cu.per_hour) + " 小时" : ""]);
      if (gst.cu.today != null) tiles.push(["今日 / 本月", `${(gst.cu.today || 0).toFixed(1)} / ${(gst.cu.month || 0).toFixed(1)}`, "CU"]);
      else if (isLocal()) tiles.push(["GPU 型号", gst.accelerator || "—", "本地模式"]);
      tiles.push(["开机耗速", (gst.cu.per_hour || 0).toFixed(2), "CU / 小时"]);
    }
    if (!isLocal() && gst.default_accelerator) tiles.push(["GPU 型号", running ? gst.accelerator : ($("#wakeAcc").value || gst.default_accelerator), running ? "本次开机" : "下次唤醒"]);
    else if (gst.clients != null) tiles.push(["在线访客", gst.clients, ""]);
  }
  $("#tiles").innerHTML = tiles.map(([k, v, x, c]) => `<div class="tile ${c || ""}"><div class="k">${esc(k)}</div><div class="v">${esc(v)}</div><div class="x">${x || ""}</div></div>`).join("");
  $("#who").innerHTML = gst && gst.you ? `当前身份：${esc(gst.you.email || "agent")}${gst.you.owner ? ' <span class="pill sm">管理员</span>' : ""}` : "";
  $("#consoleErr").textContent = gerr && !gerr.includes("网关地址") ? "连接网关失败：" + gerr : "";

  // buttons
  const asleep = !gst || s === "sleeping" || s === "failed";
  const busy = gst && (gst.busy || BUSY.has(s));
  $("#wakeOpts").hidden = !asleep || !gst;
  $("#btnWake").hidden = !asleep; $("#btnWakeTop").hidden = !asleep;
  $("#btnWake").disabled = $("#btnWakeTop").disabled = !gst;
  $("#btnStop").hidden = asleep; $("#btnStopTop").hidden = asleep;
  $("#btnStop").disabled = $("#btnStopTop").disabled = s === "stopping";
  $("#btnExtend").hidden = s !== "ready";
  $("#btnExtend").disabled = gst && gst.can_extend === false;
  $("#btnOpenComfy").hidden = s !== "ready";
  $("#topInfo").textContent = s === "ready" && gst.remaining_s != null ? "剩余 " + human(gst.remaining_s) : busy && started ? "唤醒中 " + clock(now - started) : "";
}
setInterval(() => { if (gst && gst.session_started && gst.state !== "sleeping") renderStatus(); }, 1000);

let profiles = null;
async function loadProfiles() {
  try { profiles = await api("/api/gate/profiles"); } catch { profiles = null; }
  const sel = $("#wakeProfile");
  if (!profiles || !profiles.profiles) { sel.innerHTML = `<option value="">（网关默认）</option>`; $("#wfBox").innerHTML = ""; return; }
  const cur = sel.value || (gst && gst.profile) || profiles.default;
  sel.innerHTML = profiles.profiles.map(p => `<option value="${esc(p.id)}">${esc(p.title || p.id)} · 约 ${p.size_gb_default ?? "?"} GB</option>`).join("");
  if (cur) sel.value = cur;
  renderWf(); renderExtras();
  renderStatus();
}
function isLocal() { return (appCfg.mode || "gateway") === "local"; }
async function renderExtras() {
  const box = $("#extrasBox");
  if (!isLocal()) {
    const ok = !!(gst && gst.default_accelerator);   // gateway has the GPU-select patch
    $("#accWrap").hidden = !gst;
    $("#wakeAcc").disabled = !ok;
    if (!$("#wakeAcc").dataset.touched) {
      let last = ""; try { last = localStorage.getItem("ckwb.gwAcc") || ""; } catch { }
      $("#wakeAcc").value = ok ? (last || gst.default_accelerator) : ((gst && gst.accelerator) || "G4");
    }
    box.hidden = true;
    $("#localHint").hidden = ok || !gst;
    $("#localHint").innerHTML = ok ? "" : "网关还不支持在这里选 GPU，当前用的是 VPS 上 .env 的 COLAB_ACCELERATOR。按说明给网关打补丁后即可选择。";
    return;
  }
  $("#accWrap").hidden = false; $("#wakeAcc").disabled = false;
  const p = profiles && profiles.profiles && profiles.profiles.find(x => x.id === $("#wakeProfile").value);
  if (p && !$("#wakeAcc").dataset.touched) $("#wakeAcc").value = appCfg.local_accelerator || p.accelerator || "L4";
  let items = [];
  try { items = (await api("/api/gate/library")).items || []; } catch { }
  box.hidden = !items.length;
  box.innerHTML = items.length ? `<div class="eh"><span>这次开机顺便装（来自「库」）</span><span>勾的越多，唤醒越慢</span></div>` + items.map(it => `<label><input type="checkbox" value="${esc(it.id)}" ${it.auto_wake ? "checked" : ""}><span class="kind ${it.kind}" style="width:36px">${it.kind === "node" ? "插件" : "模型"}</span>${esc(it.title)}<span class="x">${it.size_gb ? it.size_gb.toFixed(2) + " GB" : ""}</span></label>`).join("") : "";
  let st = { signed_in: true }; try { st = await api("/api/local/status"); } catch { }
  $("#localHint").hidden = st.signed_in;
  $("#localHint").innerHTML = st.signed_in ? "" : "本地模式还没登录 Google：到「设置 → 10 本地模式」登录后才能唤醒。";
}
$("#wakeAcc").onchange = () => {
  $("#wakeAcc").dataset.touched = "1";
  if (!isLocal()) try { localStorage.setItem("ckwb.gwAcc", $("#wakeAcc").value); } catch { }  renderStatus();
};
function renderWf() {
  const p = profiles && profiles.profiles && profiles.profiles.find(x => x.id === $("#wakeProfile").value);
  const opt = p ? p.workflows.filter(w => w.optional) : [];
  $("#wfBox").innerHTML = opt.length ? `<span class="muted small">额外下载：</span>` + opt.map(w => `<label><input type="checkbox" value="${esc(w.id)}">${esc(w.title || w.id)} <span class="muted small">+${w.size_gb ?? "?"}GB</span></label>`).join("") : "";
}
$("#wakeProfile").onchange = () => { renderWf(); renderExtras(); };

// mode switch (gateway <-> local)
function renderMode() { $$("#modeSeg button").forEach(b => b.classList.toggle("on", b.dataset.m === (appCfg.mode || "gateway"))); }
$$("#modeSeg button").forEach(b => b.onclick = async () => {
  const m = b.dataset.m; if (m === (appCfg.mode || "gateway")) return;
  const running = gst && gst.state && gst.state !== "sleeping" && gst.state !== "failed";
  const active = jobs.some(j => ["running", "submitted", "uploading", "waiting", "queued"].includes(j.status));
  if ((running || active) && !confirm(`当前${(appCfg.mode || "gateway") === "local" ? "本地" : "网关"}模式的 GPU ${running ? "还开着" : ""}${active ? "，队列里还有任务" : ""}。切换后，那边的 GPU 不会自动停止，队列任务会改到新模式执行。确定切换？`)) return;
  const c = await api("/api/config"); const cfg2 = c.config; cfg2.mode = m; cfg2.cf_client_secret = cfg2.cf_client_secret; 
  await api("/api/config", { json: cfg2 });
  appCfg.mode = m; renderMode();
  $("#wakeAcc").dataset.touched = "";
  toast(m === "local" ? "已切换到本地模式（直连 Colab）" : "已切换到网关模式（VPS）");
  gst = null; await pollStatus(); loadProfiles(); connectEvents(); loadTemplates();
});

async function wake() {
  const body = {};
  const prof = $("#wakeProfile").value; if (prof) body.profile = prof;
  if (isLocal()) {
    body.accelerator = $("#wakeAcc").value;
    body.extras = $$("#extrasBox input:checked").map(i => i.value);
    const p = profiles && profiles.profiles.find(x => x.id === prof);
    const est = (p ? p.size_gb_default : 0);
    if (gst && gst.cu && gst.cu.balance != null && gst.cu.balance < 2 && !confirm(`CU 余额只剩 ${gst.cu.balance.toFixed(2)}，确定开机？`)) return;
    void est;
  } else if (gst && gst.default_accelerator && !$("#wakeAcc").disabled) {
    body.accelerator = $("#wakeAcc").value;
  }
  const extra = $$("#wfBox input:checked").map(i => i.value);
  if (extra.length && profiles) {
    const p = profiles.profiles.find(x => x.id === prof);
    body.workflows = p.workflows.filter(w => !w.optional).map(w => w.id).concat(extra);
  }
  try { await api("/api/gate/wake", { json: body }); toast("已发送唤醒"); connectEvents(); } catch (e) { toast("唤醒失败：" + e.message); }
  pollStatus();
}
async function stopGate() {
  if (!confirm("确定停止并释放 GPU？正在跑的任务会中断。")) return;
  const btns = [$("#btnStop"), $("#btnStopTop")];
  btns.forEach(b => { b.disabled = true; b.dataset.t = b.textContent; b.textContent = "备份工作流…"; });
  try {
    const r = await api("/api/gate/stop", { json: {} });
    if (r && r.backup_warning) alert("已停止。" + r.backup_warning); else toast("已停止");
  } catch (e) { toast("停止失败：" + e.message); }
  finally { btns.forEach(b => { b.disabled = false; if (b.dataset.t) b.textContent = b.dataset.t; }); }
  pollStatus(); if (typeof udRefresh === "function") udRefresh();
}
$("#btnWake").onclick = wake; $("#btnWakeTop").onclick = wake;
$("#btnStop").onclick = stopGate; $("#btnStopTop").onclick = stopGate;
$("#btnExtend").onclick = async () => { try { await api("/api/gate/extend", { json: {} }); toast("已延长"); } catch (e) { toast("延长失败：" + e.message); } pollStatus(); };
$("#btnOpenComfy").onclick = () => api("/api/open-url", { json: { url: "comfy" } });

// ---------------- boot log (pretty) ----------------
const STAGE_CN = { bootstrap: "引导", deps: "安装依赖", install_workflows: "安装工作流", download: "下载模型", comfy_boot: "启动 ComfyUI", comfy_check: "检查 ComfyUI", smoke: "自检" };
let rawLines = [];
function fmtLine(line) {
  let m;
  if ((m = line.match(/^KIT_PROGRESS download (\{.*\})\s*$/))) {
    try {
      const d = JSON.parse(m[1]);
      const gb = x => (x / 1e9).toFixed(1);
      return { cls: "prog", key: "dl", html: `<i>⬇</i>下载模型 <b>${gb(d.bytes_done)} / ${gb(d.bytes_total)} GB</b><span class="m">${(d.bytes_per_s / 1e9).toFixed(2)} GB/s · 文件 ${d.files_done}/${d.files_total}${d.eta_s != null ? " · 剩余 " + human(d.eta_s) : ""}</span>` };
    } catch { }
  }
  if ((m = line.match(/^KIT_STAGE (\S+) (start|done)(?: ([\d.]+))?/))) {
    const n = STAGE_CN[m[1]] || m[1];
    return m[2] === "start" ? { cls: "stage", html: `<i>›</i>${esc(n)}…` } : { cls: "ok", html: `<i>✓</i>${esc(n)} 完成${m[3] ? `<span class="m">${(+m[3]).toFixed(1)} 秒</span>` : ""}` };
  }
  if ((m = line.match(/^KIT_READY (\{.*\})/))) {
    try { const d = JSON.parse(m[1]); kitReady = { total: d.total_s, session: gst && gst.session_started }; return { cls: "ready", html: `<i>●</i>ComfyUI 已就绪<span class="m">机器内初始化 ${human(d.total_s)}</span>` }; } catch { }
  }
  if ((m = line.match(/^\s*ok (.+)$/))) return { cls: "ok", html: `<i>✓</i>${esc(m[1].split("/").pop())}<span class="m">${esc(m[1].includes("/") ? m[1].split("/")[0] : "")}</span>` };
  if (/^KIT_(TS|PARALLEL)\b/.test(line)) return null;
  if ((m = line.match(/^KIT_TUNNEL (\{.*\})/))) return { cls: "ok", html: `<i>✓</i>隧道已建立<span class="m">${esc((JSON.parse(m[1]).url || "").replace(/^https?:\/\//, ""))}</span>` };
  if ((m = line.match(/^KIT_LIB boot extras (\[.*\])/))) { let a = []; try { a = JSON.parse(m[1]); } catch { } return { cls: "stage", html: `<i>›</i>本次顺便安装 ${a.length} 项（来自库）` }; }
  if ((m = line.match(/^KIT_LIB (.*)$/))) return { cls: "stage", html: `<i>›</i>${esc(m[1].replace("restarting ComfyUI to load new custom nodes", "重启 ComfyUI 以加载新插件").replace("ComfyUI restarted", "ComfyUI 已重启"))}` };
  if (/^KIT_WARN no_models\b/.test(line)) return { cls: "bad", html: `<i>!</i>这个启动配置没有任何模型需要下载，请检查配置` };
  if ((m = line.match(/^WB_USERDATA (.*)$/))) return { cls: /失败/.test(m[1]) ? "bad" : "ok", html: `<i>${/失败/.test(m[1]) ? "!" : "✓"}</i>${esc(m[1])}` };
  if ((m = line.match(/^KIT_FETCHED (\{.*\})/))) return { cls: "ready", html: `<i>●</i>安装完成` };
  if (/error|traceback|failed|exception/i.test(line)) return { cls: "bad", html: esc(line) };
  return { cls: "dim", html: esc(line) };
}
function renderLog(full) {
  const box = $("#log");
  const atBottom = box.scrollTop + box.clientHeight >= box.scrollHeight - 24;
  if ($("#rawLog").checked) {
    box.classList.add("raw");
    box.textContent = rawLines.join("\n");
  } else {
    box.classList.remove("raw");
    if (full) box.innerHTML = "";
    const start = full ? 0 : rawLines.length - 1;
    for (let i = start; i < rawLines.length; i++) {
      const f = fmtLine(rawLines[i]); if (!f) continue;
      const last = box.lastElementChild;
      if (f.key && last && last.dataset.key === f.key) { last.innerHTML = f.html; continue; }
      const div = document.createElement("div");
      div.className = "ln " + f.cls; div.innerHTML = f.html; if (f.key) div.dataset.key = f.key;
      box.appendChild(div);
    }
    while (box.childElementCount > 1500) box.firstElementChild.remove();
  }
  if (atBottom || full) box.scrollTop = box.scrollHeight;
}
$("#rawLog").onchange = () => renderLog(true);
$("#btnClearLog").onclick = () => { rawLines = []; renderLog(true); };

let es = null;
function connectEvents() {
  if (es) es.close();
  es = new EventSource("/api/gate/events?k=" + KEY);
  rawLines = []; renderLog(true); // the gate replays its backlog on every connect
  es.addEventListener("log", e => {
    let line = e.data; try { line = JSON.parse(e.data); } catch { }
    if (typeof line !== "string") line = JSON.stringify(line);
    for (const l of line.split("\n")) { if (!l.trim()) continue; rawLines.push(l); if (rawLines.length > 4000) rawLines.splice(0, 1000); renderLog(false); }
  });
  es.addEventListener("state", e => { try { gst = { ...(gst || {}), ...JSON.parse(e.data) }; renderStatus(); } catch { } });
  es.addEventListener("progress", e => { try { if (gst) { gst.progress = JSON.parse(e.data); renderStatus(); } } catch { } });
  es.onerror = () => { es.close(); es = null; setTimeout(connectEvents, 15000); };
}

// ---------------- templates & form ----------------
let templates = [], curTpl = null, lastJobForm = null;
async function loadTemplates(selectId) {
  templates = await api("/api/templates");
  const list = $("#tplList");
  list.innerHTML = templates.length ? templates.map(t => `<button data-id="${t.id}"><b>${esc(t.name)}</b><small>${t.nodes} 个节点 · ${t.fields.filter(f => f.primary).length} 个常用参数</small></button>`).join("") : `<p class="muted small">还没有模板</p>`;
  $$("button", list).forEach(b => b.onclick = () => selectTpl(b.dataset.id));
  const id = selectId || (curTpl && curTpl.id) || localStorage.getItem("ckwb.tpl") || (templates[0] && templates[0].id);
  if (id && templates.find(t => t.id === id)) selectTpl(id); else { curTpl = null; renderForm(); }
}
function selectTpl(id, values) {
  curTpl = templates.find(t => t.id === id) || null;
  try { localStorage.setItem("ckwb.tpl", id); } catch { }
  $$("#tplList button").forEach(b => b.classList.toggle("on", b.dataset.id === id));
  renderForm(values);
}
$("#tplFile").onchange = async e => {
  const f = e.target.files[0]; e.target.value = "";
  if (!f) return;
  const text = await f.text();
  const name = prompt("给这个模板起个名字：", f.name.replace(/\.json$/i, ""));
  if (name === null) return;
  try { const r = await api("/api/templates", { json: { name, json: text } }); toast("已导入"); await loadTemplates(r.id); }
  catch (err) { alert("导入失败：" + err.message); }
};
$("#btnResetForm").onclick = () => {
  if (!curTpl || !confirm("把参数恢复成推荐值（提示词恢复成模板自带的）？")) return;
  try { localStorage.removeItem(draftKey(curTpl.id)); } catch { }
  $("#jobTitle").value = ""; renderForm({});
};
$("#btnRename").onclick = async () => {
  const n = prompt("新名字：", curTpl.name); if (!n) return;
  await api("/api/templates/" + curTpl.id, { json: { name: n } }); loadTemplates(curTpl.id);
};
$("#btnDelTpl").onclick = async () => {
  if (!confirm(`删除模板「${curTpl.name}」？`)) return;
  await api("/api/templates/" + curTpl.id, { method: "DELETE" }); curTpl = null; loadTemplates();
};

function fieldId(k) { return "f_" + k.replace(/[^\w]/g, "_"); }
// The form remembers what you typed per template (localStorage), so re-selecting a template,
// switching modes, restarting or updating the app never throws your prompt away.
function draftKey(id) { return "ckwb.draft." + id; }
function loadDraft(id) { try { return JSON.parse(localStorage.getItem(draftKey(id)) || "null"); } catch { return null; } }
function saveDraft() {
  if (!curTpl || $("#genForm").hidden) return;
  const v = collectValues(true).values;
  v["@h3mode"] = $("#h3Mode").value; v["@h3dur"] = $("#h3Dur").value; v["@title"] = $("#jobTitle").value; v["@v"] = 2;
  try { localStorage.setItem(draftKey(curTpl.id), JSON.stringify(v)); } catch { }
}
let draftTimer = null;
function saveDraftSoon() { clearTimeout(draftTimer); draftTimer = setTimeout(saveDraft, 400); }
function renderForm(values) {
  if (values === undefined && curTpl) {
    const d = loadDraft(curTpl.id);
    if (d && !d["@v"]) { // drafts from before recommended values existed: adopt them once (keep duration/prompt)
      for (const f of curTpl.fields) if (f.rec != null && !/时长/.test(f.label_cn || "")) d[f.key] = f.rec;
      d["@v"] = 2;
      try { localStorage.setItem(draftKey(curTpl.id), JSON.stringify(d)); } catch { }
    }
    if (d) {
      values = d;
      if (d["@h3mode"]) { $("#h3Mode").value = d["@h3mode"]; renderModeSeg(); }
      if (d["@h3dur"]) $("#h3Dur").value = d["@h3dur"];
      if (d["@title"] != null) $("#jobTitle").value = d["@title"];
    }
  }
  $("#formEmpty").hidden = !!curTpl; $("#genForm").hidden = !curTpl;
  $("#btnRename").hidden = $("#btnDelTpl").hidden = $("#btnResetForm").hidden = !curTpl;
  if (!curTpl) { $("#formTitle").textContent = "参数"; return; }
  $("#formTitle").textContent = curTpl.name;
  const prim = curTpl.fields.filter(f => f.primary);
  const adv = curTpl.fields.filter(f => !f.primary);
  const v = values || {};
  // order: text (positive first), uploads, numbers/seed
  const rank = f => f.kind === "text" ? (f.negative ? 1 : 0) : f.kind === "image" || f.kind === "file" ? 2 : 3;
  prim.sort((a, b) => rank(a) - rank(b));
  const big = prim.filter(f => rank(f) < 3), small = prim.filter(f => rank(f) === 3);
  $("#primaryFields").innerHTML = big.map(f => fieldHTML(f, v, true)).join("") + (small.length ? `<div class="f-grid">${small.map(f => fieldHTML(f, v, true)).join("")}</div>` : "");
  // advanced grouped by node
  let html = "", last = "";
  for (const f of adv) {
    if (f.node !== last) { if (last) html += "</div>"; html += `<div class="node">#${esc(f.node)} ${esc(f.node_name)}</div><div class="f-grid">`; last = f.node; }
    html += fieldHTML(f, v, false);
  }
  if (last) html += "</div>";
  $("#advFields").innerHTML = html || `<p class="muted small">没有其它参数</p>`;
  $("#advCount").textContent = adv.length ? `（${adv.length}）` : "";
  bindUploads();
  $$("[data-hide]").forEach(b => b.onclick = () => toggleHidden(b.dataset.hide, true));
  $$("[data-unhide]").forEach(b => b.onclick = () => toggleHidden(b.dataset.unhide, false));
  // focus the main prompt
  lastTextarea = $("#primaryFields textarea");
  draftHint();
}
async function toggleHidden(key, hide) {
  const tpl = curTpl, next = new Set(tpl.hidden || []);
  hide ? next.add(key) : next.delete(key);
  const vals = collectValues(true).values;
  await api("/api/templates/" + tpl.id, { json: { hidden: [...next] } });
  await loadTemplates(tpl.id);
  renderForm(vals);
}

function fieldHTML(f, v, primary) {
  const id = fieldId(f.key);
  const base = f.rec !== undefined && f.rec !== null ? f.rec : f.default;
  const val = v[f.key] !== undefined ? v[f.key] : base;
  const name = f.label_cn || (primary ? f.node_name : f.input);
  const sub = f.label_cn ? (primary && !/^Primitive/.test(f.class || "") ? f.node_name : "") : (primary ? f.input : "");
  const tip = esc([f.hint, `节点 #${f.node} ${f.node_name} · ${f.input}`, f.rec != null ? `推荐值：${recText(f)}` : ""].filter(Boolean).join("\n"));
  const recBtn = f.rec != null ? `<button type="button" class="x rec" data-rec="${esc(f.key)}" title="恢复推荐值" ${String(val) === String(f.rec) ? "hidden" : ""}>推荐 ${esc(recText(f))}</button>` : "";
  const lbl = `<span class="f-lbl" title="${tip}">${esc(name)}${sub ? ` <span class="muted">· ${esc(sub)}</span>` : ""}${f.hint ? ` <i class="q">?</i>` : ""}</span>`;
  const hideBtn = primary ? `<button type="button" class="x" data-hide="${esc(f.key)}" title="移到高级参数">收起</button>` : (curTpl.hidden || []).includes(f.key) ? `<button type="button" class="x" data-unhide="${esc(f.key)}">置顶</button>` : "";
  switch (f.kind) {
    case "text":
      return `<div class="f-wrap f-text ${f.negative ? "neg" : ""}"><div class="f-head">${lbl}<span><span class="muted" data-count="${id}"></span> ${hideBtn}</span></div><textarea id="${id}" data-key="${esc(f.key)}" data-kind="text" spellcheck="false">${esc(val)}</textarea></div>`;
    case "image": case "file": {
      const up = v["@up:" + f.key];
      return `<div class="f-wrap"><div class="f-head">${lbl}${hideBtn}</div><div class="drop" data-drop="${id}"><span class="ph">${f.kind === "image" ? "🖼" : "🎞"}</span><div><div>${up ? "沿用：" + esc(up.split(/[\\/]/).pop().split("__").pop()) : "点击或拖入文件"}</div><div class="muted small">不选则用模板里的：${esc(val)}</div></div>
        <input type="file" id="${id}" data-key="${esc(f.key)}" data-kind="upload" accept="${f.accept || ""}" hidden ${up ? `data-reuse="${esc(up)}"` : ""}></div></div>`;
    }
    case "seed": {
      const rnd = v["@rand:" + f.key] !== undefined ? v["@rand:" + f.key] : true;
      return `<label class="fld">${lbl}<div class="seedrow"><input type="number" id="${id}" data-key="${esc(f.key)}" data-kind="number" value="${esc(val)}"><label class="chk" style="margin:0"><input type="checkbox" data-rand="${esc(f.key)}" ${rnd ? "checked" : ""}>每次随机</label></div></label>`;
    }
    case "number":
      return `<label class="fld"><span class="row between">${lbl}${recBtn}</span><input type="number" step="any" id="${id}" data-key="${esc(f.key)}" data-kind="number" value="${esc(val)}">${f.hint && primary ? `<span class="f-hint">${esc(f.hint)}</span>` : ""}</label>`;
    case "bool":
      return `<div class="fld"><span class="row between">${lbl}${recBtn}</span><label class="chk switch-row"><input type="checkbox" id="${id}" data-key="${esc(f.key)}" data-kind="bool" ${val ? "checked" : ""}><span>${val ? "已开启" : "已关闭"}</span></label>${f.hint && primary ? `<span class="f-hint">${esc(f.hint)}</span>` : ""}</div>`;
    default:
      return `<label class="fld"><span class="row between">${lbl}${recBtn}</span><input id="${id}" data-key="${esc(f.key)}" data-kind="string" value="${esc(val)}"></label>`;
  }
}

function recText(f) { return f.rec === true ? "开" : f.rec === false ? "关" : String(f.rec); }
function bindRec() {
  $$("[data-rec]").forEach(b => b.onclick = e => {
    e.preventDefault();
    const f = curTpl.fields.find(x => x.key === b.dataset.rec); const el = document.getElementById(fieldId(f.key));
    if (el.type === "checkbox") { el.checked = !!f.rec; el.nextElementSibling.textContent = el.checked ? "已开启" : "已关闭"; } else el.value = f.rec;
    b.hidden = true; draftHint(); saveDraftSoon();
  });
  $$('#genForm input[data-kind="bool"]').forEach(el => el.addEventListener("change", () => { el.nextElementSibling.textContent = el.checked ? "已开启" : "已关闭"; }));
  $$("#genForm [data-key]").forEach(el => el.addEventListener("input", () => {
    const b = document.querySelector(`[data-rec="${el.dataset.key}"]`); if (!b) return;
    const f = curTpl.fields.find(x => x.key === el.dataset.key); const cur = el.type === "checkbox" ? el.checked : el.value;
    b.hidden = String(cur) === String(f.rec);
  }));
}
function bindUploads() {
  bindRec();
  $$(".drop").forEach(d => {
    const inp = $("input[type=file]", d);
    d.onclick = e => { if (e.target !== inp) inp.click(); };
    d.ondragover = e => { e.preventDefault(); d.classList.add("over"); };
    d.ondragleave = () => d.classList.remove("over");
    d.ondrop = e => { e.preventDefault(); d.classList.remove("over"); if (e.dataTransfer.files[0]) { const dt = new DataTransfer(); dt.items.add(e.dataTransfer.files[0]); inp.files = dt.files; inp.onchange(); } };
    inp.onchange = () => {
      const f = inp.files[0]; if (!f) return;
      inp.removeAttribute("data-reuse");
      const url = URL.createObjectURL(f);
      const media = f.type.startsWith("image/") ? `<img src="${url}">` : f.type.startsWith("video/") ? `<video src="${url}" muted></video>` : `<span class="ph">♪</span>`;
      d.firstElementChild.outerHTML = media;
      d.children[1].firstElementChild.textContent = f.name + " · " + fmtSize(f.size);
    };
  });
  $$("textarea[data-kind=text]").forEach(t => { t.oninput = () => updCount(t); t.onfocus = () => lastTextarea = t; updCount(t); });
}
function updCount(t) { saveDraftSoon(); updCount0(t); }
function updCount0(t) { const c = $(`[data-count="${t.id}"]`); if (c) c.textContent = t.value.trim() ? t.value.trim().split(/\s+/).length + " 词" : ""; }

function collectValues(forRender) {
  const values = {}, files = {}, rand = [];
  $$("#genForm [data-key]").forEach(el => {
    const k = el.dataset.key, kind = el.dataset.kind;
    if (kind === "upload") {
      if (el.files && el.files[0]) files[k] = el.files[0];
      else if (el.dataset.reuse) { values[k] = "@upload:" + el.dataset.reuse; if (forRender) values["@up:" + k] = el.dataset.reuse; }
      return;
    }
    if (kind === "number") { const n = parseFloat(el.value); if (!isNaN(n)) values[k] = n; }
    else if (kind === "bool") values[k] = el.checked;
    else values[k] = el.value;
  });
  $$("#genForm [data-rand]").forEach(c => { if (c.checked) rand.push(c.dataset.rand); if (forRender) values["@rand:" + c.dataset.rand] = c.checked; });
  return { values, files, rand };
}

$("#genForm").onsubmit = e => { e.preventDefault(); submitJob(false); };
$("#btnDraft").onclick = () => submitJob(true);
async function submitJob(draft) {
  $("#genErr").textContent = "";
  const { values, files, rand } = collectValues(false);
  const title = $("#jobTitle").value.trim() || curTpl.name;
  const fd = new FormData();
  fd.append("spec", JSON.stringify({ template_id: curTpl.id, values, random_seeds: rand, title: draft ? title + " · 草稿" : title, count: parseInt($("#jobCount").value) || 1, draft }));
  for (const [k, f] of Object.entries(files)) fd.append(k, f, f.name);
  try {
    const r = await api("/api/jobs", { method: "POST", body: fd });
    saveDraft();
    const what = draft ? "试生成" : "任务";
    toast(`已加入${what}（${r.ids.length} 个）` + (gst && (gst.state === "sleeping" || gst.state === "failed") ? " · GPU 在休眠，记得唤醒" : ""));
    loadJobs();
  } catch (err) { $("#genErr").textContent = "提交失败：" + err.message; }
}
let appCfg = {};
function draftHint() {
  const el = $("#draftHint"); if (!el || !curTpl) return;
  const sc = +appCfg.draft_scale || 0.5;
  const get = name => { const f = curTpl.fields.find(x => x.kind === "number" && x.input.toLowerCase() === name); if (!f) return null; const inp = document.getElementById(fieldId(f.key)); const v = inp ? parseFloat(inp.value) : f.default; return isNaN(v) ? f.default : v; };
  const w = get("width"), h = get("height"), mp = get("megapixels");
  const r = v => Math.max(128, Math.round(v * sc / 32) * 32);
  $("#btnDraft").disabled = !((w && h) || mp);
  el.textContent = w && h ? `${r(w)}×${r(h)}` : mp ? `${Math.max(0.05, Math.round(mp * sc * sc * 100) / 100)}MP` : "（模板无宽高）";
}
document.addEventListener("input", e => { if (e.target.closest && e.target.closest("#genForm")) { draftHint(); saveDraftSoon(); } });
document.addEventListener("change", e => { if (e.target.closest && e.target.closest("#genForm")) saveDraftSoon(); });

// ---------------- H3 prompt scaffolds ----------------
let lastTextarea = null;
function d2(n) { return Number(n).toFixed(2); }
function skeleton(mode, dur) {
  const D = d2(dur);
  const body = `integrated_multimodal_description: [Shot 1] Realistic film look. Medium-wide view of ___; slow, gentle move closer while ___.

overall_soundscape: ___

non_diegetic_music: N/A`;
  switch (mode) {
    case "T2VA": return body;
    case "I2VA": return `<Picture 1> is the opening frame ([Shot 1], 0.00 s).

integrated_multimodal_description: [Shot 1] Realistic film look. Starting from <Picture 1>: ___ (keep the same people, outfits and room). Camera: ___ while ___.

overall_soundscape: ___

non_diegetic_music: N/A`;
    case "FL2VA": return `Picture 1 is the opening frame (0.00 s) and Picture 2 the closing frame (${D} s) of [Shot 1].

integrated_multimodal_description: [Shot 1] Realistic film look. Opens as in Picture 1: ___. Camera: ___ while ___, ending exactly as in Picture 2.

overall_soundscape: ___

non_diegetic_music: N/A`;
    case "L2VA": return `<Picture 1> is the closing frame of [Shot 1] (${D} s).

integrated_multimodal_description: [Shot 1] Realistic film look. ___ view of ___ at the start. Camera: ___ while ___. By the end everything matches <Picture 1>.

overall_soundscape: ___

non_diegetic_music: N/A`;
    case "Ref2VA": return `subject_definitions:
<Subject 1> = the ___ from <Picture 1> (___).

summary:
<Subject 1> ___.

retention_analysis:
<Subject 1>, [Shot 1]: keep ___.

detailed_description:
Style ___, light ___.
[Shot 1] ___

overall_soundscape:
___

non_diegetic_music:
N/A`;
  }
}
const SNIPS = [
  ["新镜头", () => `[Shot 2] Cut at ${Math.min(9, Math.floor(+$("#h3Dur").value / 2))} s to `],
  ["台词", () => `(S1), speaking in a ___ voice: <d>[中文] </d>`],
  ["画外音", () => `(S1) off screen, mouth closed on camera: <d>[中文] </d>`],
  ["推近", () => "Slow, gentle move closer toward "],
  ["拉远", () => "Slow, gentle move back, "],
  ["跟拍", () => "Camera travels alongside as "],
  ["环绕", () => "Camera circles around "],
  ["横移", () => "Camera glides sideways to the right as "],
  ["摇镜", () => "Quick turn of the camera to the right shows "],
  ["固定", () => "Camera stays still while "],
  ["升降", () => "Camera rises slowly, "],
  ["手持晃动", () => "Slight handheld wobble as "],
  ["屏幕文字", () => `visible text "___" `],
  ["<Picture 1>", () => "<Picture 1>"],
  ["<Subject 1>", () => "<Subject 1>"],
];
$("#snips").innerHTML = SNIPS.map((s, i) => `<button type="button" data-i="${i}">${esc(s[0])}</button>`).join("");
$$("#snips button").forEach(b => b.onclick = () => insertText(SNIPS[b.dataset.i][1]()));
function mainTextarea() { return (lastTextarea && document.body.contains(lastTextarea)) ? lastTextarea : $("#primaryFields textarea"); }
function insertText(s) {
  const t = mainTextarea(); if (!t) return toast("这个模板没有文本参数");
  const a = t.selectionStart ?? t.value.length, b = t.selectionEnd ?? a;
  t.value = t.value.slice(0, a) + s + t.value.slice(b);
  t.focus(); t.selectionStart = t.selectionEnd = a + s.length; updCount(t);
}
$("#btnSkeleton").onclick = () => {
  const t = mainTextarea(); if (!t) return toast("这个模板没有文本参数");
  const dur = Math.min(15, Math.max(4, +$("#h3Dur").value || 6));
  const sk = skeleton($("#h3Mode").value, dur);
  if (t.value.trim() && !confirm("替换当前提示词？（取消 = 插入到光标处）")) insertText(sk);
  else { t.value = sk; updCount(t); t.focus(); const i = t.value.indexOf("___"); if (i >= 0) { t.selectionStart = i; t.selectionEnd = i + 3; } }
};

// prompt library
let promptLib = [];
$("#btnSavePrompt").onclick = async () => {
  const t = mainTextarea(); if (!t || !t.value.trim()) return toast("提示词是空的");
  const name = prompt("保存为：", t.value.trim().replace(/\s+/g, " ").slice(0, 30)); if (!name) return;
  promptLib = await api("/api/prompts");
  promptLib.unshift({ name, text: t.value, mode: $("#h3Mode").value, at: Date.now() });
  await api("/api/prompts", { json: promptLib }); toast("已存入提示词库");
};
$("#btnLoadPrompt").onclick = async () => {
  promptLib = await api("/api/prompts");
  openModal(`<h3>提示词库</h3><div class="plist">${promptLib.length ? promptLib.map((p, i) => `<div class="p"><b>${esc(p.name)}</b><span class="tx">${esc(p.text.slice(0, 120))}</span><button class="btn sm" data-use="${i}">使用</button><button class="btn sm ghost danger" data-del="${i}">删除</button></div>`).join("") : `<p class="muted">还没有保存的提示词</p>`}</div><div class="row" style="justify-content:flex-end;margin-top:12px"><button class="btn" data-close>关闭</button></div>`);
  $$("[data-use]").forEach(b => b.onclick = () => { const t = mainTextarea(); if (t) { t.value = promptLib[b.dataset.use].text; updCount(t); } if (promptLib[b.dataset.use].mode) { $("#h3Mode").value = promptLib[b.dataset.use].mode; renderModeSeg(); } closeModal(); });
  $$("[data-del]").forEach(b => b.onclick = async () => { promptLib.splice(+b.dataset.del, 1); await api("/api/prompts", { json: promptLib }); $("#btnLoadPrompt").onclick(); });
};

// ---------------- queue ----------------
const JST = { queued: "排队", waiting: "等待 GPU", uploading: "上传中", submitted: "已提交", running: "生成中", downloading: "下载中", done: "完成", failed: "失败", canceled: "已取消" };
const ACTIVE = new Set(["queued", "waiting", "uploading", "submitted", "running", "downloading"]);
let jobs = [];
async function loadJobs() {
  try { jobs = await api("/api/jobs"); } catch { return; }
  if (page === "ai" && typeof aiGallery === "function") aiGallery();
  const n = jobs.filter(j => ACTIVE.has(j.status)).length;
  $("#qBadge").textContent = n || "";
  if (page !== "queue") return;
  const box = $("#jobList");
  if (!jobs.length) { box.innerHTML = `<div class="card empty">队列是空的。去「生成」页提交任务。</div>`; box._keyed = false; return; }
  if (!box._keyed) { box.innerHTML = ""; box._keyed = true; }
  // keyed update: only cards whose content changed are touched, and finished outputs
  // (video/img elements) are moved over untouched, so nothing reloads or flickers.
  const want = new Set(jobs.map(j => j.id));
  [...box.children].forEach(el => { if (!want.has(el.dataset.id)) el.remove(); });
  let prev = null;
  for (const j of jobs) {
    let el = [...box.children].find(c => c.dataset.id === j.id);
    if (!el) { el = document.createElement("div"); el.dataset.id = j.id; }
    const html = jobHTML(j);
    if (el._sig !== html) {
      const outsSig = JSON.stringify((j.outputs || []).map(o => o.path));
      const keep = el._outsSig === outsSig ? el.querySelector(".outs") : null;
      el.innerHTML = html;
      if (keep) { const fresh = el.querySelector(".outs"); if (fresh) fresh.replaceWith(keep); }
      else $$(".outs [data-path]", el).forEach(m => m.onclick = () => preview(m.dataset.path, m.dataset.kind));
      $$("[data-act]", el).forEach(btn => btn.onclick = () => jobAct(btn.dataset.act, btn.dataset.id));
      el._sig = html; el._outsSig = outsSig;
    }
    const want = prev ? prev.nextSibling : box.firstChild;
    if (want !== el) box.insertBefore(el, want);
    prev = el;
  }
}
function jobHTML(j) {
  const dur = j.finished && j.started ? " · 用时 " + fmtDur(j.finished - j.started) : j.started ? " · 已运行 " + fmtDur(Date.now() / 1000 - j.started) : "";
  const showBar = ["running", "downloading", "submitted", "uploading"].includes(j.status);
  const pct = j.status === "running" ? Math.round((j.progress || 0) * 100) : j.status === "downloading" ? 100 : 0;
  const seeds = j.used_seeds && Object.keys(j.used_seeds).length ? Object.values(j.used_seeds).join(", ") : Object.entries(j.values || {}).filter(([k]) => /\.(noise_)?seed$/.test(k)).map(([, v]) => v).join(", ");
  const acts = [];
  if (ACTIVE.has(j.status)) acts.push(`<button class="btn sm" data-act="cancel" data-id="${j.id}">取消</button>`);
  const child = jobs.find(x => x.parent_id === j.id);
  if (j.draft && j.status === "done") acts.push(child ? `<button class="btn sm ghost" disabled>已生成高清版</button>` : `<button class="btn sm primary" data-act="hires" data-id="${j.id}">满意，生成高清版${j.full_res ? " " + esc(j.full_res) : ""}</button>`);
  if (["failed", "canceled", "done"].includes(j.status)) acts.push(`<button class="btn sm" data-act="retry" data-id="${j.id}">${j.status === "done" ? "重跑" : "重试"}</button>`);
  acts.push(`<button class="btn sm ghost" data-act="again" data-id="${j.id}">载入到表单</button>`);
  if (!ACTIVE.has(j.status) || j.status === "queued" || j.status === "waiting") acts.push(`<button class="btn sm ghost danger" data-act="del" data-id="${j.id}">删除</button>`);
  const outs = (j.outputs || []).map(o => o.kind === "video" ? `<video src="${fileURL(o.path)}" preload="metadata" muted data-path="${esc(o.path)}" data-kind="video"></video>` : o.kind === "image" ? `<img src="${fileURL(o.path)}" data-path="${esc(o.path)}" data-kind="image">` : `<button class="btn sm" data-path="${esc(o.path)}" data-kind="${o.kind}">${esc(o.filename)}</button>`).join("");
  const tag = j.draft ? `<span class="tag draft">草稿 ${esc(j.draft_res || "")}</span>` : j.parent_id ? `<span class="tag hires">高清 ${esc(j.full_res || "")}</span>` : "";
  return `<div class="job${j.draft ? " is-draft" : ""}"><div><div class="t"><span class="st ${j.status}">${JST[j.status] || j.status}</span>${tag}${esc(j.title)}</div>
    <div class="meta">${esc(j.template_name)} · ${fmtTime(j.created)}${dur}${seeds ? " · seed " + esc(seeds) : ""}</div>
    <div class="msg">${esc(j.message)}${j.status === "running" && j.node ? ` · 节点 #${esc(j.node)}` : ""}${j.status === "running" ? ` · ${pct}%` : ""}</div></div>
    <div class="acts">${acts.join("")}</div>
    ${showBar ? `<div class="bar"><div style="width:${pct}%"></div></div>` : ""}
    ${outs ? `<div class="outs">${outs}</div>` : ""}</div>`;
}
async function jobAct(act, id) {
  try {
    if (act === "cancel") await api(`/api/jobs/${id}/cancel`, { json: {} });
    if (act === "retry") await api(`/api/jobs/${id}/retry`, { json: {} });
    if (act === "hires") { await api(`/api/jobs/${id}/hires`, { json: {} }); toast("已加入高清版（同一种子、原分辨率）"); }
    if (act === "del") await api(`/api/jobs/${id}`, { method: "DELETE" });
    if (act === "again") {
      const j = jobs.find(x => x.id === id);
      if (!templates.find(t => t.id === j.template_id)) return toast("这个任务的模板已被删除");
      const vals = { ...j.values };
      for (const [k, p] of Object.entries(j.uploads || {})) vals["@up:" + k] = p;
      for (const k of j.random_seeds || []) vals["@rand:" + k] = true;
      show("gen"); selectTpl(j.template_id, vals); $("#jobTitle").value = j.title === j.template_name ? "" : j.title;
      return;
    }
    loadJobs();
  } catch (e) { toast(e.message); }
}
$("#btnClearDone").onclick = async () => { await api("/api/jobs/clear", { json: {} }); loadJobs(); };

// ---------------- results ----------------
async function loadFiles() {
  let r; try { r = await api("/api/files"); } catch (e) { $("#fileGrid").innerHTML = `<p class="err">${esc(e.message)}</p>`; return; }
  $("#outDirText").textContent = "保存位置：" + r.dir;
  const k = $("#kindFilter").value;
  const fs = r.files.filter(f => !k || f.kind === k);
  $("#fileGrid").innerHTML = fs.length ? fs.map(f => `<div class="file"><div class="media" data-path="${esc(f.path)}" data-kind="${f.kind}">${f.kind === "video" ? `<video src="${fileURL(f.path)}#t=0.1" preload="metadata" muted></video>` : f.kind === "image" ? `<img src="${fileURL(f.path)}" loading="lazy">` : `<span class="aud">♪</span>`}</div>
    <div class="info"><div class="n" title="${esc(f.name)}">${esc(f.name)}</div><div class="row"><span>${esc(f.dir)} · ${fmtSize(f.size)}</span><span><button data-reveal="${esc(f.path)}">定位</button><button data-del="${esc(f.path)}">删除</button></span></div></div></div>`).join("") : `<div class="card empty" style="grid-column:1/-1">还没有结果文件</div>`;
  $$("#fileGrid .media").forEach(m => m.onclick = () => preview(m.dataset.path, m.dataset.kind));
  $$("#fileGrid [data-reveal]").forEach(b => b.onclick = () => api("/api/open", { json: { path: b.dataset.reveal } }));
  $$("#fileGrid [data-del]").forEach(b => b.onclick = async () => { if (!confirm("删除这个文件？")) return; await api("/api/delete-file", { json: { path: b.dataset.del } }); loadFiles(); });
  $$("#fileGrid video").forEach(v => { v.onmouseenter = () => v.play().catch(() => { }); v.onmouseleave = () => v.pause(); });
}
$("#kindFilter").onchange = loadFiles;
$$("#kindSeg button").forEach(b => b.onclick = () => { $$("#kindSeg button").forEach(x => x.classList.toggle("on", x === b)); $("#kindFilter").value = b.dataset.k; loadFiles(); });

// H3 mode segmented control (mirrors the hidden <select>)
const H3_MODES = [["T2VA", "文生视频"], ["I2VA", "首帧"], ["FL2VA", "首尾帧"], ["L2VA", "尾帧"], ["Ref2VA", "全参考"]];
function renderModeSeg() {
  $("#h3ModeSeg").innerHTML = H3_MODES.map(([v, n]) => `<button type="button" data-v="${v}" class="${$("#h3Mode").value === v ? "on" : ""}" title="${n}">${v}</button>`).join("");
  $$("#h3ModeSeg button").forEach(b => b.onclick = () => { $("#h3Mode").value = b.dataset.v; renderModeSeg(); saveDraftSoon(); });
}
renderModeSeg();

// theme toggle
function applyTheme(t) {
  if (t) document.documentElement.dataset.theme = t; else delete document.documentElement.dataset.theme;
  const eff = t || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  $$("[data-theme-set]").forEach(b => b.classList.toggle("on", b.dataset.themeSet === eff));
}
$$("[data-theme-set]").forEach(b => b.onclick = () => { try { localStorage.setItem("ckwb.theme", b.dataset.themeSet); } catch { } applyTheme(b.dataset.themeSet); });
let savedTheme = null; try { savedTheme = localStorage.getItem("ckwb.theme"); } catch { }
applyTheme(savedTheme);
$("#btnRefreshFiles").onclick = loadFiles;
$("#btnOpenOut").onclick = () => api("/api/open", { json: { path: "" } });
function preview(path, kind) {
  const u = fileURL(path), name = path.split(/[\\/]/).pop();
  const media = kind === "video" ? `<video src="${u}" controls autoplay loop></video>` : kind === "image" ? `<img src="${u}">` : kind === "audio" ? `<audio src="${u}" controls autoplay></audio>` : "";
  openModal(`${media}<div class="row between gap"><span class="muted small">${esc(name)}</span><span class="row gap"><button class="btn sm" id="pvReveal">在文件夹中显示</button><button class="btn sm" data-close>关闭</button></span></div>`);
  $("#pvReveal").onclick = () => api("/api/open", { json: { path } });
}
function openModal(html) { $("#modalBox").innerHTML = html; $("#modal").hidden = false; $$("[data-close]").forEach(b => b.onclick = closeModal); }
function closeModal() { $("#modal").hidden = true; $("#modalBox").innerHTML = ""; }
$("#modal").onclick = e => { if (e.target.id === "modal") closeModal(); };
document.addEventListener("keydown", e => { if (e.key === "Escape") closeModal(); });

// ---------------- library (on-demand nodes / models) ----------------
const MODEL_DIRS = ["checkpoints", "diffusion_models", "loras", "vae", "text_encoders", "clip_vision", "upscale_models", "controlnet", "SEEDVR2"];
const PRESETS = [
  { group: "SeedVR2 视频放大", items: [
    { kind: "node", url: "https://github.com/numz/ComfyUI-SeedVR2_VideoUpscaler", title: "SeedVR2 Video Upscaler 插件", on: true },
    { kind: "model", url: "hf://numz/SeedVR2_comfyUI/ema_vae_fp16.safetensors", dest: "SEEDVR2/ema_vae_fp16.safetensors", title: "SeedVR2 VAE（必需）", on: true },
    { kind: "model", url: "hf://numz/SeedVR2_comfyUI/seedvr2_ema_3b_fp16.safetensors", dest: "SEEDVR2/seedvr2_ema_3b_fp16.safetensors", title: "DiT 3B fp16（快）", on: true },
    { kind: "model", url: "hf://numz/SeedVR2_comfyUI/seedvr2_ema_7b_fp16.safetensors", dest: "SEEDVR2/seedvr2_ema_7b_fp16.safetensors", title: "DiT 7B fp16（画质最好）", on: false },
    { kind: "model", url: "hf://numz/SeedVR2_comfyUI/seedvr2_ema_7b_sharp_fp16.safetensors", dest: "SEEDVR2/seedvr2_ema_7b_sharp_fp16.safetensors", title: "DiT 7B sharp fp16（更锐利）", on: false },
  ] },
];
let lib = null, libErr = "", libSel = new Set(), libTimer = null;
async function loadLib() {
  try { lib = await api("/api/gate/library"); libErr = ""; }
  catch (e) { lib = null; libErr = e.message; }
  renderLib();
  clearTimeout(libTimer);
  if (page === "lib") libTimer = setTimeout(loadLib, lib && lib.fetching && lib.fetching.length ? 1500 : 4000);
}
function renderLib() {
  const note = $("#libNote"), list = $("#libList");
  note.className = "lib-note";
  if (!lib) {
    note.classList.add("bad");
    note.textContent = "读取库失败：" + libErr + "。如果是第一次用这个功能，需要先在 VPS 上更新网关（见说明）。";
    list.innerHTML = ""; $("#libBar").hidden = true; return;
  }
  const ready = gst && gst.state === "ready";
  const busy = lib.fetching && lib.fetching.length;
  const dl = gst && gst.progress && gst.progress.download;
  if (busy) note.innerHTML = `<span class="dot s-busy"></span>正在安装 ${lib.fetching.length} 项…（插件装完会自动重启 ComfyUI，约 30 秒）` + (dl && dl.total_gb ? `<div class="bar"><div style="width:${Math.min(100, dl.done_gb / dl.total_gb * 100)}%"></div></div><span class="mono">${dl.done_gb.toFixed(1)}/${dl.total_gb.toFixed(1)} GB</span>` : "");
  else if (lib.fetch_error) { note.classList.add("bad"); note.textContent = "上次安装失败：" + lib.fetch_error; }
  else note.textContent = ready ? "" : "GPU 未就绪：可以先整理清单，唤醒后再点安装。勾了「唤醒时装」的项目会在每次开机时自动装好。";
  const items = (lib.items || []).slice().sort((a, b) => (a.kind === b.kind ? a.title.localeCompare(b.title) : a.kind === "node" ? -1 : 1));
  if (!items.length) { list.innerHTML = ""; note.innerHTML = `<div class="lib-empty grow">库是空的。点右上角「＋ 添加」放入插件或模型，或从「预设」里一键加入 SeedVR2。</div>`; $("#libBar").hidden = true; return; }
  const inst = new Set(lib.installed || []), fetching = new Set((lib.fetching || []).map(x => x.replace(/^lib:/, "")));
  list.innerHTML = items.map(it => {
    const st = fetching.has(it.id) ? `<span class="lstat busy">安装中</span>` : inst.has(it.id) ? `<span class="lstat on">已在 GPU</span>` : `<span class="lstat">未安装</span>`;
    const sub = it.kind === "model" ? `models/${esc(it.dest)}${it.size_gb ? " · " + it.size_gb.toFixed(2) + " GB" : ""}` : esc(it.url.replace(/^https:\/\//, ""));
    return `<div class="lib-row" data-id="${esc(it.id)}">
      <input type="checkbox" data-sel="${esc(it.id)}" ${libSel.has(it.id) ? "checked" : ""}>
      <span class="kind ${it.kind}">${it.kind === "node" ? "插件" : "模型"}</span>
      <div class="main"><div class="ti">${esc(it.title)}</div><div class="sub" title="${esc(it.url)}">${sub}</div></div>
      <div class="acts">${st}<label class="seg-chk" title="每次唤醒时自动安装（会增加唤醒时间）"><input type="checkbox" data-auto="${esc(it.id)}" ${it.auto_wake ? "checked" : ""}><span>唤醒时装</span></label></div>
      <div class="acts"><button class="btn xs" data-inst="${esc(it.id)}" ${!ready || busy || inst.has(it.id) ? "disabled" : ""}>安装</button><button class="btn ghost xs danger" data-del="${esc(it.id)}">删除</button></div>
    </div>`;
  }).join("");
  $$("[data-sel]", list).forEach(c => c.onchange = () => { c.checked ? libSel.add(c.dataset.sel) : libSel.delete(c.dataset.sel); renderLibBar(); });
  $$("[data-auto]", list).forEach(c => c.onchange = async () => {
    const it = lib.items.find(x => x.id === c.dataset.auto);
    try { await api("/api/gate/library", { json: { ...it, auto_wake: c.checked } }); toast(c.checked ? "下次唤醒会自动安装" : "已改为按需安装"); } catch (e) { toast("保存失败：" + e.message); }
    loadLib();
  });
  $$("[data-inst]", list).forEach(b => b.onclick = () => libInstall([b.dataset.inst]));
  $$("[data-del]", list).forEach(b => b.onclick = async () => {
    if (!confirm("从库里删除这一项？（不影响已装在当前 GPU 上的文件）")) return;
    try { await api("/api/gate/library/" + encodeURIComponent(b.dataset.del), { method: "DELETE" }); libSel.delete(b.dataset.del); } catch (e) { toast(e.message); }
    loadLib();
  });
  renderLibBar();
}
function renderLibBar() {
  const ids = [...libSel].filter(id => lib && lib.items.some(i => i.id === id));
  $("#libBar").hidden = !ids.length;
  $("#libSel").textContent = `已选 ${ids.length} 项`;
  $("#btnLibInstallSel").disabled = !(gst && gst.state === "ready") || (lib && lib.fetching && lib.fetching.length);
}
$("#btnLibClear").onclick = () => { libSel.clear(); renderLib(); };
$("#btnLibInstallSel").onclick = () => libInstall([...libSel]);
async function libInstall(ids) {
  const hasNode = ids.some(id => (lib.items.find(i => i.id === id) || {}).kind === "node");
  const active = jobs.some(j => ["running", "submitted", "uploading"].includes(j.status));
  if (hasNode && active && !confirm("安装插件后会重启 ComfyUI（约 30 秒），正在生成的任务会中断。继续？")) return;
  try { await api("/api/gate/library/install", { json: { ids } }); toast("开始安装"); libSel.clear(); }
  catch (e) { toast("安装失败：" + e.message); }
  loadLib();
}
function libForm(prefill = {}) {
  const k = prefill.kind || "node";
  openModal(`<h3>添加到库</h3><p class="hint" style="margin:0 0 12px">只保存到清单，不会马上安装。</p>
    <div class="seg" id="lfKind" style="margin-bottom:14px"><button type="button" data-k="node">插件（Git 仓库）</button><button type="button" data-k="model">模型文件</button></div>
    <div class="form-grid">
      <label class="fld full" id="lfUrlL">下载地址<input id="lfUrl" placeholder=""></label>
      <label class="fld lf-model">保存到文件夹<select id="lfDir">${MODEL_DIRS.map(d => `<option>${d}</option>`).join("")}<option value="__custom">自定义…</option></select></label>
      <label class="fld lf-model">文件名<input id="lfFile" placeholder="自动从链接里取"></label>
      <label class="fld lf-model full" id="lfDirCustomL" hidden>自定义文件夹（相对 ComfyUI/models）<input id="lfDirCustom" placeholder="例如 SEEDVR2"></label>
      <label class="fld full">名称（可选）<input id="lfTitle"></label>
    </div>
    <label class="chk"><input type="checkbox" id="lfAuto">每次唤醒时自动安装（会增加唤醒时间）</label>
    <p class="err" id="lfErr"></p>
    <div class="row end"><button class="btn" data-close>取消</button><button class="btn primary" id="lfSave">保存到库</button></div>`);
  let kind = k;
  const sync = () => {
    $$("#lfKind button").forEach(b => b.classList.toggle("on", b.dataset.k === kind));
    $$(".lf-model").forEach(e => e.hidden = kind !== "model");
    $("#lfDirCustomL").hidden = kind !== "model" || $("#lfDir").value !== "__custom";
    $("#lfUrl").placeholder = kind === "node" ? "https://github.com/作者/仓库" : "https://huggingface.co/…/resolve/main/xxx.safetensors 或 hf://作者/仓库/文件";
  };
  $$("#lfKind button").forEach(b => b.onclick = () => { kind = b.dataset.k; sync(); });
  $("#lfDir").onchange = sync;
  $("#lfUrl").oninput = () => { if (kind === "model" && !$("#lfFile").dataset.touched) $("#lfFile").value = decodeURIComponent($("#lfUrl").value.split("?")[0].split("/").pop() || ""); };
  $("#lfFile").oninput = () => $("#lfFile").dataset.touched = "1";
  sync();
  $("#lfSave").onclick = async () => {
    const url = $("#lfUrl").value.trim();
    const body = { kind, url, title: $("#lfTitle").value.trim(), auto_wake: $("#lfAuto").checked };
    if (kind === "model") {
      const dir = $("#lfDir").value === "__custom" ? $("#lfDirCustom").value.trim().replace(/^\/+|\/+$/g, "") : $("#lfDir").value;
      const file = $("#lfFile").value.trim() || url.split("?")[0].split("/").pop();
      body.dest = dir + "/" + file;
    }
    $("#lfSave").disabled = true; $("#lfSave").textContent = "保存中…";
    try { await api("/api/gate/library", { json: body }); closeModal(); toast("已加入库"); loadLib(); }
    catch (e) { $("#lfErr").textContent = e.message; $("#lfSave").disabled = false; $("#lfSave").textContent = "保存到库"; }
  };
}
$("#btnLibAdd").onclick = () => libForm();
$("#btnLibPreset").onclick = () => {
  openModal(`<h3>预设</h3><p class="hint" style="margin:0">勾选要加入库的项目（只加清单，不会马上安装）。</p>` + PRESETS.map((g, gi) => `<div class="sec-h" style="margin:14px 0 6px"><b>${esc(g.group)}</b></div><div class="preset">${g.items.map((it, i) => `<label><input type="checkbox" data-p="${gi}.${i}" ${it.on ? "checked" : ""}><span class="kind ${it.kind}" style="width:40px">${it.kind === "node" ? "插件" : "模型"}</span>${esc(it.title)}<span class="x">${esc(it.dest || it.url.replace("https://github.com/", ""))}</span></label>`).join("")}</div>`).join("") + `<p class="err" id="psErr"></p><div class="row end"><button class="btn" data-close>取消</button><button class="btn primary" id="psAdd">加入库</button></div>`);
  $("#psAdd").onclick = async () => {
    const picks = $$("[data-p]:checked").map(c => { const [g, i] = c.dataset.p.split(".").map(Number); return PRESETS[g].items[i]; });
    $("#psAdd").disabled = true; $("#psAdd").textContent = "添加中…";
    try { for (const it of picks) await api("/api/gate/library", { json: { kind: it.kind, url: it.url, dest: it.dest, title: it.title } }); closeModal(); toast(`已加入 ${picks.length} 项`); loadLib(); }
    catch (e) { $("#psErr").textContent = e.message; $("#psAdd").disabled = false; $("#psAdd").textContent = "加入库"; }
  };
};

// ---------------- LLM providers (settings) ----------------
const LLM_PRESETS = [
  { name: "DeepSeek", kind: "openai", base_url: "https://api.deepseek.com/v1" },
  { name: "通义千问（阿里云百炼）", kind: "openai", base_url: "https://dashscope.aliyuncs.com/compatible-mode/v1" },
  { name: "Kimi（月之暗面）", kind: "openai", base_url: "https://api.moonshot.cn/v1" },
  { name: "智谱 GLM", kind: "openai", base_url: "https://open.bigmodel.cn/api/paas/v4" },
  { name: "硅基流动", kind: "openai", base_url: "https://api.siliconflow.cn/v1" },
  { name: "OpenRouter", kind: "openai", base_url: "https://openrouter.ai/api/v1" },
  { name: "OpenAI", kind: "openai", base_url: "https://api.openai.com/v1" },
  { name: "Ollama（本机）", kind: "openai", base_url: "http://127.0.0.1:11434/v1" },
  { name: "LM Studio（本机）", kind: "openai", base_url: "http://127.0.0.1:1234/v1" },
  { name: "Claude（Anthropic）", kind: "anthropic", base_url: "https://api.anthropic.com" },
  { name: "Gemini（Google）", kind: "gemini", base_url: "https://generativelanguage.googleapis.com" },
];
const KIND_CN = { openai: "OpenAI 兼容", anthropic: "Anthropic", gemini: "Gemini" };
let llms = [], defaultLLM = "";
function renderLLMs() {
  $("#llmList").innerHTML = llms.map((p, i) => `<div class="llm-row">
    <input type="radio" name="llmdef" value="${esc(p.id)}" ${p.id === defaultLLM || (!defaultLLM && i === 0) ? "checked" : ""} title="设为默认">
    <div style="min-width:0"><div class="ti">${esc(p.name)} <span class="pill sm">${KIND_CN[p.kind] || p.kind}</span></div><div class="sub">${esc(p.model || "（未填模型）")} · ${esc(p.base_url || "")}</div></div>
    <div class="acts"><span class="muted small" data-tmsg="${i}"></span><button type="button" class="btn xs" data-ltest="${i}">测试</button><button type="button" class="btn ghost xs" data-ledit="${i}">编辑</button><button type="button" class="btn ghost xs danger" data-ldel="${i}">删除</button></div>
  </div>`).join("");
  $$("[name=llmdef]").forEach(r => r.onchange = () => { defaultLLM = r.value; saveAll("已设为默认"); });
  $$("[data-ledit]").forEach(b => b.onclick = () => llmForm(+b.dataset.ledit));
  $$("[data-ldel]").forEach(b => b.onclick = () => { if (!confirm("删除这个 API？")) return; llms.splice(+b.dataset.ldel, 1); saveAll("已删除"); });
  $$("[data-ltest]").forEach(b => b.onclick = async () => {
    const m = $(`[data-tmsg="${b.dataset.ltest}"]`); m.textContent = "测试中…";
    try { const r = await api("/api/llm/test", { json: llms[+b.dataset.ltest] }); m.textContent = `✓ ${r.ms} ms`; }
    catch (e) { m.textContent = "✗ " + e.message; m.title = e.message; }
  });
}
async function saveAll(msg) {
  await $("#setForm").onsubmit({ preventDefault() { }, target: $("#setForm") });
  if (msg) toast(msg);
}
function llmForm(idx) {
  const p = idx != null ? { ...llms[idx] } : { kind: "openai", name: "", base_url: "", api_key: "", model: "" };
  openModal(`<h3>${idx != null ? "编辑" : "添加"}大模型 API</h3>
    <div class="form-grid" style="margin-top:12px">
      ${idx == null ? `<label class="fld full">预设<select id="lpPreset"><option value="">— 选一个常用服务（可选）—</option>${LLM_PRESETS.map((x, i) => `<option value="${i}">${esc(x.name)}</option>`).join("")}</select></label>` : ""}
      <label class="fld full">接口类型<div class="seg" id="lpKind">${Object.entries(KIND_CN).map(([k, n]) => `<button type="button" data-k="${k}">${n}</button>`).join("")}</div></label>
      <label class="fld">名称<input id="lpName" value="${esc(p.name)}"></label>
      <label class="fld">Base URL<input id="lpBase" value="${esc(p.base_url)}" placeholder="留空用官方默认地址"></label>
      <label class="fld full">API Key<input id="lpKey" type="password" value="${esc(p.api_key)}" autocomplete="off" placeholder="本机模型（Ollama / LM Studio）可留空"></label>
      <label class="fld full">模型<div class="row gap"><input id="lpModel" class="grow" list="lpModels" value="${esc(p.model)}" placeholder="填模型名，或点右边获取列表"><datalist id="lpModels"></datalist><button type="button" class="btn xs" id="lpFetch">获取模型列表</button></div></label>
    </div>
    <p class="hint" style="margin-top:0">提示：想写不受限制的内容，可以接本机 Ollama / LM Studio 里的 abliterated 模型，或 OpenRouter 上的对应模型。要看参考图的话，选支持识图的模型。</p>
    <p class="err" id="lpErr"></p>
    <div class="row end"><button type="button" class="btn" data-close>取消</button><button type="button" class="btn" id="lpTest">测试</button><button type="button" class="btn primary" id="lpSave">保存</button></div>`);
  let kind = p.kind;
  const sync = () => $$("#lpKind button").forEach(b => b.classList.toggle("on", b.dataset.k === kind));
  $$("#lpKind button").forEach(b => b.onclick = () => { kind = b.dataset.k; sync(); });
  sync();
  if ($("#lpPreset")) $("#lpPreset").onchange = e => { const x = LLM_PRESETS[e.target.value]; if (!x) return; kind = x.kind; sync(); $("#lpName").value = x.name; $("#lpBase").value = x.base_url; };
  const cur = () => ({ id: p.id || "", name: $("#lpName").value.trim() || KIND_CN[kind], kind, base_url: $("#lpBase").value.trim(), api_key: $("#lpKey").value.trim(), model: $("#lpModel").value.trim() });
  $("#lpFetch").onclick = async () => {
    $("#lpErr").textContent = ""; $("#lpFetch").textContent = "获取中…";
    try { const r = await api("/api/llm/models", { json: cur() }); $("#lpModels").innerHTML = r.models.map(m => `<option value="${esc(m)}">`).join(""); $("#lpFetch").textContent = `${r.models.length} 个模型`; $("#lpModel").focus(); }
    catch (e) { $("#lpErr").textContent = "获取失败：" + e.message; $("#lpFetch").textContent = "获取模型列表"; }
  };
  $("#lpTest").onclick = async () => {
    $("#lpErr").textContent = "测试中…";
    try { const r = await api("/api/llm/test", { json: cur() }); $("#lpErr").textContent = ""; toast(`连接成功（${r.ms} ms）`); }
    catch (e) { $("#lpErr").textContent = "测试失败：" + e.message; }
  };
  $("#lpSave").onclick = async () => {
    const v = cur();
    if (!v.model) { $("#lpErr").textContent = "请填写模型名"; return; }
    if (idx != null) llms[idx] = v; else llms.push(v);
    closeModal(); await saveAll("已保存");
  };
}
$("#btnLLMAdd").onclick = () => llmForm(null);

// ---- imported H3 prompt guide (only on this PC; the official guide is not shipped with Wakeloom) ----
let guideInfo = { imported: false };
async function loadGuide() {
  try { guideInfo = await api("/api/prompt-guide"); } catch { }
  $("#guideState").textContent = guideInfo.imported ? `已导入 · ${fmtSize(guideInfo.size)} · ${fmtTime(guideInfo.modified)}` : "未导入（使用内置通用说明）";
  $("#btnGuideClear").hidden = !guideInfo.imported;
  $("#btnGuideImport").textContent = guideInfo.imported ? "重新导入" : "导入 H3 提示词指南";
}
$("#btnGuideImport").onclick = () => $("#guideFile").click();
$("#guideFile").onchange = async () => {
  const f = $("#guideFile").files[0]; $("#guideFile").value = "";
  if (!f) return;
  if (!/\.(md|txt)$/i.test(f.name)) return toast("只支持 .md 或 .txt 文件");
  if (f.size > 1 << 20) return toast("文件太大（最大 1 MB）");
  try { await api("/api/prompt-guide", { json: { name: f.name, text: await f.text() } }); toast("已导入，AI 写提示词会按它输出"); }
  catch (e) { toast("导入失败：" + e.message); }
  loadGuide();
};
$("#btnGuideClear").onclick = async () => {
  if (!confirm("清除导入的 H3 提示词指南？之后改用内置通用说明。")) return;
  await api("/api/prompt-guide", { method: "DELETE" }).catch(e => toast(e.message));
  loadGuide();
};
loadGuide();

// ---------------- AI prompt writer ----------------
let aiCtl = null, aiState = { tab: "new", idea: "", instr: "", withImg: true };
async function formImages() {
  const files = $$("#genForm input[type=file]").map(i => i.files && i.files[0]).filter(f => f && f.type.startsWith("image/"));
  const out = [];
  for (const f of files) {
    const url = URL.createObjectURL(f);
    const img = await new Promise((ok, no) => { const im = new Image(); im.onload = () => ok(im); im.onerror = no; im.src = url; });
    const sc = Math.min(1, 1280 / Math.max(img.width, img.height));
    const cv = document.createElement("canvas"); cv.width = Math.round(img.width * sc); cv.height = Math.round(img.height * sc);
    cv.getContext("2d").drawImage(img, 0, 0, cv.width, cv.height);
    out.push(cv.toDataURL("image/jpeg", 0.88));
    URL.revokeObjectURL(url);
  }
  return out;
}
$("#btnAI").onclick = async () => {
  if (!llms.length) { toast("先到「设置 → 大模型 API」添加一个"); show("settings"); setTimeout(() => $("#btnLLMAdd").scrollIntoView({ block: "center" }), 100); return; }
  const imgs = await formImages().catch(() => []);
  const t = mainTextarea();
  openModal(`<div class="row between" style="margin-bottom:12px"><h3 style="margin:0">✦ AI 写提示词</h3><span class="muted small mono">${esc($("#h3Mode").value)} · ${esc($("#h3Dur").value)} 秒</span></div>
  <div class="ai-box">
    <div class="left">
      <div class="ai-row"><span class="lbl">模型</span><select id="aiProv" class="grow">${llms.map(p => `<option value="${esc(p.id)}" ${p.id === (defaultLLM || llms[0].id) ? "selected" : ""}>${esc(p.name)} · ${esc(p.model)}</option>`).join("")}</select></div>
      <div class="seg" id="aiTab"><button type="button" data-t="new">从想法生成</button><button type="button" data-t="edit">改写当前提示词</button></div>
      <textarea id="aiIn" placeholder=""></textarea>
      ${imgs.length ? `<label class="chk" style="margin:0"><input type="checkbox" id="aiImg" ${aiState.withImg ? "checked" : ""}>附带表单里的 ${imgs.length} 张图（让模型看图写）</label><div class="ai-thumbs">${imgs.map(u => `<img src="${u}">`).join("")}</div>` : `<p class="hint" style="margin:0">首帧 / 尾帧模式下，先在表单里放好图片，这里就能让模型看图来写。</p>`}
      <div class="ai-row"><button type="button" class="btn primary" id="aiGo">生成</button><button type="button" class="btn" id="aiStop" hidden>停止</button></div>
      <p class="hint" style="margin:0">写出镜头、运镜、台词、环境音、配乐。时长和模式取自生成页的设置。${guideInfo.imported ? "按你导入的 H3 指南输出。" : "使用内置通用说明（可在设置里导入 H3 官方指南）。"}</p>
    </div>
    <div class="ai-out">
      <div class="ai-meta" id="aiMeta"></div>
      <textarea id="aiOut" spellcheck="false" placeholder="生成结果会出现在这里，可以直接修改"></textarea>
      <div class="ai-row" style="justify-content:flex-end"><button type="button" class="btn ghost" data-close>关闭</button><button type="button" class="btn" id="aiCopy">复制</button><button type="button" class="btn" id="aiInsert">插入光标处</button><button type="button" class="btn primary" id="aiUse">填入提示词框</button></div>
    </div>
  </div>`);
  const syncTab = () => {
    $$("#aiTab button").forEach(b => b.classList.toggle("on", b.dataset.t === aiState.tab));
    $("#aiIn").value = aiState.tab === "new" ? aiState.idea : aiState.instr;
    $("#aiIn").placeholder = aiState.tab === "new" ? "用中文写想法，比如：黄昏草地上，一个女孩回头看镜头，风吹起头发，她轻声说「该回家了」，远处有火车声" : "说要怎么改，比如：镜头改成慢慢推近；加一句台词；配乐换成钢琴";
  };
  $$("#aiTab button").forEach(b => b.onclick = () => { if (aiState.tab === "new") aiState.idea = $("#aiIn").value; else aiState.instr = $("#aiIn").value; aiState.tab = b.dataset.t; syncTab(); });
  syncTab();
  $("#aiIn").focus();
  $("#aiCopy").onclick = () => { navigator.clipboard.writeText($("#aiOut").value).then(() => toast("已复制")); };
  $("#aiUse").onclick = () => { if (!t) return toast("这个模板没有文本参数"); t.value = $("#aiOut").value.trim(); updCount(t); closeModal(); toast("已填入提示词框"); };
  $("#aiInsert").onclick = () => { closeModal(); insertText($("#aiOut").value.trim()); };
  $("#aiStop").onclick = () => aiCtl && aiCtl.abort();
  $("#aiGo").onclick = async () => {
    const text = $("#aiIn").value.trim();
    if (aiState.tab === "new") aiState.idea = text; else aiState.instr = text;
    if (!text) return $("#aiIn").focus();
    if (aiState.tab === "edit" && !(t && t.value.trim())) return toast("提示词框是空的，先用「从想法生成」");
    aiState.withImg = $("#aiImg") ? $("#aiImg").checked : false;
    const body = { provider: $("#aiProv").value, mode: $("#h3Mode").value, duration: +$("#h3Dur").value || 6,
      idea: aiState.tab === "new" ? text : "", instruction: aiState.tab === "edit" ? text : "", current: aiState.tab === "edit" ? t.value : "",
      images: aiState.withImg ? imgs : [] };
    const out = $("#aiOut"); out.value = ""; $("#aiMeta").textContent = "连接中…";
    $("#aiGo").hidden = true; $("#aiStop").hidden = false;
    aiCtl = new AbortController(); const t0 = Date.now(); let n = 0;
    try {
      const r = await fetch("/api/llm/write", { method: "POST", headers: { "X-Key": KEY, "Content-Type": "application/json" }, body: JSON.stringify(body), signal: aiCtl.signal });
      if (!r.ok) { let m = "HTTP " + r.status; try { m = (await r.json()).error || m; } catch { } throw new Error(m); }
      const rd = r.body.getReader(), dec = new TextDecoder(); let buf = "";
      for (;;) {
        const { value, done } = await rd.read(); if (done) break;
        buf += dec.decode(value, { stream: true });
        let i;
        while ((i = buf.indexOf("\n\n")) >= 0) {
          const chunk = buf.slice(0, i); buf = buf.slice(i + 2);
          const ev = (chunk.match(/^event: (.*)$/m) || [])[1], data = (chunk.match(/^data: (.*)$/m) || [])[1];
          if (!data) continue;
          const v = JSON.parse(data);
          if (ev === "start") $("#aiMeta").textContent = `${v.provider} · ${v.model} · 生成中…`;
          else if (ev === "delta") { out.value += v; n += v.length; out.scrollTop = out.scrollHeight; $("#aiMeta").textContent = `生成中… ${((Date.now() - t0) / 1000).toFixed(0)} 秒 · ${n} 字符`; }
          else if (ev === "error") throw new Error(v);
          else if (ev === "done") $("#aiMeta").textContent = `完成 · ${((Date.now() - t0) / 1000).toFixed(1)} 秒 · ${out.value.trim().split(/\s+/).length} 词`;
        }
      }
      out.value = out.value.replace(/^```[a-z]*\n?|```\s*$/g, "").trim();
    } catch (e) { $("#aiMeta").textContent = e.name === "AbortError" ? "已停止" : "出错：" + e.message; }
    finally { $("#aiGo").hidden = false; $("#aiStop").hidden = true; $("#aiGo").textContent = "重新生成"; aiCtl = null; }
  };
};

// ---------------- local mode (test) ----------------
let gAuthURL = "";
async function loadLocal() {
  let st = { signed_in: false };
  try { st = await api("/api/local/status"); } catch { }
  $("#gDot").className = "dot " + (st.signed_in ? "s-ok" : "");
  $("#gState").textContent = st.signed_in ? "已登录 Google：" + (st.email || "") : "未登录 Google";
  $("#btnGLogin").hidden = st.signed_in; $("#btnGLogout").hidden = !st.signed_in;
  $("#gTools").hidden = !st.signed_in;
  if (st.signed_in) $("#gCodeBox").hidden = true;
}
$("#btnGLogin").onclick = async () => {
  $("#gErr").textContent = "";
  const r = await api("/api/local/login", { json: {} }); gAuthURL = r.url;
  $("#gCodeBox").hidden = false; $("#gCode").focus();
};
$("#gOpenAgain").onclick = e => { e.preventDefault(); if (gAuthURL) api("/api/open-url", { json: { url: gAuthURL } }); };
$("#btnGCode").onclick = async () => {
  $("#gErr").textContent = ""; $("#btnGCode").disabled = true;
  try { const r = await api("/api/local/code", { json: { code: $("#gCode").value } }); toast("已登录 " + (r.email || "")); $("#gCode").value = ""; loadLocal(); }
  catch (e) { $("#gErr").textContent = e.message; }
  finally { $("#btnGCode").disabled = false; }
};
$("#btnGLogout").onclick = async () => { if (!confirm("退出 Google 登录？")) return; await api("/api/local/logout", { json: {} }); loadLocal(); };
$("#btnGCCU").onclick = async () => {
  $("#gCCU").textContent = "查询中…"; $("#gErr").textContent = "";
  try { const r = await api("/api/local/ccu", { json: {} }); $("#gCCU").textContent = `余额 ${r.balance.toFixed(2)} CU · 当前消耗 ${r.per_hour.toFixed(2)}/小时 · 运行中 ${r.assignments} 台${r.running.length ? "（" + r.running.join("、") + "）" : ""}`; }
  catch (e) { $("#gCCU").textContent = ""; $("#gErr").textContent = e.message; }
};
$("#btnGSmoke").onclick = async () => {
  const log = $("#gLog"); log.hidden = false; log.textContent = ""; $("#gErr").textContent = "";
  $("#btnGSmoke").disabled = true;
  const add = s => { log.textContent += s + "\n"; log.scrollTop = log.scrollHeight; };
  try {
    const r = await fetch("/api/local/smoke", { method: "POST", headers: { "X-Key": KEY, "Content-Type": "application/json" }, body: JSON.stringify({ accelerator: $("#gAcc").value }) });
    const rd = r.body.getReader(), dec = new TextDecoder(); let buf = "";
    for (;;) {
      const { value, done } = await rd.read(); if (done) break;
      buf += dec.decode(value, { stream: true }); let i;
      while ((i = buf.indexOf("\n\n")) >= 0) {
        const ch = buf.slice(0, i); buf = buf.slice(i + 2);
        const ev = (ch.match(/^event: (.*)$/m) || [])[1], d = JSON.parse((ch.match(/^data: (.*)$/m) || [])[1] || "null");
        if (ev === "log") add(d); else if (ev === "fail") add("✗ " + d); else if (ev === "ok") add("✓ " + d); else if (ev === "done") add(`— 用时 ${d.toFixed(0)} 秒`);
      }
    }
  } catch (e) { add("✗ " + e.message); }
  finally { $("#btnGSmoke").disabled = false; }
};

// ---------------- load panel ----------------
const loadHist = { util: [], vram: [], ram: [], cpu: [] };
let loadBusy = false;
function pushHist(k, v) { if (v == null || isNaN(v)) return; const h = loadHist[k]; h.push(v); if (h.length > 72) h.shift(); }
function spark(k, max) {
  const h = loadHist[k]; if (h.length < 2) return `<svg class="spark"></svg>`;
  const W = 100, H = 26, n = h.length, mx = max || Math.max(...h, 1);
  const pts = h.map((v, i) => `${(i / (n - 1) * W).toFixed(1)},${(H - 2 - Math.min(1, v / mx) * (H - 4)).toFixed(1)}`);
  const first = pts[0].split(",")[0];
  return `<svg class="spark" viewBox="0 0 ${W} ${H}" preserveAspectRatio="none"><polygon class="area" points="${first},${H} ${pts.join(" ")} ${W},${H}"/><polyline points="${pts.join(" ")}"/></svg>`;
}
const gb = b => (b / 1073741824);
function meter(k, label, val, unit, sub, pct, sparkKey, sparkMax, na) {
  const cls = pct >= 92 ? "crit" : pct >= 80 ? "hot" : "";
  return `<div class="meter ${na ? "na" : ""}"><div class="k"><span>${label}</span><span>${sub || ""}</span></div>
    <div class="v">${val}${unit ? `<small>${unit}</small>` : ""}</div>
    ${pct != null ? `<div class="mbar ${cls}"><div style="width:${Math.max(0, Math.min(100, pct))}%"></div></div>` : ""}
    ${sparkKey ? spark(sparkKey, sparkMax) : ""}</div>`;
}
async function pollLoad() {
  if (loadBusy || page !== "console" || document.hidden || !$("#loadLive").checked) return;
  if (!gst || gst.state !== "ready") { if (!$("#loadCard .load-empty")) { $("#loadCard").innerHTML = `<div class="load-empty">GPU 开机后，这里显示实时负载</div>`; for (const k in loadHist) loadHist[k] = []; } return; }
  loadBusy = true;
  let d; try { d = await api("/api/gpu/stats"); } catch { loadBusy = false; return; }
  loadBusy = false;
  if (!d.ready) return;
  const ss = d.system_stats || {}, dev = (ss.devices || [])[0] || {}, sys = ss.system || {};
  const nv = d.nv && d.nv.gpus && d.nv.gpus[0];
  const vramTot = nv ? nv.mem_total / 1024 : dev.vram_total ? gb(dev.vram_total) : null;
  const vramUsed = nv ? nv.mem_used / 1024 : dev.vram_total ? gb(dev.vram_total - dev.vram_free) : null;
  const ramTot = d.nv && d.nv.ram_total ? gb(d.nv.ram_total) : sys.ram_total ? gb(sys.ram_total) : null;
  const ramUsed = d.nv && d.nv.ram_total ? gb(d.nv.ram_used) : sys.ram_total ? gb(sys.ram_total - sys.ram_free) : null;
  pushHist("util", nv ? nv.util : null); pushHist("vram", vramUsed); pushHist("ram", ramUsed); pushHist("cpu", d.nv ? d.nv.cpu : null);
  const q = d.queue || { running: 0, pending: 0 };
  const name = (nv && nv.name) || (dev.name || "").replace(/^cuda:\d+\s*/, "").replace(/\s*:\s*cudaMallocAsync$/, "") || "GPU";
  const local = d.mode === "local";
  const cells = [
    nv ? meter("u", "GPU 利用率", nv.util.toFixed(0), "%", "", nv.util, "util", 100) : meter("u", "GPU 利用率", "—", "", "仅本地模式", null, null, null, true),
    meter("v", "显存", vramUsed != null ? vramUsed.toFixed(1) : "—", vramTot ? `/ ${vramTot.toFixed(0)} GB` : "", vramTot ? Math.round(vramUsed / vramTot * 100) + "%" : "", vramTot ? vramUsed / vramTot * 100 : null, "vram", vramTot),
    meter("r", "内存", ramUsed != null ? ramUsed.toFixed(1) : "—", ramTot ? `/ ${ramTot.toFixed(0)} GB` : "", ramTot ? Math.round(ramUsed / ramTot * 100) + "%" : "", ramTot ? ramUsed / ramTot * 100 : null, "ram", ramTot),
    nv ? meter("t", "温度 · 功耗", nv.temp.toFixed(0), "°C", nv.power_max ? `${nv.power.toFixed(0)}/${nv.power_max.toFixed(0)} W` : `${nv.power.toFixed(0)} W`, nv.power_max ? nv.power / nv.power_max * 100 : null) : meter("t", "温度 · 功耗", "—", "", "仅本地模式", null, null, null, true),
    d.nv && d.nv.cpu != null ? meter("c", "CPU", d.nv.cpu.toFixed(0), "%", "", d.nv.cpu, "cpu", 100) : meter("c", "CPU", "—", "", local ? "" : "仅本地模式", null, null, null, true),
    meter("q", "ComfyUI 队列", `${q.running}`, "运行中", q.pending ? `排队 ${q.pending}` : "无排队", null),
  ];
  const ver = [d.nv && d.nv.disk_total && `磁盘 ${gb(d.nv.disk_used).toFixed(0)}/${gb(d.nv.disk_total).toFixed(0)} GB`, sys.comfyui_version && "ComfyUI " + sys.comfyui_version, sys.pytorch_version && "PyTorch " + sys.pytorch_version, sys.python_version && "Python " + sys.python_version.split(" ")[0]].filter(Boolean).join(" · ");
  $("#loadCard").innerHTML = `<div class="load-head"><span class="dot s-ok"></span><b>${esc(name)}</b><span class="m">${local ? "本地模式 · 每 5 秒" : "网关模式 · 每 5 秒"}</span></div><div class="meters">${cells.join("")}</div>${ver ? `<div class="load-foot">${esc(ver)}</div>` : ""}`;
  $("#loadSub").textContent = nv ? `${nv.util.toFixed(0)}% · 显存 ${vramUsed.toFixed(1)}/${vramTot.toFixed(0)} GB` : vramTot ? `显存 ${vramUsed.toFixed(1)}/${vramTot.toFixed(0)} GB` : "GPU · 显存 · 内存";
}
setInterval(pollLoad, 5000);
$("#loadLive").onchange = () => pollLoad();
document.addEventListener("visibilitychange", () => pollLoad());

// ---------------- self-update ----------------
let updInfo = null;
async function checkUpdate(manual) {
  if (manual) $("#updMsg").textContent = "检查中…";
  let r; try { r = await api("/api/update/check"); } catch (e) { if (manual) $("#updMsg").textContent = "检查失败：" + e.message; return; }
  updInfo = r;
  if (r.error) { if (manual) $("#updMsg").textContent = "检查失败：" + r.error; return; }
  if (r.available) {
    $("#btnUpd").hidden = false; $("#btnUpd").textContent = `↑ 新版本 v${r.latest.version}`;
    if (manual) { $("#updMsg").innerHTML = `有新版本 v${esc(r.latest.version)} <button type="button" class="btn primary xs" id="btnUpdNow">立即更新</button>`; $("#btnUpdNow").onclick = showUpdate; }
  } else { $("#btnUpd").hidden = true; if (manual) $("#updMsg").textContent = `已是最新版本（v${r.current}）`; }
}
function showUpdate() {
  if (!updInfo || !updInfo.available) return;
  const L = updInfo.latest;
  openModal(`<h3>新版本 v${esc(L.version)}</h3><p class="hint" style="margin:0">当前 v${esc(updInfo.current)}${L.size ? " · 大小 " + (L.size / 1e6).toFixed(1) + " MB" : ""}</p>
    <div class="upd-notes">${esc(L.notes || "（没有更新说明）")}</div>
    <p class="hint">更新会自动下载、校验并重启程序，设置、模板、队列都会保留。${jobs.some(j => ["running", "submitted", "uploading", "downloading"].includes(j.status)) ? "<b>队列里有任务在跑，重启后会自动接着跟进。</b>" : ""}</p>
    <div class="bar" id="updBar" hidden><div></div></div><p class="muted small" id="updProg"></p>
    <div class="row end"><button class="btn" data-close>以后再说</button><button class="btn primary" id="btnUpdGo">立即更新</button></div>`);
  $("#btnUpdGo").onclick = async () => {
    $("#btnUpdGo").disabled = true; $("#updBar").hidden = false;
    try { await api("/api/update/apply", { json: {} }); } catch (e) { $("#updProg").textContent = e.message; $("#btnUpdGo").disabled = false; return; }
    const t = setInterval(async () => {
      let s; try { s = await api("/api/update/status"); } catch { $("#updProg").textContent = "正在重启…"; clearInterval(t); return; }
      $("#updBar > div").style.width = Math.round((s.progress || 0) * 100) + "%";
      $("#updProg").textContent = s.message || "";
      if (s.state === "error") { clearInterval(t); $("#btnUpdGo").disabled = false; }
    }, 500);
  };
}
$("#btnUpd").onclick = showUpdate;
$("#btnUpdCheck").onclick = async () => { await $("#setForm").onsubmit({ preventDefault() { }, target: $("#setForm"), quiet: true }); checkUpdate(true); };
setTimeout(() => checkUpdate(false), 5000);
setInterval(() => checkUpdate(false), 6 * 3600 * 1000);

// ---------------- settings ----------------
async function loadSettings() {
  const r = await api("/api/config");
  const f = $("#setForm");
  for (const [k, v] of Object.entries(r.config)) { const el = f.elements[k]; if (!el) continue; if (el.type === "checkbox") el.checked = !!v; else el.value = v ?? ""; }
  $("#dataDirText").textContent = "配置与模板存放在：" + r.data_dir;
  appCfg = r.config; renderMode();
  $("#verText").textContent = "v" + r.version; $("#updCur").textContent = "当前版本 v" + r.version; $("#updUrl").placeholder = r.default_update_url;
  llms = r.config.llms || []; defaultLLM = r.config.default_llm || ""; renderLLMs();
  const ds = f.elements.draft_scale; if (![...ds.options].some(o => +o.value === +r.config.draft_scale)) ds.add(new Option(Math.round(r.config.draft_scale * 100) + "%", r.config.draft_scale)); ds.value = String(r.config.draft_scale);
  draftHint();
  return r.config;
}
$("#setForm").onsubmit = async e => {
  e.preventDefault();
  const f = e.target, c = {};
  for (const el of f.elements) { if (!el.name || el.name === "llmdef") continue; c[el.name] = el.type === "checkbox" ? el.checked : el.name === "draft_scale" ? parseFloat(el.value) || 0.5 : el.name === "local_max_hours" ? parseFloat(el.value) || 0 : el.type === "number" ? parseInt(el.value) || 0 : el.value.trim(); }
  c.llms = llms; c.default_llm = defaultLLM; c.mode = appCfg.mode || "gateway"; c.local_profile = appCfg.local_profile || "";
  try {
    await api("/api/config", { json: c }); appCfg = { ...appCfg, ...c }; const rr = await api("/api/config"); llms = rr.config.llms || []; renderLLMs(); draftHint();
    if (!e.quiet) { $("#setMsg").textContent = "已保存"; clearTimeout($("#setMsg")._t); $("#setMsg")._t = setTimeout(() => $("#setMsg").textContent = "", 2500); }
    pollStatus(); loadProfiles(); connectEvents();
  }
  catch (err) { $("#setMsg").textContent = "保存失败：" + err.message; }
};
$("#btnShowTok").onclick = () => { const i = $("#setForm").elements.token; i.type = i.type === "password" ? "text" : "password"; };
$("#btnTest").onclick = async () => {
  const m = $("#setMsg"); clearTimeout(m._t);
  await $("#setForm").onsubmit({ preventDefault() { }, target: $("#setForm"), quiet: true });
  m.textContent = "正在测试网关连接…"; $("#btnTest").disabled = true;
  try { const st = await api("/api/gate/test", { json: {} }); m.textContent = `✓ 网关连接成功 · GPU ${STATE_CN[st.state] || st.state}${st.you ? " · 身份 " + (st.you.email || "agent") : ""}`; }
  catch (err) { m.textContent = "✗ 网关连接失败：" + err.message; }
  finally { $("#btnTest").disabled = false; }
};
$("#btnPair").onclick = async () => {
  await $("#setForm").onsubmit({ preventDefault() { }, target: $("#setForm") });
  try {
    const r = await api("/api/pair/start", { json: {} });
    $("#pairMsg").textContent = `已在浏览器打开确认页，设备码 ${r.code}`;
    const t = setInterval(async () => {
      const s = await api("/api/pair/state");
      $("#pairMsg").textContent = s.message + (s.state === "pending" ? `（设备码 ${s.code}）` : "");
      if (s.state !== "pending") { clearInterval(t); if (s.state === "ok") { loadSettings(); pollStatus(); loadProfiles(); } }
    }, 2000);
  } catch (e) { $("#pairMsg").textContent = "配对失败：" + e.message + "（Access 可能拦截了 /api/auth/device/*，可改为手动粘贴令牌）"; }
};

// ---------------- boot ----------------
(async () => {
  const c = await loadSettings();
  await loadTemplates();
  if (!c.gate_url) { show("settings"); toast("先填写网关地址和令牌"); }
  await pollStatus();
  loadProfiles();
  connectEvents();
  loadJobs();
  setInterval(pollStatus, 4000);
  setInterval(loadJobs, 2000);
  setInterval(() => { if (page === "results") { /* manual refresh to keep players stable */ } }, 10000);
})();
