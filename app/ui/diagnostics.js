"use strict";
let diagnosticEntries = [], diagnosticReport = "", diagnosticLoading = false;
const diagnosticNames = { local: "本地启动", gateway: "网关启动", task: "生成任务", api: "接口错误", http: "请求", ui: "界面", backup: "工作流备份", app: "应用" };
window.reportDiagnostic = (message, stack = "") => {
  // Cap repeated browser errors; reporting failures must never report themselves.
  const now = Date.now(); const last = window.reportDiagnostic.last || 0;
  if (now - last < 1000) return;
  window.reportDiagnostic.last = now;
  fetch("/api/diagnostics/client", {method:"POST",headers:{"X-Key":KEY,"Content-Type":"application/json"},
    body:JSON.stringify({message:String(message).slice(0,2000),stack:String(stack).slice(0,2000)})}).catch(()=>{});
};
window.addEventListener("error", e => window.reportDiagnostic(e.message || "界面资源加载失败", e.error?.stack || ""));
window.addEventListener("unhandledrejection", e => window.reportDiagnostic(e.reason?.message || String(e.reason), e.reason?.stack || ""));
async function loadDiagnostics() {
  if (diagnosticLoading) return;
  diagnosticLoading = true; $("#diagRefresh").disabled = true;
  try {
    const r = await api("/api/diagnostics"); diagnosticEntries = r.entries || []; diagnosticReport = r.report || "";
    $("#diagWarning").textContent = r.warning || ""; renderDiagnostics();
  } catch (e) { $("#diagWarning").textContent = "读取日志失败：" + e.message; }
  finally { diagnosticLoading = false; $("#diagRefresh").disabled = false; }
}
function renderDiagnostics() {
  const level=$("#diagLevel").value, query=$("#diagSearch").value.toLowerCase().trim();
  const rows=diagnosticEntries.filter(e=>(!level||e.level===level)&&(!query||(e.message+" "+e.source+" "+(diagnosticNames[e.source]||"")).toLowerCase().includes(query)));
  $("#diagCount").textContent = `${rows.length} / ${diagnosticEntries.length} 条`;
  $("#diagList").innerHTML = rows.length ? rows.slice().reverse().map(e=>{
    const d=new Date(e.time), when=isNaN(d)?e.time:d.toLocaleTimeString("zh-CN",{hour12:false});
    const date=isNaN(d)?"":d.toLocaleDateString("zh-CN");
    const level=["error","warn"].includes(e.level)?e.level:"info";
    return `<article class="diag-row ${level}"><div class="diag-time">${esc(when)}<br>${esc(date)}</div><span class="diag-level">${{error:"错误",warn:"提醒",info:"信息"}[level]}</span><div><div class="diag-source">${esc(diagnosticNames[e.source]||e.source)}</div><pre class="diag-message">${esc(e.message)}</pre></div></article>`;
  }).join("") : `<div class="diag-empty">${diagnosticEntries.length ? "没有符合筛选条件的日志" : "暂无日志，后续启动和错误会自动记录在这里"}</div>`;
}
async function copyDiagnosticReport() {
  await loadDiagnostics();
  if (!diagnosticReport) { toast("暂无可复制的日志"); return; }
  try { await navigator.clipboard.writeText(diagnosticReport); toast("已复制诊断日志，可粘贴给维护者"); }
  catch { diagnosticPreview("复制未成功，可在下方全选后手动复制"); }
}
function diagnosticPreview(title="诊断日志预览") {
  openModal(`<div class="row between"><b>${esc(title)}</b><button type="button" class="btn ghost xs" id="diagPreviewClose">关闭</button></div><p class="hint">以下内容与复制、导出内容一致，分享前请检查。</p><textarea class="diag-preview" id="diagPreviewText" readonly aria-label="诊断日志预览"></textarea>`);
  $("#diagPreviewText").value=diagnosticReport;
  $("#diagPreviewClose").onclick=closeModal;
}
$("#diagRefresh").onclick=loadDiagnostics;
$("#diagLevel").onchange=renderDiagnostics;
$("#diagSearch").oninput=renderDiagnostics;
$("#diagCopy").onclick=copyDiagnosticReport;
$("#diagPreview").onclick=async()=>{await loadDiagnostics();diagnosticPreview();};
$("#diagExport").onclick=async()=>{
  await loadDiagnostics();if(!diagnosticReport)return;
  const blob=new Blob(["\ufeff",diagnosticReport],{type:"text/plain;charset=utf-8"});
  const url=URL.createObjectURL(blob), a=document.createElement("a");
  a.href=url;a.download=`Wakeloom-diagnostics-${new Date().toISOString().replace(/[:.]/g,"-")}.txt`;
  document.body.append(a);a.click();a.remove();setTimeout(()=>URL.revokeObjectURL(url),30000);
  toast("已导出诊断日志");
};
$("#btnDiagnostics").onclick=()=>show("diagnostics");
$("#helpDiagnostics").onclick=()=>{ $("#helpMenu").hidden=true;show("diagnostics"); };
