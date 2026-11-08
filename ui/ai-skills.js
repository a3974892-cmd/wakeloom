"use strict";
const AIS = { items: [], pid: "", loading: false, package: null };
function aiSkillReferenceSelection() { return lsGet("ckwb.ai.skillrefs." + AI.pid, {}); }
function aiSkillReferenceMode(id) { const v = aiSkillReferenceSelection()[id]; return v == null ? "auto" : v.length ? "manual" : "main"; }
async function aiSaveSkillReferences(pid, options) { lsSet("ckwb.ai.skillrefs." + pid, options); await aiPersistChatState(pid, { skill_references: options }); }
function aiSkillSelectionKey(pid = AI.pid) { return "ckwb.ai.skills." + pid; }
function aiEnabledSkills() { return lsGet(aiSkillSelectionKey(), []).filter(id => AIS.items.some(s => s.id === id)); }
function aiSkillsLabel() { const n = aiEnabledSkills().length; $("#aiSkillsBtn").textContent = "技能" + (n ? " · " + n : ""); $("#aiSkillsBtn").disabled = !!AI.busy; }
async function aiSkillsSync(force = false) {
  if (AIS.loading || (!force && AIS.pid === AI.pid)) { aiSkillsLabel(); return; }
  const pid = AI.pid; AIS.loading = true;
  try { const data = await api("/api/ai/skills"); AIS.items = data.skills || []; AIS.pid = pid; }
  catch (e) { AIS.pid = ""; toast("技能列表读取失败：" + e.message); }
  finally { AIS.loading = false; aiSkillsLabel(); }
}
async function aiSkillImport(input, pid) {
  let pkg;
  if (typeof input === "string") pkg = await api("/api/ai/skills/import", { json: { github: input } });
  else {
    const files = [...input]; if (!files.length) return;
    const form = new FormData(); for (const f of files) form.append("files", f, f.name);
    form.append("paths", JSON.stringify(files.map(f => f.webkitRelativePath || f.name)));
    pkg = await api("/api/ai/skills/import", { method: "POST", body: form });
  }
  AIS.package = pkg; AIS.failedPaths=[];
  if (pkg.candidates.length === 1) await aiInstallSkillCandidate(pkg.candidates[0].path, pid);
  else await aiSkillsOpen();
}
async function aiInstallSkillCandidate(skillPath, pid) {
  const pkg = AIS.package; if (!pkg) return;
  const request = { import_id: pkg.id, skill_path: skillPath };
  async function install(replace = false) {
    const r = await fetch("/api/ai/skills/install", { method: "POST", headers: { "X-Key": KEY, "Content-Type": "application/json" }, body: JSON.stringify({ ...request, replace }) });
    const data = await r.json();
    if (r.status === 409 && !replace) { if (await appConfirm(`技能「${data.name}」已安装，替换为这次导入的版本？`)) return install(true); return null; }
    if (!r.ok) throw new Error(data.error || "安装失败"); return data;
  }
  const s = await install(); if (!s) return;
  const selected = lsGet(aiSkillSelectionKey(pid), []).filter(id => AIS.items.some(x => x.id === id)); if (!selected.includes(s.id) && selected.length < 8) selected.push(s.id);
  await aiSaveSkillSelection(pid, selected);
  if (pkg.candidates.length === 1) AIS.package = null;
  await aiSkillsSync(true); await aiSkillsOpen(); toast("已安装技能「" + s.name + "」" + (selected.includes(s.id) ? "，已启用" : "；启用数量已达上限"));
}
function aiSkillCatalogHTML() {
  const pkg = AIS.package; if (!pkg) return "";
  return `<div class="ai-skill-catalog"><b>发现 ${pkg.candidates.length} 个技能，可多选或全部安装</b><input id="aiSkillSearch" placeholder="搜索技能名称、说明或目录"><select id="aiSkillCandidates" size="5" multiple></select><p id="aiSkillCandidateInfo" class="small muted"></p><div class="row gap wrap"><button type="button" class="btn primary" id="aiSkillInstallSelected">安装所选技能</button><button type="button" class="btn" id="aiSkillInstallAll">安装全部 ${pkg.candidates.length} 个</button><button type="button" class="btn ghost" id="aiSkillSelectVisible">全选筛选结果</button><button type="button" class="btn ghost" id="aiSkillRetryFailed" hidden>重试失败项</button><button type="button" class="btn ghost" id="aiSkillCancelImport">关闭预览</button></div><p class="hint">已安装的默认跳过。同名但来自不同目录的技能分别保留；安装不等于把全部指令同时塞进模型。</p></div>`;
}
function aiBindSkillCatalog(pid) {
  const pkg = AIS.package, select = $("#aiSkillCandidates"); if (!pkg || !select) return;
  const details = () => { const c = pkg.candidates.find(x => x.path === select.value); $("#aiSkillCandidateInfo").textContent = c ? `已选 ${select.selectedOptions.length} 个\n${c.description}\n${c.path} · ${c.files} 个文件 · ${c.chars} 字（发送时按需选择参考）` : "没有匹配的技能"; $("#aiSkillInstallSelected").disabled = !c; };
  const filter = () => { const q = $("#aiSkillSearch").value.toLowerCase(); select.innerHTML = pkg.candidates.filter(c => (c.name + " " + c.path + " " + c.description).toLowerCase().includes(q)).map(c => `<option value="${esc(c.path)}">${esc(c.name)} · ${esc(c.path)}</option>`).join(""); if (select.options.length) select.selectedIndex = 0; details(); };
  $("#aiSkillSearch").oninput = filter; select.onchange = details; filter();
  $("#aiSkillCancelImport").onclick = () => { AIS.package = null; aiSkillsOpen(); };
  $("#aiSkillSelectVisible").onclick = () => { [...select.options].forEach(o => o.selected = true); details(); };
  $("#aiSkillInstallSelected").onclick = async () => { const paths=[...select.selectedOptions].map(o=>o.value); if(paths.length>1)return aiInstallSkillBatch(paths,pid); const controls = $$("#modalBox button, #modalBox input, #modalBox select"); controls.forEach(b => b.disabled = true); const msg = $("#aiSkillMsg"); msg.textContent = "正在安装所选技能…"; try { await aiInstallSkillCandidate(paths[0], pid); } catch (err) { msg.textContent = err.message; msg.classList.add("err"); } finally { controls.forEach(b => b.disabled = false); } };
  $("#aiSkillInstallAll").onclick = () => aiInstallSkillBatch(pkg.candidates.map(c=>c.path),pid);
  $("#aiSkillRetryFailed").onclick = () => aiInstallSkillBatch(AIS.failedPaths||[],pid);
  $("#aiSkillRetryFailed").hidden=!(AIS.failedPaths&&AIS.failedPaths.length);
}
async function aiInstallSkillBatch(paths,pid){
  if(!paths.length||!AIS.package)return;const msg=$("#aiSkillMsg"),controls=$$("#modalBox button, #modalBox input, #modalBox select");controls.forEach(b=>b.disabled=true);msg.classList.remove("err");msg.textContent="正在批量安装…";let result=null;
  try{const r=await fetch("/api/ai/skills/install-batch",{method:"POST",headers:{"X-Key":KEY,"Content-Type":"application/json"},body:JSON.stringify({import_id:AIS.package.id,paths})});if(!r.ok){const e=await r.json();throw new Error(e.error||"安装失败")};const reader=r.body.getReader(),dec=new TextDecoder();let buf="";
    for(;;){const {value,done}=await reader.read();if(done)break;buf+=dec.decode(value,{stream:true});let i;while((i=buf.indexOf("\n"))>=0){const line=buf.slice(0,i);buf=buf.slice(i+1);if(!line.trim())continue;const d=JSON.parse(line);if(d.finished)result=d;else msg.textContent=`安装 ${d.done}/${d.total} · ${d.name}${d.status==="exists"?"（已安装，跳过）":d.status==="error"?"（失败："+d.message+"）":""}`;}}
    if(!result)throw new Error("安装连接中断。已完成项目会保留，可再次安装以跳过已完成项。");AIS.failedPaths=result.failed||[];await aiSkillsSync(true);await aiSkillsOpen();const final=$("#aiSkillMsg");final.textContent=`新增 ${result.installed} 个，已存在 ${result.existing} 个，失败 ${AIS.failedPaths.length} 个。勾选本次需要的技能，或在聊天中指定技能名称。`;final.classList.toggle("err",AIS.failedPaths.length>0);$("#aiSkillRetryFailed").hidden=!AIS.failedPaths.length;
  }catch(e){msg.textContent=e.message;msg.classList.add("err");}finally{controls.forEach(b=>b.disabled=false);}
}
async function aiSkillsOpen() {
  if (AI.busy) return;
  await aiSkillsSync(true); const pid = AI.pid, enabled = aiEnabledSkills();
  openModal(`<div class="row between"><h3>模型技能</h3><button type="button" class="btn ghost sm" data-close>关闭</button></div>
    <p class="hint">为当前模型选择技能，发送下一条消息时生效，无需重启 GPU。支持 SKILL.md、技能文件夹、ZIP 和公开 GitHub 链接。</p>
    <div class="row gap wrap"><button type="button" class="btn" id="aiSkillFileBtn">安装文件 / ZIP</button><button type="button" class="btn" id="aiSkillFolderBtn">安装文件夹</button><input type="file" id="aiSkillFiles" multiple hidden><input type="file" id="aiSkillFolder" webkitdirectory multiple hidden></div>
    <div class="row gap ai-skill-github"><input id="aiSkillGitHub" class="grow" placeholder="https://github.com/作者/仓库/tree/main/技能目录"><button type="button" class="btn" id="aiSkillGitHubBtn">从 GitHub 安装</button></div>
    <p class="hint">大技能包可安装保存，聊天按问题选择参考文件，也可手动指定；过长主说明和手选参考会节选并在回复中标明。脚本不会自动运行，外部工具仍需相应能力。</p>
    <p class="hint">使用方法：勾选的技能固定启用；未勾选的已安装技能，也可在聊天中写「请使用 code-audit 技能审查这段代码」按名称临时调用。回复里的「本次技能」显示真正使用的技能。</p>
    <p id="aiSkillMsg" class="small" role="status"></p>${aiSkillCatalogHTML()}<input id="aiInstalledSkillSearch" placeholder="搜索已安装的技能名称、说明或目录" aria-label="搜索已安装技能"><div class="ai-skill-list">${AIS.items.length ? AIS.items.map(s => `<div class="ai-skill-card" data-skill-search="${esc((s.name+' '+s.description+' '+(s.source_path||'')).toLowerCase())}"><label class="chk"><input type="checkbox" data-skill-enable="${esc(s.id)}" ${enabled.includes(s.id) ? "checked" : ""}><b>${esc(s.name)}</b></label><p class="small muted">${esc(s.description)}</p>${s.source_path?`<p class="hint">${esc(s.source_path)}</p>`:""}<div class="row gap wrap"><span class="small muted">${s.files} 个文字文件 · ${s.chars} 字${s.skipped && s.skipped.length ? ` · ${s.skipped.length} 项未用作指令` : ""}</span><span class="grow"></span><button type="button" class="btn ghost xs" data-skill-view="${esc(s.id)}">查看 / 参考资料</button><button type="button" class="btn ghost xs" data-skill-delete="${esc(s.id)}">卸载</button></div><label class="ai-skill-mode small">本次参考 <select data-skill-mode="${esc(s.id)}"><option value="auto" ${aiSkillReferenceMode(s.id) === "auto" ? "selected" : ""}>按关键词自动选择</option><option value="main" ${aiSkillReferenceMode(s.id) === "main" ? "selected" : ""}>只用技能主说明</option><option value="manual" ${aiSkillReferenceMode(s.id) === "manual" ? "selected" : ""}>手动指定参考文件</option></select></label></div>`).join("") : `<p class="muted ai-skill-empty">还没有安装技能</p>`}</div><div id="aiSkillReferencePanel" hidden></div><pre id="aiSkillPreview" class="ai-skill-preview" hidden></pre>`);
  const msg = $("#aiSkillMsg");
  aiBindSkillCatalog(pid);
  $("#aiInstalledSkillSearch").oninput=e=>{const q=e.target.value.toLowerCase().trim();$$("[data-skill-search]").forEach(card=>card.hidden=!card.dataset.skillSearch.includes(q));};
  async function run(input) {
    const controls = $$("#modalBox button, #modalBox input"); controls.forEach(b => b.disabled = true); msg.classList.remove("err"); msg.textContent = "正在读取并安装技能…";
    try { await aiSkillImport(input, pid); msg.textContent = ""; } catch (e) { msg.textContent = e.message; msg.classList.add("err"); }
    finally { controls.forEach(b => b.disabled = false); }
  }
  $("#aiSkillFileBtn").onclick = () => $("#aiSkillFiles").click(); $("#aiSkillFolderBtn").onclick = () => $("#aiSkillFolder").click();
  for (const id of ["#aiSkillFiles", "#aiSkillFolder"]) $(id).onchange = e => { const f = [...e.target.files]; e.target.value = ""; run(f); };
  $("#aiSkillGitHubBtn").onclick = () => { const value = $("#aiSkillGitHub").value.trim(); if (value) run(value); else $("#aiSkillGitHub").focus(); };
  $$("[data-skill-enable]").forEach(b => b.onchange = () => {
    const ids = $$("[data-skill-enable]:checked").map(x => x.dataset.skillEnable);
    if (ids.length > 8) { b.checked = false; msg.textContent = "每次最多启用 8 个技能"; return; }
    aiSaveSkillSelection(pid, ids).catch(() => {}); aiSkillsLabel();
  });
  $$("[data-skill-delete]").forEach(b => b.onclick = async () => {
    if (!await appConfirm("卸载此技能？后续消息将不再使用它。")) return;
    try { await api("/api/ai/skills/" + b.dataset.skillDelete, { method: "DELETE" }); await aiSkillsOpen(); } catch (e) { msg.textContent = e.message; }
  });
  $$("[data-skill-view]").forEach(b => b.onclick = async () => {
    try { await aiShowSkillReferences(b.dataset.skillView, pid); } catch (e) { msg.textContent = e.message; }
  });
  $$("[data-skill-mode]").forEach(b => b.onchange = async () => {
    try { const options = aiSkillReferenceSelection(); if (b.value === "manual") { if (options[b.dataset.skillMode] == null) options[b.dataset.skillMode] = []; await aiSaveSkillReferences(pid, options); return await aiShowSkillReferences(b.dataset.skillMode, pid); } if (b.value === "auto") delete options[b.dataset.skillMode]; else options[b.dataset.skillMode] = []; await aiSaveSkillReferences(pid, options); } catch (e) { msg.textContent = e.message; }
  });
}
async function aiShowSkillReferences(id, pid) {
  const s = await api("/api/ai/skills/" + id), panel = $("#aiSkillReferencePanel"), options = aiSkillReferenceSelection(), selected = options[id] || [];
  const preview = $("#aiSkillPreview"); preview.hidden = false; preview.textContent = s.instructions;
  panel.hidden = false; panel.innerHTML = `<h4>${esc(s.name)} · 参考文件</h4><p class="hint">勾选后改为手动指定，未勾选文件不发送。内容过长时按问题选段，回复会标明节选与未带入的资料。</p><div class="ai-skill-reference-files">${(s.references || []).map((p, i) => `<label class="chk"><input type="checkbox" data-ref-index="${i}" ${selected.includes(p.name) ? "checked" : ""}><span>${esc(p.name)} <small class="muted">${[...p.text].length} 字</small></span><button type="button" class="btn ghost xs" data-ref-view="${i}">查看</button></label>`).join("") || `<p class="muted small">此技能只有主说明，没有额外参考文件。</p>`}</div>`;
  if (s.skipped && s.skipped.length) { const details = document.createElement("details"); details.className = "ai-skill-usage"; details.innerHTML = `<summary>${s.skipped.length} 项未作为文字导入</summary><div>${s.skipped.map(esc).join("<br>")}</div>`; panel.append(details); }
  $$("[data-ref-index]", panel).forEach(b => b.onchange = async () => { const names = $$("[data-ref-index]:checked", panel).map(x => s.references[+x.dataset.refIndex].name), latest = aiSkillReferenceSelection(); latest[id] = names; try { await aiSaveSkillReferences(pid, latest); const mode = $(`[data-skill-mode="${id}"]`); if (mode) mode.value = names.length ? "manual" : "main"; } catch (e) { $("#aiSkillMsg").textContent = e.message; } });
  $$("[data-ref-view]", panel).forEach(b => b.onclick = () => { const p = s.references[+b.dataset.refView]; preview.textContent = "--- " + p.name + " ---\n" + p.text; });
}
function aiSkillUsageHTML(usage) {
  if (!usage || !usage.length) return "";
  return `<details class="ai-skill-usage"><summary>本次技能：${usage.map(s => esc(s.name)).join("、")} · ${usage.reduce((n, s) => n + s.references.length, 0)} 份参考</summary>${usage.map(s => `<div><b>${esc(s.name)}</b> · ${s.auto ? "自动选择" : "手动指定"} · ${s.chars} 字<br>${s.main_excerpted ? "<p>主说明已按本次问题选段，未带入部分仍保存在本机。</p>" : ""}${s.references.length ? s.references.map(esc).join("<br>") : "仅技能主说明"}${s.excerpted_references?.length?`<p>以下手选参考按问题节选：${s.excerpted_references.map(esc).join("、")}</p>`:""}${s.omitted ? `<p>另有 ${s.omitted} 份资料未带入；可在「技能 → 参考资料」中手动指定。</p>` : ""}</div>`).join("")}</details>`;
}
$("#aiSkillsBtn").onclick = aiSkillsOpen;
