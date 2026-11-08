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
		if total > aiTextLimit || !strings.Contains(all, "FOLDER_EVIDENCE") || strings.Contains(all, "old long code") {
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
	if usage.HistoryOmitted != 2 || usage.HistoryUsed != 2 || len(skills) != 1 || len(skills[0].ExcerptedReferences) != 1 {
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
