package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAIContextLongHistoryAndManySkillsStillSendsFolder(t *testing.T) {
	withTempConfig(t, Config{})
	files := map[string][]byte{}
	for i := 0; i < 113; i++ {
		files[fmt.Sprintf("folder/source%d.txt", i)] = []byte(strings.Repeat("folder data ", 5000) + " FOLDER_EVIDENCE")
	}
	a, w := aiTestUpload(t, map[string][]byte{"folder.zip": aiTestZIP(t, files)})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	ids := []string{}
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("%032x", i+1)
		s := &aiSkill{ID: id, Name: fmt.Sprintf("review-%d", i), Description: "Review documents", Instructions: strings.Repeat("Skill background. ", 1000) + " FOLDER_REVIEW_RULE"}
		p, _ := aiSkillPath(id)
		os.MkdirAll(filepath.Dir(p), 0700)
		writeJSON(p, s)
		ids = append(ids, id)
	}
	messages := []map[string]any{{"role": "system", "content": "Keep the reply concise."}, {"role": "user", "content": "Earlier topic", "attachments": []map[string]string{{"id": a.ID}}}, {"role": "assistant", "content": strings.Repeat("old long code ", 3000)}, {"role": "user", "content": "Find FOLDER_EVIDENCE in this folder"}}
	original, _ := json.Marshal(messages)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wbllm/api/chat" {
			t.Error(r.URL.Path)
			w.WriteHeader(404)
			return
		}
		calls++
		var payload struct {
			Messages []map[string]any `json:"messages"`
		}
		json.NewDecoder(r.Body).Decode(&payload)
		total := 0
		all := ""
		for _, m := range payload.Messages {
			c, _ := m["content"].(string)
			total += utf8.RuneCountInString(c)
			all += c
		}
		if total > aiTextLimit || !strings.Contains(all, "FOLDER_EVIDENCE") || !strings.Contains(all, "Earlier topic") || !strings.Contains(all, "Earlier message excerpt") {
			t.Error("request not packed correctly", total)
		}
		fmt.Fprintln(w, `{"message":{"content":"sent"},"done":true}`)
	}))
	defer srv.Close()
	cfg := getConfig()
	cfg.Mode = "gateway"
	cfg.GateURL = srv.URL
	cfg.Proxy = "direct"
	saveConfig(cfg)
	raw, _ := json.Marshal(map[string]any{"model": "test", "messages": messages, "skills": ids})
	w = httptest.NewRecorder()
	aiChatHandler(w, httptest.NewRequest("POST", "/api/ai/chat", bytes.NewReader(raw)))
	if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "wb_context") || !strings.Contains(w.Body.String(), "main_excerpted") {
		t.Fatal(w.Code, w.Body.String())
	}
	after, _ := json.Marshal(messages)
	if !bytes.Equal(original, after) {
		t.Fatal("stored history mutated")
	}
	loaded, e := loadAIAttachment(a.ID)
	if e != nil || loaded.Files != 113 {
		t.Fatal(e)
	}
}

func TestAIContextKeepsWholeRecentTurnsAndManualExcerpts(t *testing.T) {
	withTempConfig(t, Config{})
	s := &aiSkill{ID: strings.Repeat("a", 32), Name: "review", Description: "Review", Instructions: "Use the supplied evidence.", References: []aiAttachmentPart{{Name: "manual.md", Text: strings.Repeat("background ", 3000) + " IMPORTANT_CONCLUSION"}}}
	p, _ := aiSkillPath(s.ID)
	os.MkdirAll(filepath.Dir(p), 0700)
	writeJSON(p, s)
	messages := []map[string]any{{"role": "user", "content": "old"}, {"role": "assistant", "content": strings.Repeat("x", 20000)}, {"role": "user", "content": "recent"}, {"role": "assistant", "content": "recent answer"}, {"role": "user", "content": "Find IMPORTANT_CONCLUSION"}}
	packed, skills, usage, e := aiPrepareChatContext(messages, []string{s.ID}, map[string][]string{s.ID: {"manual.md"}})
	if e != nil {
		t.Fatal(e)
	}
	if usage.HistoryOmitted != 0 || usage.HistoryUsed != 4 || usage.HistoryExcerpted != 1 || len(skills) != 1 || len(skills[0].ExcerptedReferences) != 1 {
		t.Fatal(usage, skills)
	}
	all := ""
	for _, m := range packed {
		all += m["content"].(string)
	}
	if !strings.Contains(all, "IMPORTANT_CONCLUSION") || !strings.Contains(all, "recent answer") || len(all) > aiTextLimit {
		t.Fatal("manual reference or recent history lost")
	}
	_, _, e = aiExpandStoredAttachments(context.Background(), packed, 0)
	if e != nil {
		t.Fatal(e)
	}
}

