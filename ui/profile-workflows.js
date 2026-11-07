"use strict";

function workflowLabel(w) {
  const names = {"qwen21-turbo-t2i":"快速文生图 · Qwen-Image 2.1 Turbo", "qwen21-t2i":"高质量文生图 · Qwen-Image 2.1", "qwen21-turbo-edit":"快速图片编辑 · Qwen-Image 2.1 Turbo", "qwen21-edit":"高质量图片编辑 · Qwen-Image 2.1"};
  return names[w.id] || w.title || w.id || "工作流";
}
function templateProfileMismatch(t) {
  if (!t || !gst || gst.state !== "ready" || !gst.profile) return "";
  const required = t.profile || (t.id.startsWith("ai-") ? t.id.slice(3) : "");
  if (!required || required === gst.profile) return "";
  const p = (profiles && profiles.profiles || []).find(p => p.id === gst.profile);
  return `当前运行的是「${p ? p.title : gst.profile}」，此模板需要「${required}」。请使用当前模型的工作流，或停止后以对应配置重新唤醒。`;
}
function profileWorkspaceHTML(p) {
  const ready = gst && gst.state === "ready" && gst.profile === p.id;
  return `<div class="card profile-workspace"><div class="profile-workspace-head"><div><span class="pill ${ready ? "img" : ""}">${ready ? "当前模型已就绪" : "工作流模式"}</span><h3>${esc(p.title || p.id)}</h3><p class="muted small">这个配置使用完整的 ComfyUI 工作流，无需切换到其他模型。打开 ComfyUI 即可使用，默认加载本次启动的工作流。</p></div><button type="button" class="btn primary" data-workspace-open ${ready ? "" : "disabled"}>打开 ComfyUI 使用 ↗</button></div><div class="profile-workflow-list">${(p.workflows || []).map(w => `<div class="profile-workflow-row"><div><b>${esc(workflowLabel(w))}</b><span class="muted small">${esc(w.id)}.json${w.id === p.app_default ? " · 默认工作流" : " · 可能需追加模型"}</span></div>${w.embedded ? `<a class="btn sm" download href="/api/profile-workflows/${encodeURIComponent(p.id)}/${encodeURIComponent(w.id)}?k=${encodeURIComponent(KEY)}">下载工作流</a>` : ""}</div>`).join("")}</div><p class="hint">在 ComfyUI 左侧「工作流」中打开对应名称。找不到时，下载上面的 JSON 后拖入 ComfyUI 页面。要使用本程序的任务队列，可从 ComfyUI 导出 API JSON，再导入下方模板。</p></div>`;
}
function bindProfileWorkspace(root) {
  $$('[data-workspace-open]',root).forEach(b => b.onclick = async () => { try { await api('/api/open-url',{json:{url:'comfy'}}); } catch(e) { toast(e.message); } });
  $$('[data-profileworkflows]',root).forEach(b => b.onclick = () => { show('gen'); renderProfileWorkspace(); $('#profileWorkspace').scrollIntoView({block:'start',behavior:'smooth'}); });
}
let profileWorkspaceKey = "";
function renderProfileWorkspace() {
  const box = $('#profileWorkspace');
  const p = (profiles && profiles.profiles || []).find(p => gst && gst.profile === p.id);
  const visible = p && !p.ai && (p.workflows || []).length && gst.state !== 'sleeping' && gst.state !== 'failed';
  box.hidden = !visible;
  const mismatch = templateProfileMismatch(curTpl);
  const submit = $('#genForm button[type=submit]');
  if (mismatch) { submit.disabled = true; submit.dataset.profileMismatch = '1'; }
  else if (submit.dataset.profileMismatch) { submit.disabled = false; delete submit.dataset.profileMismatch; }
  const signature = JSON.stringify([visible && p, gst && gst.state, mismatch]);
  if (signature === profileWorkspaceKey) return;
  profileWorkspaceKey = signature;
  box.innerHTML = visible ? profileWorkspaceHTML(p) + (mismatch ? `<p class="warn-note">${esc(mismatch)}</p>` : '') : '';
  bindProfileWorkspace(box);
}
const _workspaceRenderStatus = renderStatus;
renderStatus = function () { _workspaceRenderStatus(); renderProfileWorkspace(); };
const _workspaceShow = show;
show = function (p) { _workspaceShow(p); if (p === 'gen') renderProfileWorkspace(); };
const _workspaceSelectTpl = selectTpl;
selectTpl = function (...args) { _workspaceSelectTpl(...args); renderProfileWorkspace(); };
