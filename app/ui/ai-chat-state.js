"use strict";
const AIChatSaves = new Map();
function aiPersistChatState(pid, patch) {
  // Capture now; serialize writes so an older streaming response cannot overwrite a newer edit.
  const captured = JSON.parse(JSON.stringify(patch));
  const done = (AIChatSaves.get(pid) || Promise.resolve()).catch(() => {}).then(() => api("/api/ai/chat-state/" + encodeURIComponent(pid), { method: "PUT", json: captured }));
  AIChatSaves.set(pid, done); done.catch(e => toast("本机对话保存失败：" + e.message)); return done;
}
async function aiLoadChatState(pid) {
  AI.chatLoading = true;
  try {
    const s = await api("/api/ai/chat-state/" + encodeURIComponent(pid));
    if (AI.pid !== pid) return;
    AI.chatDiskPid = pid;
    if (s.exists) {
      const saved = Array.isArray(s.conversations) ? s.conversations : [];
      const ids = new Set(saved.map(c => c.id));
      AI.convs = saved.concat(AI.convs.filter(c => !ids.has(c.id))).slice(0, 60);
      AI.cur = s.current || AI.cur;
      if (!AI.convs.some(c => c.id === AI.cur)) AI.cur = AI.convs[0] ? AI.convs[0].id : "";
      lsSet("ckwb.ai.skills." + pid, s.skills || []);
      lsSet("ckwb.ai.skillrefs." + pid, s.skill_references || {});
    }
  } catch (e) { if (AI.pid === pid) AI.chatDiskPid = ""; toast("本机对话读取失败：" + e.message); }
  finally {
    if (AI.pid === pid) { AI.chatLoading = false; aiSaveConvs(); aiConvList(); aiMsgs(); AI.key = ""; aiOnStatus(); }
  }
}
function aiSaveSkillSelection(pid, ids) {
  lsSet("ckwb.ai.skills." + pid, ids); return aiPersistChatState(pid, { skills: ids });
}
