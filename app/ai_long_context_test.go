package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAILongBuiltinProfile(t *testing.T) {
	profiles := localProfiles()
	p := profiles["qwen38-27b-1m"]
	if p == nil {
		t.Fatal("1M profile not embedded")
	}
	llm := p.aiSummary()["llm"].(map[string]any)
	if llm["ctx"] != aiLongContext || llm["engine"] != "llama_cpp_1m" || llm["name"] != "qwen3.8-27b-1m" {
		t.Fatal(llm)
	}
	raw, err := fs.ReadFile(kitFS, "kit/profiles/qwen38-27b-1m/profile.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = validateProfileYAML(raw); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"qwen38-27b-chat", "qwen38-27b-vision"} {
		old := profiles[id].aiSummary()["llm"].(map[string]any)
		if old["ctx"] != 32768 || old["engine"] != nil {
			t.Fatal("normal profile changed", id, old)
		}
	}
}

// Exercise the actual HTTP handler: verified capacity, tokenization, packing and
// inference must agree. No GPU/network service is used by these loopback tests.
func TestAILongContextRequest(t *testing.T) {
	for _, scenario := range []string{"large", "repack", "unverified", "wrong-window", "token-error", "images", "normal"} {
		t.Run(scenario, func(t *testing.T) {
			withTempConfig(t, Config{})
			checks, chats := 0, 0
			previousSize := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/wbllm/info":
					fmt.Fprintf(w, `{"ready":true,"context_verified":%t,"context_tokens":1000000,"llm":{"name":"long","engine":"llama_cpp_1m"}}`, scenario != "unverified")
				case "/wbllm/api/context-check":
					checks++
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["options"].(map[string]any)["num_ctx"] != float64(aiLongContext) {
						t.Error("missing 1M request")
					}
					packed, _ := json.Marshal(body["messages"])
					count := 200000
					if scenario == "repack" && checks == 1 {
						count = 1900000
						previousSize = len(packed)
					}
					if scenario == "repack" && checks > 1 && len(packed) >= previousSize {
						t.Error("over-budget history was not repacked")
					}
					if scenario == "token-error" {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":"tokenizer unavailable"}`)
						return
					}
					window := aiLongContext
					if scenario == "wrong-window" {
						window = 32768
					}
					fmt.Fprintf(w, `{"tokens":%d,"input_budget":979520,"context_tokens":%d}`, count, window)
				case "/wbllm/api/chat":
					chats++
					var body struct {
						Messages []map[string]any `json:"messages"`
						Options  map[string]any   `json:"options"`
					}
					json.NewDecoder(r.Body).Decode(&body)
					all := ""
					for _, m := range body.Messages {
						all += m["content"].(string)
					}
					if !strings.Contains(all, "A_ALREADY_FAILED") || !strings.Contains(all, "LATEST_QUESTION") {
						t.Error("lost failure feedback/current question")
					}
					if scenario == "normal" {
						if utf8.RuneCountInString(all) > aiTextLimit {
							t.Error("normal limit changed")
						}
					} else {
						if len(all) <= aiTextLimit || body.Options["num_ctx"] != float64(aiLongContext) {
							t.Error("long history still capped")
						}
						if scenario == "large" && !strings.Contains(all, "ATTACHMENT_END") {
							t.Error("long document did not reach model")
						}
					}
					fmt.Fprintln(w, `{"message":{"content":"ok"},"done":true}`)
				default:
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			cfg := getConfig()
			cfg.Mode = "gateway"
			cfg.GateURL = srv.URL
			cfg.Proxy = "direct"
			saveConfig(cfg)
			repeat := 12000
			if scenario == "repack" {
				repeat = 600000
			}
			messages := []map[string]any{
				{"role": "user", "content": "A_ALREADY_FAILED"},
				{"role": "assistant", "content": strings.Repeat("abcdef ", repeat)},
				{"role": "user", "content": "LATEST_QUESTION"},
			}
			if scenario == "large" {
				a, w := aiTestUpload(t, map[string][]byte{"document.txt": []byte(strings.Repeat("document text ", 30000) + " ATTACHMENT_END")})
				if w.Code != 200 {
					t.Fatal(w.Body.String())
				}
				messages[2]["attachments"] = []map[string]string{{"id": a.ID}}
			}
			if scenario == "images" {
				messages[2]["images"] = []string{"aW1hZ2U="}
			}
			before, _ := json.Marshal(messages)
			ctx := aiLongContext
			if scenario == "normal" {
				ctx = 32768
			}
			raw, _ := json.Marshal(map[string]any{"model": "long", "messages": messages, "num_ctx": ctx, "think": false})
			w := httptest.NewRecorder()
			aiChatHandler(w, httptest.NewRequest("POST", "/api/ai/chat", bytes.NewReader(raw)))
			success := scenario == "large" || scenario == "repack" || scenario == "normal"
			if success {
				if w.Code != 200 || chats != 1 {
					t.Fatalf("status=%d chats=%d: %s", w.Code, chats, w.Body.String())
				}
				if scenario != "normal" && !strings.Contains(w.Body.String(), `"context_tokens":1000000`) {
					t.Error("missing capacity disclosure", w.Body.String())
				}
			} else if w.Code == 200 || chats != 0 {
				t.Errorf("unsafe inference: status=%d chats=%d", w.Code, chats)
			}
			if scenario == "normal" && checks != 0 {
				t.Error("normal model used long tokenizer")
			}
			if scenario == "repack" && checks != 2 {
				t.Error("repack checks", checks)
			}
			after, _ := json.Marshal(messages)
			if !bytes.Equal(before, after) {
				t.Fatal("mutated original history")
			}
		})
	}
}

func TestAILongChatPersistenceAboveEightMB(t *testing.T) {
	withTempConfig(t, Config{})
	text := strings.Repeat("old conversation ", 600000)
	raw, _ := json.Marshal(map[string]any{"conversations": []any{map[string]any{"id": "long", "msgs": []any{map[string]string{"role": "user", "content": text}}}}, "current": "long"})
	if len(raw) <= 8<<20 {
		t.Fatal("fixture too small")
	}
	r := httptest.NewRequest("PUT", "/api/ai/chat-state/long", bytes.NewReader(raw))
	r.SetPathValue("profile", "long")
	w := httptest.NewRecorder()
	aiChatStateHandler(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	p, _ := aiChatStatePath("long")
	state, err := readAIChatState(p)
	if err != nil || !state.Exists || !strings.Contains(string(state.Conversations), text) {
		t.Fatal("large history lost", err)
	}
}
