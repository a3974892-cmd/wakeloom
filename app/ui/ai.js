"use strict";
// ---------------- AI page: chat with the profile's LLM, or text-to-image with its checkpoint ----------------
// Relies on app.js globals: $, $$, esc, api, toast, show, page, gst, profiles, jobs, appCfg, KEY, fileURL,
// preview, wake, isLocal, STATE_CN, BUSY.

const AI = { key: "", info: null, infoAt: 0, infoKey: "", busy: null, convs: [], cur: "", pid: "", imgInit: "", galSig: "" };
const lsGet = (k, d) => { try { const v = localStorage.getItem(k); return v == null ? d : JSON.parse(v); } catch { return d; } };
const lsSet = (k, v) => { try { localStorage.setItem(k, JSON.stringify(v)); } catch { } };

function aiProfiles() { return (profiles && profiles.profiles || []).filter(p => p.ai && p.ai.type); }
// user-added "any model" profiles: no ai block, used through a linked 生成-page template
function aiGenericProfiles() { return (profiles && profiles.profiles || []).filter(p => !p.ai && ((p.workflows || []).length || p.id.startsWith("custom-"))); }
function cmItem(pid) { return typeof CM !== "undefined" ? (CM.items || []).find(x => x.id === pid) : null; }
function aiCurrent() {
  const p = gst && gst.profile && profiles && profiles.profiles && profiles.profiles.find(x => x.id === gst.profile);
  return p && p.ai ? p : null;
}
function aiTplId(pid) { return "ai-" + pid.toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-+|-+$/g, ""); }

// re-render on every status change (renderStatus runs every second while the GPU is up, so this must stay cheap)
const _aiRS = renderStatus;
renderStatus = function () { _aiRS(); if (page === "ai") aiOnStatus(); };

function aiRender() { AI.key = ""; aiOnStatus(); aiGallery(); }

async function aiOnStatus() {
  const p = aiCurrent();
  const st = gst ? gst.state : "";
  const ready = st === "ready";
  // ask the VM whether the model is loaded (cheap, but not more than every 5 s)
  if (p && ready && (!AI.info || AI.infoKey !== gst.session_started + p.id || (!aiModelReady() && Date.now() - AI.infoAt > 5000))) {
    if (!AI.busyInfo) {
      AI.busyInfo = true; AI.infoAt = Date.now();
      try { AI.info = await api("/api/ai/info"); } catch (e) { AI.info = { error: e.message }; }
      AI.infoKey = gst && (gst.session_started + p.id); AI.busyInfo = false;
    }
  }
  if (!p || !ready) { AI.info = null; AI.infoKey = ""; }
  const key = [st, gst && gst.profile, p && p.id, aiModelReady(), AI.info && AI.info.error, gst ? 1 : 0, profiles ? aiProfiles().length : -1].join("|");
  if (key === AI.key) return;
  AI.key = key;

  // chip
  let chip = "—", cls = "";
  if (!gst) chip = gerr ? "未连接" : "—";
  else if (!p) { chip = st === "sleeping" ? "GPU 休眠中" : st === "ready" ? "当前模型 · 工作流模式" : (STATE_CN[st] || st); cls = st === "ready" ? "ok" : ""; }
  else if (BUSY.has(st)) { chip = (p.ai.title || p.title) + " · " + (STATE_CN[st] || st); cls = "busy"; }
  else if (ready && !aiModelReady()) { chip = (p.ai.title || p.title) + (AI.info && AI.info.error ? " · 不可用" : " · 加载模型中"); cls = AI.info && AI.info.error ? "bad" : "busy"; }
  else if (ready) { chip = (p.ai.title || p.title) + " · 就绪"; cls = "ok"; }
  else chip = STATE_CN[st] || st;
  $("#aiChipText").textContent = chip;
  $("#aiChip").className = "chip ai-chip " + cls;

  const showChat = p && p.ai.type === "llm" && st !== "sleeping" && st !== "failed";
  const showImg = p && p.ai.type === "image" && st !== "sleeping" && st !== "failed";
  $("#aiPick").hidden = !!(showChat || showImg);
  $("#aiChat").hidden = !showChat;
  $("#aiImg").hidden = !showImg;
  if (showChat) aiChatSetup(p);
  else if (showImg) aiImgSetup(p);
  else aiPickRender();
}

