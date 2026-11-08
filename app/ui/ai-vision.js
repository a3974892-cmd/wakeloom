"use strict";
const AIV={poll:null};
const aiVisionPhase={prepare:"准备组件",download:"下载并校验组件",register:"注册视觉模型",load:"加载模型",test:"检查真实图片推理",ready:"图片理解已启用"};
async function aiVisionOpen(){
 if(AI.busy)return;let state;try{state=await api("/api/ai/vision/status");}catch(e){toast(e.message);return;}
 const supported=state.can_install;
 openModal(`<div class="row between"><h3>启用图片理解</h3><button type="button" class="btn ghost sm" data-close>关闭</button></div><p>完整理解图片需要加载与模型匹配的视觉组件。附件和聊天记录会保留。</p>${state.local?`<p>内置 Qwen3.8 27B 可在当前 Colab 会话中补装约 0.94 GB 组件。程序保留原文字模型，通过真实图片推理检查后才切换；GPU 会话保持运行。</p>${supported?`<button type="button" class="btn primary" id="aiVisionInstall">下载并启用</button>`:`<p>请先用内置 Qwen3.8 27B 配置唤醒并等待就绪。下次唤醒也可直接选「对话与看图」。</p>`}`:`<p>当前是网关模式。组件需要在 VPS 连接的 Colab 会话中安装，仅更新本机 exe 不会更新服务器。</p><p>下载更新包，将文件放在 VPS 的同一目录，运行：</p><pre class="ai-skill-preview">python3 enable-vision-on-vps.py /opt/comfy-colab-kit</pre><p class="hint">请替换成网关实际安装目录。补装脚本保持现有 GPU 会话和网关运行，完成后点击下方刷新。</p><a class="btn primary" download href="/api/ai/vision/update?k=${encodeURIComponent(KEY)}">下载看图更新包</a>`}<p id="aiVisionProgress" role="status" class="hint"></p><button type="button" class="btn" id="aiVisionRefresh">刷新看图状态</button>`);
 const install=$("#aiVisionInstall");if(install)install.onclick=async()=>{install.disabled=true;try{await api("/api/ai/vision/enable",{json:{}});aiVisionPoll();}catch(e){$("#aiVisionProgress").textContent=e.message;install.disabled=false;}};
 $("#aiVisionRefresh").onclick=async()=>{AI.info=null;AI.infoAt=0;AI.infoKey="";AI.key="";await aiOnStatus();const ok=AI.info?.vision===true;$("#aiVisionProgress").textContent=ok?"图片理解已启用，可以关闭窗口并发送保留的图片。":"尚未检测到图片理解，请查看安装结果。";};
 if(state.job.running)aiVisionPoll();
}
async function aiVisionPoll(){
 clearTimeout(AIV.poll);const panel=$("#aiVisionProgress");if(!panel)return;
 try{const state=await api("/api/ai/vision/status"),job=state.job;panel.textContent=job.error||aiVisionPhase[job.phase]||job.message||"准备中…";panel.classList.toggle("err",!!job.error);
  if(job.running){AIV.poll=setTimeout(aiVisionPoll,1500);return;}
  const install=$("#aiVisionInstall");if(install)install.disabled=false;
  if(job.done){AI.info=null;AI.infoAt=0;AI.infoKey="";AI.key="";await aiOnStatus();panel.textContent=AI.info?.vision===true?"视觉组件和图片推理检查已完成，关闭窗口后可发送保留的图片。":"补装已结束，正在刷新模型能力；可点击「刷新看图状态」。";}
 }catch(e){panel.textContent=e.message;}
}
$("#aiVisionBtn").onclick=aiVisionOpen;
