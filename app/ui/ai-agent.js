"use strict";
const WBAgent={controller:null,id:"",events:[],support:null};
function agentReveal(){const panel=$("#aiAgentPanel");panel.hidden=false;panel.open=true;$("#aiAgentShow").hidden=false;}
function agentLogText(){return WBAgent.events.map(e=>`${e.time} · 第 ${e.step} 步 · ${e.kind}\n${e.text}`).join("\n\n");}
function agentShowEvent(e){
 if(e.run_id)WBAgent.id=e.run_id;
 if(e.kind!=="heartbeat"){WBAgent.events.push(e);const log=$("#aiAgentLog");log.textContent=agentLogText();log.scrollTop=log.scrollHeight;}
 const labels={start:"已开始",thinking:"模型思考中",action:"正在执行",result:"已收到结果",recovery:"正在调整方法",format:"修正动作格式",finish:"模型报告完成",ask_user:"需要你的帮助",error:"执行中断",stopped:"已停止",limit:"达到步数上限"};
 if(labels[e.kind]){$("#aiAgentState").textContent=labels[e.kind];$("#aiAgentShow").textContent="执行记录 · "+labels[e.kind];}
 if(e.kind!=="heartbeat"){const awaiting=e.kind==="ask_user"&&!!WBAgent.controller;$("#aiAgentReply").hidden=!awaiting;$("#aiAgentReply").closest(".ai-main").classList.toggle("agent-awaiting",awaiting);}
 if(e.kind==="ask_user"&&WBAgent.controller){$("#aiAgentQuestion").textContent=e.text;agentReveal();$("#aiAgentPanel").open=false;$("#aiAgentQuestion").scrollTop=0;}
}
async function agentOpen(){
 if(WBAgent.controller){agentReveal();return;}
 if(AI.busy)return toast("请等当前回复结束");
 let support;try{support=await api("/api/ai/agent/support");}catch(e){return toast(e.message)}
 WBAgent.support=support;
 openModal(`<div class="row between"><h3>自动执行 · 实验功能</h3><button class="btn ghost sm" data-close>关闭</button></div>
 <p>${support.supported?`当前模型：${esc(support.title||support.model)}`:esc(support.reason)}</p>
 <p class="hint">选择工作目录和任务后，模型会连续执行并检查结果。文件工具只访问所选目录；终端以当前 Windows 用户权限运行，所选目录不是终端沙盒。读取的任务文件内容和执行结果会发给当前云端模型。</p>
 <label class="fld">工作目录<input id="agentRoot" placeholder="例如 C:\\Users\\你的用户名\\Documents\\项目" value="${esc(support.last?.root||"")}"></label>
 <label class="fld">任务<textarea id="agentTask" rows="4" placeholder="例如：检查这个项目的启动错误，修复后运行测试，并告诉我结果。">${esc($("#aiText").value)}</textarea></label>
 <div class="row wrap"><label><input type="checkbox" id="agentTerminal" checked> 允许终端执行</label><label><input type="checkbox" id="agentBrowser" checked> 允许独立浏览器</label></div>
 <label class="fld">最多执行步数（最多 100 步，单次任务最长 30 分钟）<input id="agentSteps" type="number" value="30" min="1" max="100"></label>
 <p class="hint">已选技能会作为任务参考。独立浏览器不使用日常登录状态；需要登录时会停下来提示。覆盖文本文件前保存备份；终端修改不自动备份。任务结束后关闭独立浏览器。</p>
 <p id="agentError" class="err" role="status"></p><div class="row end"><button id="agentLast" class="btn" ${support.last?.id?"":"disabled"}>查看上次记录</button><button id="agentBegin" class="btn primary" ${support.supported?"":"disabled"}>开始执行</button></div>`, {dismissOnBackdrop:false});
 $("#agentLast").onclick=async()=>{try{await agentLoadLog(support.last.id);closeModal()}catch(e){$("#agentError").textContent=e.message}};
 $("#agentBegin").onclick=()=>{
  const input={root:$("#agentRoot").value.trim(),task:$("#agentTask").value.trim(),model:support.model,terminal:$("#agentTerminal").checked,browser:$("#agentBrowser").checked,steps:Number($("#agentSteps").value),skills:aiEnabledSkills(),approved:true};
  if(!input.root||!input.task){$("#agentError").textContent="请填写工作目录和任务。";return}
  closeModal();agentStart(input);
 };
}
async function agentStart(input){
 const ctl=new AbortController();WBAgent.controller=ctl;AI.busy=ctl;WBAgent.id="";WBAgent.events=[];
 agentReveal();$("#aiAgentLog").textContent="";$("#aiAgentState").textContent="连接中";$("#aiAgentStop").hidden=false;$("#aiSend").textContent="停止任务";
 let terminal=false;
 try{
  const r=await fetch("/api/ai/agent/run",{method:"POST",signal:ctl.signal,headers:{"X-Key":KEY,"Content-Type":"application/json"},body:JSON.stringify(input)});
  if(!r.ok){const e=await r.json();throw Error(e.error||`HTTP ${r.status}`)}
  const reader=r.body.getReader(),decoder=new TextDecoder();let buffer="";
  const consume=line=>{if(!line.trim())return;const e=JSON.parse(line);agentShowEvent(e);if(["finish","error","stopped","limit"].includes(e.kind))terminal=true;};
  for(;;){const {value,done}=await reader.read();buffer+=decoder.decode(value||new Uint8Array(),{stream:!done});let n;while((n=buffer.indexOf("\n"))>=0){consume(buffer.slice(0,n));buffer=buffer.slice(n+1)}if(done){consume(buffer);break}}
  if(!terminal)throw Error("执行连接提前断开，请查看本机记录；任务已取消。");
 }catch(e){agentShowEvent({time:new Date().toISOString(),step:0,kind:e.name==="AbortError"?"stopped":"error",text:e.name==="AbortError"?"已请求停止任务；正在结束运行中的工具。":e.message})}
 finally{WBAgent.controller=null;if(AI.busy===ctl)AI.busy=null;$("#aiAgentStop").hidden=true;$("#aiAgentReply").hidden=true;$("#aiAgentReply").closest(".ai-main").classList.remove("agent-awaiting");$("#aiSend").textContent="发送";AI.key="";aiOnStatus();}
}
async function agentLoadLog(id){
 const r=await fetch(`/api/ai/agent/logs/${encodeURIComponent(id)}`,{headers:{"X-Key":KEY}});if(!r.ok)throw Error("执行记录读取失败");const text=await r.text();WBAgent.id=id;WBAgent.events=[];agentReveal();
 text.split("\n").filter(Boolean).forEach(line=>agentShowEvent(JSON.parse(line)));
 if(!WBAgent.events.some(e=>["finish","error","stopped","limit"].includes(e.kind)))$("#aiAgentState").textContent="记录未完成，程序或连接可能中断";
}
$("#aiAgentBtn").onclick=agentOpen;
$("#aiAgentContinue").onclick=async()=>{const btn=$("#aiAgentContinue");btn.disabled=true;try{await api("/api/ai/agent/answer",{json:{id:WBAgent.id,text:$("#aiAgentAnswer").value.trim()||"已处理，请继续"}});$("#aiAgentAnswer").value="";$("#aiAgentReply").hidden=true}catch(e){toast(e.message)}finally{btn.disabled=false}};
$("#aiAgentStop").onclick=async e=>{e.preventDefault();e.stopPropagation();const ctl=WBAgent.controller;if(!ctl)return;try{if(WBAgent.id)await api("/api/ai/agent/stop",{json:{id:WBAgent.id}})}finally{ctl.abort()}};
$("#aiAgentHide").onclick=e=>{e.preventDefault();e.stopPropagation();$("#aiAgentPanel").hidden=true;$("#aiAgentShow").focus()};
$("#aiAgentShow").onclick=agentReveal;
$("#aiAgentCollapse").onclick=e=>{e.preventDefault();e.stopPropagation();$("#aiAgentPanel").open=!$("#aiAgentPanel").open};
$("#aiAgentPanel").ontoggle=()=>{$("#aiAgentCollapse").textContent=$("#aiAgentPanel").open?"收起":"展开"};
$("#aiAgentHeight").oninput=()=>{$("#aiAgentPanel").style.setProperty("--agent-log-height",$("#aiAgentHeight").value+"px")};
$("#aiAgentCopy").onclick=async()=>{try{await navigator.clipboard.writeText(agentLogText());toast("已复制执行记录，分享前请检查内容") }catch{openModal(`<h3>复制执行记录</h3><textarea id="agentManualCopy" rows="12"></textarea><button class="btn" data-close>关闭</button>`);$("#agentManualCopy").value=agentLogText();$("#agentManualCopy").select()}};
$("#aiAgentExport").onclick=()=>{const blob=new Blob([agentLogText()],{type:"text/plain;charset=utf-8"}),url=URL.createObjectURL(blob),a=document.createElement("a");a.href=url;a.download=`Wakeloom-task-${WBAgent.id||"log"}.txt`;a.click();setTimeout(()=>URL.revokeObjectURL(url),1000)};