function aiModelReady() {
  const p = aiCurrent();
  if (!p || !gst || gst.state !== "ready" || !AI.info || AI.info.error) return false;
  if (p.ai.type === "llm") return !!(AI.info.ready && AI.info.llm_up);
  return AI.info.ready === true;
}

// ---- profile picker ----
const AI_ADD_CARD = `<button type="button" class="card ai-card ai-add" id="aiAddModel"><span class="ai-add-plus">＋</span><b>添加模型</b>
  <span class="muted small">粘贴 Hugging Face 链接，自动识别类型、大小，推荐 GPU 和参数</span></button>`;
function aiPickRender() {
  const list = aiProfiles().concat(aiGenericProfiles());
  const running = gst && gst.state && gst.state !== "sleeping" && gst.state !== "failed";
  const cur = gst && gst.profile && profiles && profiles.profiles && profiles.profiles.find(x => x.id === gst.profile);
  let html = "";
  if (running && cur && !cur.ai) html += profileWorkspaceHTML(cur);
  if (!list.length) {
    html += `<div class="card empty">${isLocal() ? "没有找到 AI 启动配置。" : "网关还没有 AI 启动配置：需要先在 VPS 上运行 <code>gate-ai.py</code> 补丁并重建网关容器。"}</div>`;
    html += `<div class="ai-cards">${AI_ADD_CARD}</div>`;
  } else {
    html += `<div class="ai-cards">` + list.map(p => {
      const it = cmItem(p.id), problem = it && it.problem;
      const wakeBtn = problem ? `<button class="btn sm" data-cmdel="${esc(p.id)}">删除</button>`
        : running ? `<button class="btn sm" disabled>先停止当前 GPU</button>` : `<button class="btn primary sm" data-aiwake="${esc(p.id)}">用它唤醒</button>`;
      const bad = problem ? `<p class="err">无法使用：${esc(problem)}</p>` : "";
      if (!p.ai) { // 通用
        if ((p.workflows || []).length) {
          const current = running && cur && cur.id === p.id;
          return `<div class="card ai-card"><div class="row between"><span class="pill">官方工作流</span><span class="muted small">${current ? "当前已部署" : `约 ${p.size_gb_default ?? "?"} GB`}</span></div><h3>${esc(p.title || p.id)}</h3><p class="muted small">${p.workflows.length} 个工作流 · 在 ComfyUI 中使用</p><p class="small">${esc(workflowLabel((p.workflows || []).find(w => w.id === p.app_default) || {}))}</p><div class="row end">${current ? `<button class="btn primary sm" data-profileworkflows="${esc(p.id)}">使用当前模型</button>` : wakeBtn}</div></div>`;
        }
        const tid = it && it.spec && it.spec.template, tpl = (templates || []).find(t => t.id === tid);
        return `<div class="card ai-card"><div class="row between"><span class="pill ${problem ? "bad" : ""}">通用</span><span class="muted small mono">约 ${p.size_gb_default ?? "?"} GB</span></div>
          <h3>${esc(p.title || p.id)}</h3><p class="muted small">自定义模型 · ComfyUI 文件</p>
          ${problem ? "" : `<p class="small">唤醒后在「生成」页使用关联模板${tpl ? `「<span class="tpl-link" data-tpl="${esc(tpl.id)}">${esc(tpl.name)}</span>」` : ""}</p>`}
          <p class="muted small">建议 ${esc(p.accelerator || "L4")}</p>${bad}<div class="row end">${wakeBtn}</div></div>`;
      }
      const a = p.ai, llm = a.type === "llm";
      const chroma = !llm && a.image.arch === "chroma";
      const long = llm && a.llm.engine === "llama_cpp_1m";
      const meta = long ? "1,000,000 tokens · 实验 · 纯文字" : llm ? `Ollama · 上下文 ${Math.round((a.llm.ctx || 32768) / 1024)}K` : `${chroma ? "Chroma (FLUX)" : "SDXL 检查点"} · 默认 ${a.image.width}×${a.image.height} · ${a.image.steps} 步`;
      const gpu = long ? "显存至少 80 GB；长输入处理较慢，完整窗口效果待测试" : p.id.startsWith("custom-") ? `建议 ${p.accelerator || "L4"}` : llm ? "建议 G4 / A100 / H100（显存 ≥ 40 GB）" : chroma ? "建议 G4 / A100（显存 ≥ 32 GB）" : "任意 GPU（L4 就够）";
      const btn = wakeBtn;
      return `<div class="card ai-card"><div class="row between"><span class="pill ${problem ? "bad" : llm ? "llm" : "img"}">${llm ? "对话" : "生图"}</span><span class="muted small mono">约 ${p.size_gb_default ?? "?"} GB</span></div>
        <h3>${esc(a.title || p.title)}</h3><p class="muted small">${p.id.startsWith("custom-") ? "自定义模型" : esc(p.title)}</p><p class="small">${esc(meta)}</p><p class="muted small">${esc(gpu)}</p>${bad}
        <div class="row end">${btn}</div></div>`;
    }).join("") + AI_ADD_CARD + `</div>`;
  }
  $("#aiPick").innerHTML = html;
  bindProfileWorkspace($("#aiPick"));
  $$("[data-aiwake]").forEach(b => b.onclick = () => aiWake(b.dataset.aiwake));
  $$("#aiPick [data-cmdel]").forEach(b => b.onclick = () => cmDelete(b.dataset.cmdel));
  $$("#aiPick [data-tpl]").forEach(s => s.onclick = () => { show("gen"); selectTpl(s.dataset.tpl); });
}

