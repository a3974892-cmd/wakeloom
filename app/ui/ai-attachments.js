"use strict";

// Only small attachment references go in conversation storage. Extracted content stays on disk.
const AIA = { drafts: {}, status: {}, uploading: false, controller: null };
$("#btnPDFLicense").onclick = async () => {
  try { const r = await fetch("/licenses/ledongthuc-pdf.txt"); if (!r.ok) throw new Error("许可文件读取失败"); const text = await r.text(); openModal(`<div class="row between"><h3>PDF 组件许可</h3><button type="button" class="btn ghost sm" data-close>关闭</button></div><pre class="ai-skill-preview">${esc(text)}</pre>`); } catch (e) { toast(e.message); }
};
function aiAttachmentDraftKey() { return AI.pid + ":" + (AI.cur || "new"); }
function aiPendingAttachments() { return AIA.drafts[aiAttachmentDraftKey()] || []; }
function aiAttachmentStatus(text, error = false, key = aiAttachmentDraftKey()) {
  AIA.status[key] = { text, error }; aiAttachmentsRender();
}
function aiAttachmentCards(attachments, removable = false) {
  if (!attachments.length) return "";
  return `<div class="ai-attachment-list">` + attachments.map(a => {
    const pictures = (a.image_parts || []).map(i => `<img alt="${esc(a.name)}" loading="lazy" src="/api/ai/attachments/${encodeURIComponent(a.id)}/preview/${i}?k=${encodeURIComponent(KEY)}">`).join("");
    return `<div class="ai-attachment"><div class="ai-attachment-label"><b title="${esc(a.name)}">${esc(a.name)}</b>${removable ? `<button type="button" class="btn ghost xs" data-attach-remove="${esc(a.id)}" aria-label="移除 ${esc(a.name)}">×</button>` : ""}</div><span class="muted small">${a.files || 1} 个文件${a.chars ? ` · ${a.chars} 字` : ""}${a.images ? ` · ${a.images} 张图片` : ""}</span>${a.binary_files ? `<p class="hint">${a.binary_files} 个程序/二进制：原文件保存在本机，发送静态报告；未执行、未反编译。</p>` : ""}${pictures ? `<div class="ai-attachment-pictures">${pictures}</div>` : ""}${a.skipped && a.skipped.length ? `<details class="small muted"><summary>已略过 ${a.skipped_total||a.skipped.length} 项</summary><div>${a.skipped.map(esc).join("<br>")}</div></details>` : ""}</div>`;
  }).join("") + `</div>`;
}
function aiAttachmentsRender() {
  const box = $("#aiAttachments"); if (!box) return;
  const list = aiPendingAttachments(); box.hidden = !list.length; box.innerHTML = aiAttachmentCards(list, true);
  $$("[data-attach-remove]", box).forEach(b => { b.disabled = !!AI.busy || AIA.uploading; b.onclick = () => { AIA.drafts[aiAttachmentDraftKey()] = list.filter(a => a.id !== b.dataset.attachRemove); aiAttachmentStatus(""); }; });
  $("#aiAttachFiles").disabled = $("#aiAttachFolder").disabled = !!AI.busy || AIA.uploading;
  const s = AIA.status[aiAttachmentDraftKey()]; const status = $("#aiAttachStatus"); status.hidden = !s || !s.text; status.textContent = s ? s.text : ""; status.classList.toggle("err", !!(s && s.error));
  $("#aiAttachHint").textContent = AI.info?.llm?.engine === "llama_cpp_1m" ? "1M 实验 · 文字、文档与程序静态报告 · 图片请用看图配置" : AI.info && AI.info.vision === false ? "按内容读取 · 支持程序静态分析 · 图片需启用看图" : "按内容读取 · 程序静态分析 · 可拖入文件";
  $("#aiAttachCancel").hidden = !AIA.uploading;
  $("#aiVisionBtn").hidden=AI.info?.llm?.engine==="llama_cpp_1m"||!(AI.info&&AI.info.vision===false);
  if (AIA.uploading && !AI.busy) $("#aiSend").disabled = true;
}
async function aiAddAttachments(files) {
  if (AI.busy || AIA.uploading || !files.length) return;
  const key = aiAttachmentDraftKey(); const list = (AIA.drafts[key] || []).slice();
  if (list.length >= 8) { aiAttachmentStatus("每条消息最多添加 8 组附件，请先移除部分附件。", true); return; }
  const names=files.map(f=>f._aiPath||f.webkitRelativePath||f.name),label=names[0].includes("/")?names[0].split("/")[0]:files.length>1?`${files.length} 个文件`:files[0].name;
  const failures=[];let attachment=null;AIA.controller=new AbortController();
  AIA.uploading = true; aiAttachmentStatus("正在读取附件…", false, key);
  try {
    for(let i=0;i<files.length;i++){
      if(AIA.controller.signal.aborted)break;
      if(names[i].split("/").some(n=>[".git","node_modules","__pycache__",".venv","__MACOSX"].includes(n))){failures.push(names[i]+"（开发缓存，已略过）");continue;}
      aiAttachmentStatus(`正在读取 ${i+1}/${files.length}：${names[i]}`,false,key);
      const form=new FormData();form.append("files",files[i],files[i].name);form.append("paths",JSON.stringify([names[i]]));form.append("label",label);if(attachment)form.append("attachment_id",attachment.id);
      try{attachment=await api("/api/ai/attachments",{method:"POST",body:form,signal:AIA.controller.signal});AIA.drafts[key]=list.concat(attachment);}catch(e){if(e.name==="AbortError")break;failures.push(names[i]+"（"+e.message+"）");}
    }
    if(attachment){attachment.skipped_total=(attachment.skipped_total||attachment.skipped?.length||0)+failures.length;attachment.skipped=(attachment.skipped||[]).concat(failures).slice(0,100);AIA.drafts[key]=list.concat(attachment);}
    aiAttachmentStatus(`${AIA.controller.signal.aborted?"已取消后续读取。":""}${attachment?`已添加 ${attachment.files} 个文件，文字按本次问题选段使用。`:"没有添加可读取文件。"}${failures.length?` ${failures.length} 项未读取：${failures.slice(0,3).join("；")}`:""}`,failures.length>0,key);
  } catch (e) { aiAttachmentStatus(e.message, true, key); }
  finally { AIA.uploading = false; AIA.controller=null; AI.key = ""; aiOnStatus(); aiAttachmentsRender(); }
}
$("#aiAttachCancel").onclick=()=>AIA.controller&&AIA.controller.abort();
$("#aiAttachFiles").onclick = () => $("#aiFileInput").click();
$("#aiAttachFolder").onclick = () => $("#aiFolderInput").click();
for (const id of ["#aiFileInput", "#aiFolderInput"]) $(id).onchange = e => { const files = [...e.target.files]; e.target.value = ""; aiAddAttachments(files); };

