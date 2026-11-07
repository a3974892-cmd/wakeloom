"use strict";
// ---------------- ComfyUI user-data backup: status line under the GPU session card ----------------
// Relies on app.js globals: $, esc, api, toast, gst, page, renderStatus.

const UD = { st: null, busy: false };

function udTime(t) {
  if (!t) return "";
  const d = new Date(t * 1000), now = new Date();
  const hm = d.toLocaleTimeString("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });
  return d.toDateString() === now.toDateString() ? hm : `${d.getMonth() + 1}/${d.getDate()} ${hm}`;
}

function udRender() {
  const st = UD.st, ready = gst && gst.state === "ready";
  const text = $("#udText"), dot = $("#udDot");
  if (!text) return;
  let s = "工作流备份：", cls = "";
  if (!st) s += "—";
  else if (!st.enabled) s += "已关闭（可在设置里开启）";
  else if (st.running === "restore" || (UD.busy && !st.running)) { s += st.running === "restore" ? "正在恢复…" : "正在备份…"; cls = "busy"; }
  else if (st.running === "backup") { s += "正在备份…"; cls = "busy"; }
  else if (st.last_backup) { s += `上次 ${udTime(st.last_backup)} · ${st.files} 个文件`; cls = "ok"; }
  else s += st.files ? `${st.files} 个文件` : "还没有备份";
  let html = esc(s);
  if (st && st.enabled && st.error) { html += ` · <span class="ud-err">${esc(st.error)}</span>`; cls = "warn"; }
  text.innerHTML = html;
  text.title = st && st.skipped && st.skipped.length ? "未备份：\n" + st.skipped.slice(0, 20).join("\n") : (st && st.dir ? "保存在 " + st.dir : "");
  dot.className = "ud-dot " + cls;
  const b = $("#btnUdBackup");
  b.disabled = UD.busy || !ready || !(st && st.enabled);
  b.title = !ready ? "GPU 就绪后才能备份" : "把 ComfyUI 里保存的工作流和设置备份到这台电脑";
}

async function udRefresh() {
  try { UD.st = await api("/api/userdata/status"); } catch { }
  udRender();
}

$("#btnUdBackup").onclick = async () => {
  UD.busy = true; udRender();
  try {
    const r = await api("/api/userdata/backup", { json: {} });
    UD.st = r.status;
    const n = r.result.downloaded, t = r.result.trashed;
    toast(n || t ? `已备份：更新 ${n} 个文件${t ? `，${t} 个已删除的移到回收站` : ""}` : "已备份，没有变化");
  } catch (e) { toast("备份失败：" + e.message); await udRefresh(); }
  finally { UD.busy = false; udRender(); }
};
$("#btnUdOpen").onclick = () => api("/api/userdata/open", { json: {} }).catch(e => toast(e.message));

// follow the GPU state (renderStatus runs on every status change) and poll while the console is visible
const _udRS = renderStatus;
renderStatus = function () { _udRS(); udRender(); };
setInterval(() => { if (page === "console") udRefresh(); }, 15000);
udRefresh();