async function aiWake(pid) {
  const p = (profiles && profiles.profiles || []).find(x => x.id === pid); if (!p) return;
  const acc = p.accelerator || "";
  const accSel = $("#wakeAcc");
  const canAcc = accSel && !accSel.disabled && !$("#accWrap").hidden;
  if (!await appConfirm(`用「${(p.ai && p.ai.title) || p.title}」唤醒 GPU？\n\n要下载约 ${p.size_gb_default ?? "?"} GB 模型${canAcc && acc ? `，GPU 用 ${acc}` : ""}。`)) return;
  $("#wakeProfile").value = pid;
  $("#wakeProfile").dispatchEvent(new Event("change"));
  if (canAcc && acc && [...accSel.options].some(o => o.value === acc)) accSel.value = acc;
  await wake();
  AI.key = "";
}

// ---- chat ----
function aiConvKey() { return "ckwb.ai.convs." + AI.pid; }
function aiChatSetup(p) {
  const ready = aiModelReady();
  if (AI.pid !== p.id) {
    AI.pid = p.id;
    AI.convs = lsGet(aiConvKey(), []);
    AI.cur = lsGet(aiConvKey() + ".cur", "");
    if (!AI.convs.find(c => c.id === AI.cur)) AI.cur = AI.convs[0] ? AI.convs[0].id : "";
    const s = lsGet("ckwb.ai.chatset." + p.id, null) || { think: p.ai.llm.think !== false, temp: p.ai.llm.temperature ?? 0.7, sys: p.ai.llm.system || "" };
    $("#aiThink").checked = !!s.think; $("#aiTemp").value = s.temp; $("#aiTempV").textContent = (+s.temp).toFixed(2); $("#aiSys").value = s.sys || "";
    $("#aiSys").hidden = !s.sys;
    aiLoadChatState(p.id);
    aiConvList(); aiMsgs();
  }
  $("#aiText").disabled = !ready || !!AI.chatLoading;
  $("#aiSend").disabled = (!ready || !!AI.chatLoading) && !AI.busy;
  $("#aiText").placeholder = ready ? "输入消息，Enter 发送，Shift+Enter 换行" : (AI.info && AI.info.error ? AI.info.error : "模型还在加载，GPU 就绪并完成「llm」阶段后就能聊天");
  aiAttachmentsRender();
  aiSkillsSync();
}
function aiSaveSet() {
  lsSet("ckwb.ai.chatset." + AI.pid, { think: $("#aiThink").checked, temp: +$("#aiTemp").value, sys: $("#aiSys").value });
}
$("#aiThink").onchange = aiSaveSet;
$("#aiTemp").oninput = () => { $("#aiTempV").textContent = (+$("#aiTemp").value).toFixed(2); aiSaveSet(); };
$("#aiSys").oninput = aiSaveSet;
$("#aiSysBtn").onclick = () => { $("#aiSys").hidden = !$("#aiSys").hidden; if (!$("#aiSys").hidden) $("#aiSys").focus(); };

