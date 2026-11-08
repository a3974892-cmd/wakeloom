package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAIChatStatePersistsAndMergesPartialUpdates(t *testing.T) {
	withTempConfig(t, Config{})
	call := func(method, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/api/ai/chat-state/test-model", strings.NewReader(body))
		r.SetPathValue("profile", "test-model")
		w := httptest.NewRecorder()
		aiChatStateHandler(w, r)
		return w
	}
	w := call("PUT", `{"conversations":[{"id":"old","msgs":[{"role":"user","content":"保留旧内容"}]}],"current":"old"}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("PUT", `{"skills":["skill-a"]}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = call("GET", "")
	var state aiChatState
	if json.Unmarshal(w.Body.Bytes(), &state) != nil {
		t.Fatal(w.Body.String())
	}
	if !state.Exists || state.Current != "old" || !strings.Contains(string(state.Conversations), "保留旧内容") || len(state.Skills) != 1 {
		t.Fatalf("partial update lost data: %+v", state)
	}
	// Corrupt data must not be overwritten by an empty fallback from the page.
	p, _ := aiChatStatePath("test-model")
	os.WriteFile(p, []byte("broken existing history"), 0600)
	if w = call("GET", ""); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if w = call("PUT", `{"conversations":[]}`); w.Code != 500 {
		t.Fatal("corrupt history overwritten")
	}
	raw, _ := os.ReadFile(p)
	if string(raw) != "broken existing history" {
		t.Fatal("original history lost")
	}
	w = httptest.NewRecorder()
	newServer().ServeHTTP(w, httptest.NewRequest("GET", "/api/ai/chat-state/test-model", nil))
	if w.Code == 200 {
		t.Fatal("history exposed without local key")
	}
}