func TestAIContextLongReplyRetainsFailedSteps(t *testing.T) {
	withTempConfig(t, Config{})
	messages := []map[string]any{
		{"role": "user", "content": "环境是 Windows，本地模式。"},
		{"role": "assistant", "content": "请先尝试 A。"},
		{"role": "user", "content": "A 已执行，仍然失败，错误是 CONNECTION_REFUSED。不要再让我原样做 A。"},
		{"role": "assistant", "content": "改用 B 检查：\n" + strings.Repeat("很长的代码内容\n", 5000) + "\nB_DONE 请提供 B 的结果。"},
		{"role": "user", "content": "B 也失败了，下一步呢？"},
	}
	before, _ := json.Marshal(messages)
	packed, _, usage, err := aiPrepareChatContext(messages, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if usage.HistoryUsed != 4 || usage.HistoryOmitted != 0 || usage.HistoryExcerpted != 1 {
		t.Fatal(usage)
	}
	all, chars := "", 0
	for _, m := range packed {
		s, _ := m["content"].(string)
		all += s
		chars += utf8.RuneCountInString(s)
	}
	for _, want := range []string{"环境是 Windows", "CONNECTION_REFUSED", "改用 B 检查", "B_DONE", "do not recommend an unchanged action", "middle omitted"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q", want)
		}
	}
	if chars > aiTextLimit || strings.Count(all, "B 也失败了，下一步呢？") != 1 {
		t.Fatal("overflow or duplicated current question", chars)
	}
	for i, role := range []string{"system", "user", "assistant", "user", "assistant", "user"} {
		if packed[i]["role"] != role {
			t.Fatal("role order changed", packed)
		}
	}
	after, _ := json.Marshal(messages)
	if !bytes.Equal(before, after) {
		t.Fatal("persisted history modified")
	}
}

func TestAIContextShortTurnsRemainVerbatim(t *testing.T) {
	withTempConfig(t, Config{})
	messages := []map[string]any{{"role": "user", "content": "运行 A"}, {"role": "assistant", "content": "A 的命令"}, {"role": "user", "content": "A 失败了"}}
	packed, _, usage, err := aiPrepareChatContext(messages, nil, nil)
	if err != nil || usage.HistoryUsed != 2 || usage.HistoryExcerpted != 0 || usage.HistoryOmitted != 0 {
		t.Fatal(err, usage)
	}
	for i, m := range messages {
		if packed[i+1]["content"] != m["content"] {
			t.Fatal("short message changed")
		}
	}
}

func TestAIContextBudgetBoundaries(t *testing.T) {
	withTempConfig(t, Config{})
	for _, size := range []int{1, 1000, 8000, 15000, 20000} {
		messages := []map[string]any{}
		for i := 0; i < 20; i++ {
			messages = append(messages, map[string]any{"role": "user", "content": strings.Repeat("问", size)}, map[string]any{"role": "assistant", "content": strings.Repeat("答", size*2)})
		}
		messages = append(messages, map[string]any{"role": "user", "content": "继续排查"})
		packed, _, usage, err := aiPrepareChatContext(messages, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		chars := 0
		for _, m := range packed {
			chars += utf8.RuneCountInString(m["content"].(string))
		}
		if chars > aiTextLimit || usage.HistoryUsed+usage.HistoryOmitted != 40 || usage.HistoryExcerpted > usage.HistoryUsed {
			t.Fatal(size, chars, usage)
		}
		if packed[len(packed)-1]["content"] != "继续排查" {
			t.Fatal("latest question lost")
		}
	}
}