function aiSaveConvs() {
  AI.convs = AI.convs.slice(0, 60);
  try { localStorage.setItem(aiConvKey(), JSON.stringify(AI.convs)); localStorage.setItem(aiConvKey() + ".cur", JSON.stringify(AI.cur)); }
  catch { if (AI.chatDiskPid !== AI.pid) toast("对话保存失败，本机存储空间可能已满；请先复制重要内容"); }
  if (!AI.chatLoading && AI.chatDiskPid === AI.pid) aiPersistChatState(AI.pid, { conversations: AI.convs, current: AI.cur }).catch(() => {});
}
function aiConv() { return AI.convs.find(c => c.id === AI.cur); }
function aiConvList() {
  const box = $("#aiConvList");
  box.innerHTML = AI.convs.length ? AI.convs.map(c => `<div class="ai-conv ${c.id === AI.cur ? "on" : ""}" data-cid="${c.id}"><span>${esc(c.title || "新对话")}</span><button class="x" data-cdel="${c.id}" title="删除">×</button></div>`).join("")
    : `<p class="muted small ai-conv-empty">还没有对话</p>`;
  $$("[data-cid]", box).forEach(el => el.onclick = e => { if (e.target.dataset.cdel) return; if (AI.busy) return; AI.cur = el.dataset.cid; aiSaveConvs(); aiConvList(); aiMsgs(); });
  $$("[data-cdel]", box).forEach(b => b.onclick = async () => {
    if (AI.busy && AI.cur === b.dataset.cdel) return;
    if (!await appConfirm("删除这个对话？")) return;
    AI.convs = AI.convs.filter(c => c.id !== b.dataset.cdel);
    if (AI.cur === b.dataset.cdel) AI.cur = AI.convs[0] ? AI.convs[0].id : "";
    aiSaveConvs(); aiConvList(); aiMsgs();
  });
}
$("#aiNew").onclick = () => { if (AI.busy) return; AI.cur = ""; aiSaveConvs(); aiConvList(); aiMsgs(); $("#aiText").focus(); };