$("#aiText").addEventListener("paste", e => {
  const files = [...(e.clipboardData && e.clipboardData.items || [])].filter(i => i.kind === "file").map(i => i.getAsFile()).filter(Boolean);
  if (files.length) { e.preventDefault(); aiAddAttachments(files); }
});
const aiDropArea = $("#aiChat .ai-main");
aiDropArea.addEventListener("dragover", e => { if ([...(e.dataTransfer.types || [])].includes("Files")) { e.preventDefault(); aiDropArea.classList.add("ai-dragover"); } });
aiDropArea.addEventListener("dragleave", e => { if (!aiDropArea.contains(e.relatedTarget)) aiDropArea.classList.remove("ai-dragover"); });
aiDropArea.addEventListener("drop", async e => {
  e.preventDefault(); aiDropArea.classList.remove("ai-dragover"); if (AI.busy || AIA.uploading) return;
  const entries = [...(e.dataTransfer.items || [])].filter(i => i.kind === "file").map(i => i.webkitGetAsEntry && i.webkitGetAsEntry()).filter(Boolean);
  if (!entries.length) { aiAddAttachments([...e.dataTransfer.files]); return; }
  const files = [];
  async function read(entry, prefix = "") {
    if ([".git", "node_modules", "__pycache__", ".venv", "__MACOSX"].includes(entry.name)) return;
    if (entry.isFile) { const f = await new Promise((resolve, reject) => entry.file(resolve, reject)); f._aiPath = prefix + entry.name; files.push(f); }
    else if (entry.isDirectory) { const reader = entry.createReader(); for (;;) { const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject)); if (!batch.length) break; for (const child of batch) await read(child, prefix + entry.name + "/"); } }
  }
  try { for (const entry of entries) await read(entry); await aiAddAttachments(files); } catch (err) { aiAttachmentStatus(err.message, true); }
});

function aiAttachmentUsageHTML(items){if(!items||!items.length)return "";return `<details class="ai-attachment-usage ai-skill-usage"><summary>本次附件：${items.reduce((n,a)=>n+a.used_files,0)} 个文件 · ${items.reduce((n,a)=>n+a.chars,0)} 字 · ${items.reduce((n,a)=>n+a.images,0)} 张图片</summary>${items.map(a=>`<div><b>${esc(a.name)}</b>：${a.used_files}/${a.files} 个文件，${a.chars}/${a.total_chars} 字${a.omitted_images?`，另有 ${a.omitted_images} 张图片未带入`:""}<br>${a.excerpts.map(p=>`${esc(p.name)} · 第 ${p.start}–${p.end} 字`).join("<br>")}</div>`).join("")}<p>文件文字或程序静态报告保存在本机；本次按问题选用片段，可在下一条消息中指定文件名与关注内容。</p></details>`;}

function aiContextUsageHTML(info){if(!info)return "";return `<p class="hint ai-context-usage">${info.context_tokens?`1M 实验 · 本次输入 ${Number(info.input_tokens).toLocaleString()} tokens · 预留 16,384 tokens 用于回复；`:""}本次带入历史 ${info.history_used || 0}/${info.history_total || 0} 条；${info.history_excerpted?`其中 ${info.history_excerpted} 条长消息保留首尾节选；`:""}${info.history_omitted?`${info.history_omitted} 条较早记录未带入；`:""}${info.skill_excerpts?`${info.skill_excerpts} 个技能主说明按问题选段；`:""}${info.skipped_skills?`${info.skipped_skills} 个技能本次未带入；`:""}完整记录和资料仍保存在本机。</p>`;}