function mdLite(s) {
  const parts = String(s || "").split(/```/);
  return parts.map((p, i) => {
    if (i % 2) {
      const nl = p.indexOf("\n"), hasLanguage = nl >= 0 && /^[\w+#.-]*$/.test(p.slice(0, nl).trim());
      const language = hasLanguage ? p.slice(0, nl).trim() : "", code = hasLanguage ? p.slice(nl + 1) : p;
      return `<div class="ai-code-block"><div class="ai-code-head"><span>${esc(language || "代码")}</span><button type="button" class="btn ghost xs" data-code-copy aria-label="复制代码">复制代码</button></div><pre><code>${esc(code.replace(/\r?\n$/, ""))}</code></pre></div>`;
    }
    return esc(p).replace(/`([^`\n]+)`/g, "<code>$1</code>").replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>")
      .replace(/^#{1,6} (.+)$/gm, "<b class=\"h\">$1</b>");
  }).join("");
}
function aiMessageTime(m) {
  const date = typeof m.created_at === "number" && m.created_at > 0 ? new Date(m.created_at) : null;
  if (!date || !Number.isFinite(date.getTime())) return '<span class="ai-message-time">时间未记录</span>';
  const pad = n => String(n).padStart(2, "0");
  const text = `${date.getFullYear()}-${pad(date.getMonth()+1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
  return `<time class="ai-message-time" datetime="${date.toISOString()}" title="${m.role === "user" ? "发送时间" : "回复开始时间"}（本机时间）">${text}</time>`;
}
async function aiCopyChatText(text) {
  try { await navigator.clipboard.writeText(text); return true; } catch { /* WebView permission may be unavailable. */ }
  const input = document.createElement("textarea"), active = document.activeElement;
  input.value = text; input.readOnly = true; input.style.cssText = "position:fixed;left:-9999px;top:0";
  document.body.appendChild(input); input.select();
  let copied = false;
  try { copied = document.execCommand("copy"); } catch { }
  input.remove(); if (active && active.isConnected) active.focus({preventScroll:true});
  if (!copied) {
    openModal('<div class="row between"><h3>复制内容</h3><button type="button" class="btn ghost sm" data-close>关闭</button></div><p class="hint">未能自动复制，请按 Ctrl+C 复制下方已选内容。</p><textarea id="aiManualCopy" class="diag-preview" readonly></textarea>');
    const field = $("#aiManualCopy"); field.value = text; field.focus(); field.select();
  }
  return copied;
}
$("#aiMsgs").addEventListener("click", async event => {
  const button = event.target.closest("[data-code-copy]"); if (!button) return;
  const code = button.closest(".ai-code-block")?.querySelector("pre code"); if (!code) return;
  const text = code.textContent; // Capture this block before a streaming repaint replaces it.
  if (await aiCopyChatText(text)) {
    toast("代码已复制"); button.textContent = "已复制";
    setTimeout(() => { if (button.isConnected) button.textContent = "复制代码"; }, 1800);
  }
});
function aiMsgHTML(m, i, last) {
  if (m.role === "user") return `<div class="ai-msg user" data-i="${i}"><div class="ai-user-message"><div class="ai-message-meta"><span>你</span>${aiMessageTime(m)}</div><div class="bubble">${esc(m.content)}${aiAttachmentCards(m.attachments || [])}</div></div></div>`;
  const think = m.thinking ? `<details class="ai-think" ${m.streaming && !m.content ? "open" : ""}><summary>思考过程${m.streaming && !m.content ? "…" : ""}</summary><div>${esc(m.thinking)}</div></details>` : "";
  const body = m.error ? `<p class="err">${esc(m.error)}</p>` : `<div class="md">${mdLite(m.content)}${m.streaming ? '<span class="caret"></span>' : ""}</div>`;
  const acts = m.streaming ? "" : `<div class="ai-acts"><button class="btn ghost xs" data-ma="copy" data-i="${i}">复制</button><button class="btn ghost xs" data-ma="use" data-i="${i}">用作生成提示词</button>${last ? `<button class="btn ghost xs" data-ma="again" data-i="${i}">重新回答</button>` : ""}<span class="muted small mono">${esc(m.stats || "")}</span></div>`;
  return `<div class="ai-msg bot" data-i="${i}"><div class="ai-message-meta"><span>AI</span>${aiMessageTime(m)}</div>${aiContextUsageHTML(m.context_usage)}${aiSkillUsageHTML(m.skill_context)}${aiAttachmentUsageHTML(m.attachment_context)}${think}${body}${acts}</div>`;
}
function aiMsgs() {
  aiAttachmentsRender();
  const c = aiConv(), box = $("#aiMsgs");
  if (!c || !c.msgs.length) {
    box.innerHTML = `<div class="ai-hello"><b>${esc((aiCurrent() && (aiCurrent().ai.title || aiCurrent().title)) || "AI 对话")}</b><p class="muted small">模型运行在你的 Colab GPU 上，对话只保存在这台电脑。</p></div>`;
    return;
  }
  box.innerHTML = c.msgs.map((m, i) => aiMsgHTML(m, i, i === c.msgs.length - 1)).join("");
  $$("[data-ma]", box).forEach(b => b.onclick = () => aiMsgAct(b.dataset.ma, +b.dataset.i));
  box.scrollTop = box.scrollHeight;
}
function aiUpdateLast() {
  const c = aiConv(); if (!c) return;
  const i = c.msgs.length - 1, box = $("#aiMsgs");
  const el = box.querySelector(`.ai-msg[data-i="${i}"]`);
  const nearBottom = box.scrollHeight - box.scrollTop - box.clientHeight < 80;
  const tmp = document.createElement("div"); tmp.innerHTML = aiMsgHTML(c.msgs[i], i, true);
  const fresh = tmp.firstChild;
  // keep the user's open/closed choice on the thinking block
  const oldD = el && el.querySelector(".ai-think"), newD = fresh.querySelector(".ai-think");
  if (oldD && newD && oldD.dataset.touched) { newD.open = oldD.open; newD.dataset.touched = "1"; }
  if (el) el.replaceWith(fresh); else box.appendChild(fresh);
  const oldUsage = el && el.querySelector(".ai-skill-usage"), newUsage = fresh.querySelector(".ai-skill-usage");
  if (oldUsage && newUsage) newUsage.open = oldUsage.open;
  const oldAttachments = el && el.querySelector(".ai-attachment-usage"), newAttachments=fresh.querySelector(".ai-attachment-usage");if(oldAttachments&&newAttachments)newAttachments.open=oldAttachments.open;
  const d = fresh.querySelector(".ai-think"); if (d) d.addEventListener("toggle", () => d.dataset.touched = "1", { once: true });
  $$("[data-ma]", fresh).forEach(b => b.onclick = () => aiMsgAct(b.dataset.ma, +b.dataset.i));
  if (nearBottom) box.scrollTop = box.scrollHeight;
}
async function aiMsgAct(act, i) {
  const c = aiConv(); if (!c) return; const m = c.msgs[i]; if (!m) return;
  if (act === "copy") { if (await aiCopyChatText(m.content)) toast("已复制"); }
  if (act === "use") aiUseAsPrompt(m.content);
  if (act === "again") { if (AI.busy) return; c.msgs.splice(i, 1); aiSaveConvs(); aiMsgs(); aiStream(c); }
}
function aiUseAsPrompt(text) {
  show("gen");
  setTimeout(() => {
    const ta = $$("#primaryFields textarea").find(t => !/negative|反向/i.test(t.closest(".fld") ? t.closest(".fld").textContent : ""));
    if (!ta) { navigator.clipboard && navigator.clipboard.writeText(text); toast("先在左边选一个模板；文字已复制到剪贴板"); return; }
    ta.value = text; ta.dispatchEvent(new Event("input", { bubbles: true })); ta.focus();
    toast("已填入提示词");
  }, 60);
}

$("#aiText").addEventListener("keydown", e => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); aiSend(); }
});
$("#aiSend").onclick = () => AI.busy ? AI.busy.abort() : aiSend();

function aiSend() {
  const text = $("#aiText").value.trim();
  const attachments = aiPendingAttachments().slice();
  if ((!text && !attachments.length) || AI.busy || AI.chatLoading || AIA.uploading || !aiModelReady()) return;
  if (attachments.some(a => a.images > 0) && AI.info && AI.info.vision === false) {
    if (AI.info.llm?.engine === "llama_cpp_1m") { aiAttachmentStatus("1M 实验配置目前仅支持文字。图片请使用原来的「对话与看图」配置；附件已保留。", true); return; }
    aiAttachmentStatus("当前模型尚未加载视觉组件。点击「启用看图」完成补装后，这些图片可继续发送。", true);aiVisionOpen();return;
  }
  const draftKey = aiAttachmentDraftKey();
  let c = aiConv();
  if (!c) { c = { id: Date.now().toString(36), title: (text || attachments[0].name).slice(0, 24), msgs: [], created: Date.now() }; AI.convs.unshift(c); AI.cur = c.id; }
  c.msgs.push({ role: "user", content: text, created_at: Date.now(), ...(attachments.length ? { attachments } : {}) });
  delete AIA.drafts[draftKey]; delete AIA.status[draftKey];
  $("#aiText").value = "";
  aiSaveConvs(); aiConvList(); aiMsgs();
  aiStream(c);
}

async function aiStream(c) {
  const p = aiCurrent(); if (!p) return;
  const msgs = [];
  const sys = $("#aiSys").value.trim();
  if (sys) msgs.push({ role: "system", content: sys });
  for (const m of c.msgs) if (!m.error) msgs.push({ role: m.role, content: m.content, ...(m.attachments ? { attachments: m.attachments.map(a => ({ id: a.id })) } : {}) });
  const bot = { role: "assistant", content: "", thinking: "", streaming: true, created_at: Date.now() };
  c.msgs.push(bot); aiMsgs();
  const ctl = new AbortController(); AI.busy = ctl;
  aiAttachmentsRender();
  $("#aiSend").textContent = "停止"; $("#aiSend").disabled = false;
  const t0 = performance.now();
  let raf = 0; const paint = () => { if (!raf) raf = requestAnimationFrame(() => { raf = 0; if (AI.cur === c.id) aiUpdateLast(); }); };
  try {
    const r = await fetch("/api/ai/chat", {
      method: "POST", signal: ctl.signal, headers: { "X-Key": KEY, "Content-Type": "application/json" },
      body: JSON.stringify({ model: AI.info?.llm?.name||p.ai.llm.name, messages: msgs, skills: aiEnabledSkills(), skill_references: aiSkillReferenceSelection(), num_ctx:AI.info?.llm?.ctx||p.ai.llm.ctx||32768, think: $("#aiThink").checked, temperature: +$("#aiTemp").value })
    });
    if (!r.ok) { let e = "HTTP " + r.status; try { e = (await r.json()).error || e; } catch { } throw new Error(e); }
    const rd = r.body.getReader(), dec = new TextDecoder(); let buf = "";
    for (; ;) {
      const { value, done } = await rd.read(); if (done) break;
      buf += dec.decode(value, { stream: true });
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim(); buf = buf.slice(nl + 1);
        if (!line) continue;
        let d; try { d = JSON.parse(line); } catch { continue; }
        if (d.error) throw new Error(d.error);
        if (d.wb_skills) { bot.skill_context = d.wb_skills; paint(); continue; }
        if (d.wb_context) {bot.context_usage=d.wb_context;paint();continue;}
        if (d.wb_attachments) { bot.attachment_context=d.wb_attachments;paint();continue; }
        const m = d.message || {};
        if (m.thinking) bot.thinking += m.thinking;
        if (m.content) bot.content += m.content;
        if (d.done) {
          if (typeof obMark === "function") obMark("generate"); // first chat counts for the checklist
          const tps = d.eval_count && d.eval_duration ? (d.eval_count / (d.eval_duration / 1e9)).toFixed(1) : "";
          bot.stats = `${d.eval_count || 0} tokens${tps ? " · " + tps + " tok/s" : ""} · ${((performance.now() - t0) / 1000).toFixed(1)} s`;
        }
        paint();
      }
    }
    // models that don't separate their reasoning: pull <think>…</think> out of the text
    const mt = bot.content.match(/^\s*<think>([\s\S]*?)<\/think>\s*/);
    if (mt) { bot.thinking += mt[1]; bot.content = bot.content.slice(mt[0].length); }
    if (!bot.content && !bot.thinking) bot.error = "模型没有返回内容";
  } catch (e) {
    if (e.name === "AbortError") bot.stats = "已停止";
    else { bot.error = "出错了：" + e.message; if (window.reportDiagnostic) window.reportDiagnostic("AI 对话："+e.message,e.stack||""); }
  } finally {
    bot.streaming = false; AI.busy = null;
    $("#aiSend").textContent = "发送";
    if (c.title === "" || !c.title) c.title = (c.msgs[0] && c.msgs[0].content || "").slice(0, 24);
    aiSaveConvs();
    if (AI.cur === c.id) aiMsgs();
    $("#aiStats").textContent = bot.stats || "";
    AI.key = ""; aiOnStatus();
  }
}

// ---- text to image ----
function aiImgKey() { return "ckwb.ai.img." + AI.pid; }
function aiImgSetup(p) {
  const im = p.ai.image;
  if (AI.imgInit !== p.id) {
    AI.imgInit = p.id; AI.pid = p.id;
    const s = lsGet(aiImgKey(), {});
    $("#aiPrompt").value = s.prompt || "";
    $("#aiNeg").value = s.neg ?? (im.negative || "");
    $("#aiW").value = s.w || im.width; $("#aiH").value = s.h || im.height;
    $("#aiSteps").value = s.steps || im.steps; $("#aiCfg").value = s.cfg || im.cfg;
    $("#aiCount").value = s.count || 1; $("#aiSeed").value = s.seed ?? "";
    aiSizeSync();
    AI.galSig = ""; aiGallery();
  }
  const ready = aiModelReady();
  $("#aiGen").disabled = false;
  $("#aiImgHint").textContent = ready ? `模型：${im.checkpoint || im.unet_name} · ${im.sampler} / ${im.scheduler}` : "GPU 还没就绪：现在提交会先进队列，就绪后自动开始。";
}
function aiImgSave() {
  lsSet(aiImgKey(), { prompt: $("#aiPrompt").value, neg: $("#aiNeg").value, w: +$("#aiW").value, h: +$("#aiH").value, steps: +$("#aiSteps").value, cfg: +$("#aiCfg").value, count: +$("#aiCount").value, seed: $("#aiSeed").value });
}
function aiSizeSync() {
  const s = $("#aiW").value + "x" + $("#aiH").value;
  const preset = $$("#aiSize button").find(b => b.dataset.s === s);
  $$("#aiSize button").forEach(b => b.classList.toggle("on", preset ? b === preset : b.dataset.s === "custom"));
  $("#aiWH").hidden = !!preset && !AI.customSize;
}
$$("#aiSize button").forEach(b => b.onclick = () => {
  if (b.dataset.s === "custom") { AI.customSize = true; $("#aiWH").hidden = false; $$("#aiSize button").forEach(x => x.classList.toggle("on", x === b)); return; }
  AI.customSize = false;
  const [w, h] = b.dataset.s.split("x"); $("#aiW").value = w; $("#aiH").value = h; aiSizeSync(); aiImgSave();
});
["#aiPrompt", "#aiNeg", "#aiW", "#aiH", "#aiSteps", "#aiCfg", "#aiCount", "#aiSeed"].forEach(s => $(s).addEventListener("input", aiImgSave));
$("#aiPrompt").addEventListener("keydown", e => { if (e.key === "Enter" && (e.ctrlKey || e.metaKey)) { e.preventDefault(); aiGenerate(); } });
$("#aiGen").onclick = aiGenerate;

async function aiGenerate() {
  const p = aiCurrent(); if (!p || p.ai.type !== "image") return;
  const prompt = $("#aiPrompt").value.trim();
  $("#aiErr").textContent = "";
  if (!prompt) { $("#aiErr").textContent = "先写提示词"; return; }
  const im = p.ai.image;
  const w = Math.round((+$("#aiW").value || im.width) / 16) * 16, h = Math.round((+$("#aiH").value || im.height) / 16) * 16;
  $("#aiGen").disabled = true;
  try {
    const t = await api("/api/ai/image-template", { json: { profile: p.id, title: p.ai.title || p.title, ...im } });
    const K = t.keys || {};
    const seed = $("#aiSeed").value.trim();
    const values = {
      [K.prompt]: prompt, [K.negative]: $("#aiNeg").value, [K.width]: w, [K.height]: h,
      [K.steps]: +$("#aiSteps").value || im.steps, [K.cfg]: +$("#aiCfg").value || im.cfg, [K.seed]: seed ? +seed : 0
    };
    const fd = new FormData();
    fd.append("spec", JSON.stringify({ template_id: t.id, values, random_seeds: seed ? [] : [K.seed], title: "AI生图 " + prompt.slice(0, 20), count: Math.max(1, Math.min(20, +$("#aiCount").value || 1)) }));
    const r = await api("/api/jobs", { method: "POST", body: fd });
    toast(`已加入队列（${r.ids.length} 张）`);
    loadJobs();
  } catch (e) { $("#aiErr").textContent = "提交失败：" + e.message; }
  finally { $("#aiGen").disabled = false; }
}

function aiGallery() {
  const p = aiCurrent(); const box = $("#aiGallery"); if (!box) return;
  if (!p || p.ai.type !== "image") return;
  const tid = aiTplId(p.id);
  const mine = (jobs || []).filter(j => j.template_id === tid).slice(0, 30);
  const sig = JSON.stringify(mine.map(j => [j.id, j.status, Math.round((j.progress || 0) * 20), (j.outputs || []).length]));
  if (sig === AI.galSig) return;
  AI.galSig = sig;
  if (!mine.length) { box.innerHTML = `<div class="ai-gal-empty muted small">生成的图会出现在这里</div>`; return; }
  const items = [];
  for (const j of mine) {
    const imgs = (j.outputs || []).filter(o => o.kind === "image");
    if (j.status === "done" && imgs.length) imgs.forEach(o => items.push(`<button class="ai-tile" data-path="${esc(o.path)}" title="${esc(j.title)}"><img loading="lazy" src="${fileURL(o.path)}"></button>`));
    else if (j.status === "failed" || j.status === "canceled") items.push(`<div class="ai-tile st bad" title="${esc(j.message || "")}"><span>${j.status === "failed" ? "失败" : "已取消"}</span><small>${esc((j.message || "").slice(0, 60))}</small></div>`);
    else if (j.status !== "done") items.push(`<div class="ai-tile st"><span>${esc((JST && JST[j.status]) || j.status)}</span>${j.status === "running" ? `<div class="mini"><div style="width:${Math.round((j.progress || 0) * 100)}%"></div></div>` : ""}</div>`);
  }
  box.innerHTML = items.join("");
  $$("[data-path]", box).forEach(b => b.onclick = () => preview(b.dataset.path, "image"));
}
