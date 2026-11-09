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
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgentActionAndFileBoundary(t *testing.T) {
	for _, raw := range []string{"```json\n{\"tool\":\"run_command\",\"command\":\"x\"}\n```", `{"tool":"run_command","unexpected":true}`, `{"tool":"write_file"} {"tool":"finish"}`, `{"tool":"unknown"}`} {
		if _, e := agentParseAction(raw); e == nil {
			t.Fatal("accepted invalid action", raw)
		}
	}
	root := t.TempDir()
	root, _ = agentRoot(root)
	outside := t.TempDir()
	tools := &agentTools{root: root, logDir: t.TempDir()}
	ctx := context.Background()
	for _, p := range []string{"../escape", filepath.Join(outside, "escape"), "file.txt:stream"} {
		if _, e := tools.path(p); e == nil {
			t.Error("escape allowed", p)
		}
	}
	if e := os.Symlink(outside, filepath.Join(root, "link")); e == nil {
		if _, e = tools.path("link/escape"); e == nil {
			t.Error("link escape allowed")
		}
	}
	_, e := tools.execute(ctx, agentAction{Tool: "write_file", Path: "note.txt", Content: "first", SHA256: "new"})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tools.execute(ctx, agentAction{Tool: "write_file", Path: "note.txt", Content: "bad", SHA256: "new"}); e == nil {
		t.Fatal("overwrite without digest")
	}
	_, e = tools.execute(ctx, agentAction{Tool: "write_file", Path: "note.txt", Content: "second", SHA256: agentHash([]byte("first"))})
	if e != nil {
		t.Fatal(e)
	}
	backups, _ := filepath.Glob(filepath.Join(tools.logDir, "backups", "*.bin"))
	if len(backups) != 1 {
		t.Fatal("missing backup")
	}
	raw, _ := os.ReadFile(backups[0])
	if string(raw) != "first" {
		t.Fatal("wrong backup")
	}
	for _, tool := range []string{"run_command", "browser_open"} {
		if _, e = tools.execute(ctx, agentAction{Tool: tool, Command: "Write-Output forbidden", URL: "https://example.com"}); e == nil {
			t.Fatal("disabled tool allowed", tool)
		}
	}
}

func TestAgentPromptKeepsTaskAndNewestResult(t *testing.T) {
	base := "ORIGINAL_TASK_AND_RULES"
	latest := "LATEST_FAILURE: " + strings.Repeat("x", 10000)
	prompt, e := agentPrompt(base, []string{"previous ERROR"}, []string{strings.Repeat("old", 5000), latest}, 16384)
	if e != nil || len(prompt) > 8192 || !strings.Contains(prompt, base) || !strings.Contains(prompt, "LATEST_FAILURE") || !strings.Contains(prompt, "previous ERROR") {
		t.Fatal(len(prompt), e)
	}
	if _, e = agentPrompt(base, nil, nil, 4096); e == nil {
		t.Fatal("small window accepted")
	}
	if _, e = agentPrompt(strings.Repeat("x", 20000), nil, nil, 16384); e == nil {
		t.Fatal("task silently clipped")
	}
}

func TestAgentRunsSupportedModelsAndReceivesFailures(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows executor")
	}
	for _, model := range []struct {
		name, title, engine string
		ctx                 int
	}{
		{"custom-deepseek", "DeepSeek-R1-70B-去审查-Q5", "", 32768},
		{"qwen3.8-27b", "Qwen3.8 27B", "", 32768},
		{"qwen3.8-27b-1m", "Qwen3.8 1M", "llama_cpp_1m", 1000000},
	} {
		t.Run(model.name, func(t *testing.T) {
			withTempConfig(t, Config{})
			root := t.TempDir()
			step := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wbllm/info" {
					json.NewEncoder(w).Encode(map[string]any{"title": model.title, "ready": true, "llm_up": true, "llm": map[string]any{"name": model.name, "ctx": model.ctx, "engine": model.engine}})
					return
				}
				if r.URL.Path != "/wbllm/api/chat" {
					t.Error("unexpected endpoint", r.URL.Path)
					w.WriteHeader(404)
					return
				}
				var body struct {
					Model    string              `json:"model"`
					Messages []map[string]string `json:"messages"`
					Format   string              `json:"format"`
					Options  map[string]any      `json:"options"`
				}
				if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
					t.Error(e)
				}
				if body.Model != model.name || body.Format != "json" || len(body.Messages) != 1 || body.Messages[0]["role"] != "user" || body.Options["num_ctx"] != float64(model.ctx) {
					t.Error("incorrect model protocol", body.Model)
				}
				prompt := body.Messages[0]["content"]
				step++
				var action agentAction
				switch step {
				case 1:
					action = agentAction{Tool: "read_file", Path: "missing.txt"}
				case 2:
					if !strings.Contains(prompt, "RESULT ERROR") || !strings.Contains(prompt, "missing.txt") {
						t.Error("failure not sent back")
					}
					action = agentAction{Tool: "write_file", Path: "result.txt", SHA256: "new", Content: "实际执行成功"}
				case 3:
					if !strings.Contains(prompt, "written sha256=") {
						t.Error("write result missing")
					}
					action = agentAction{Tool: "run_command", Command: "Get-Content -LiteralPath 'result.txt' -Encoding UTF8"}
				case 4:
					if !strings.Contains(prompt, "实际执行成功") || !strings.Contains(prompt, "run_command OK") {
						t.Error("terminal result missing")
					}
					action = agentAction{Tool: "finish", Message: "已写入并通过终端读取确认"}
				default:
					t.Error("unexpected extra model turn")
				}
				raw, _ := json.Marshal(action)
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(raw)}, "done": true})
			}))
			defer srv.Close()
			cfg := getConfig()
			cfg.Mode = "gateway"
			cfg.Proxy = "direct"
			cfg.GateURL = srv.URL
			saveConfig(cfg)
			raw, _ := json.Marshal(map[string]any{"root": root, "task": "创建并检查 result.txt", "approved": true, "terminal": true, "model": model.name})
			w := httptest.NewRecorder()
			agentRun(w, httptest.NewRequest("POST", "/api/ai/agent/run", bytes.NewReader(raw)))
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"kind":"finish"`) {
				t.Fatal(w.Code, w.Body.String())
			}
			if step != 4 {
				t.Fatal(step)
			}
			actual, _ := os.ReadFile(filepath.Join(root, "result.txt"))
			if string(actual) != "实际执行成功" {
				t.Fatal("not executed")
			}
			files, _ := filepath.Glob(filepath.Join(dataDir, "agent-runs", "*", "events.jsonl"))
			if len(files) != 1 {
				t.Fatal("journal missing")
			}
			log, _ := os.ReadFile(files[0])
			if !strings.Contains(string(log), `"kind":"result"`) {
				t.Fatal("no persistent result")
			}
			if agentActive.Load() {
				t.Fatal("agent lock leaked")
			}
		})
	}
}

func TestAgentRejectsUnapprovedAndUnauthorized(t *testing.T) {
	withTempConfig(t, Config{})
	req := httptest.NewRequest("POST", "/api/ai/agent/run", strings.NewReader(`{"approved":false,"task":"x"}`))
	w := httptest.NewRecorder()
	agentRun(w, req)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	newServer().ServeHTTP(w, httptest.NewRequest("POST", "/api/ai/agent/run", strings.NewReader(`{"approved":true}`)))
	if w.Code < 400 {
		t.Fatal("unauthenticated agent endpoint")
	}
	for _, name := range []string{"deepseek-v3", "deepseek-r1-32b-q5", "qwen2.5-70b", "llama-70b"} {
		if agentFamily(name) != "" {
			t.Fatal("unsupported model enabled", name)
		}
	}
}

func TestAgentMalformedAndBrokenStream(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows feature")
	}
	for _, mode := range []string{"malformed", "broken", "length"} {
		t.Run(mode, func(t *testing.T) {
			withTempConfig(t, Config{})
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wbllm/info" {
					fmt.Fprint(w, `{"title":"Qwen3.8","ready":true,"llm_up":true,"llm":{"name":"qwen3.8","ctx":32768}}`)
					return
				}
				calls++
				content := `{"tool":"read_file","path":"absent"}`
				done := true
				reason := "stop"
				if mode == "malformed" {
					content = "```powershell\nWrite-Output nope\n```"
				}
				if mode == "broken" {
					done = false
				}
				if mode == "length" {
					reason = "length"
				}
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": content}, "done": done, "done_reason": reason})
			}))
			defer srv.Close()
			cfg := getConfig()
			cfg.Mode = "gateway"
			cfg.GateURL = srv.URL
			cfg.Proxy = "direct"
			saveConfig(cfg)
			raw, _ := json.Marshal(map[string]any{"root": t.TempDir(), "task": "test", "approved": true})
			w := httptest.NewRecorder()
			agentRun(w, httptest.NewRequest("POST", "/api/ai/agent/run", bytes.NewReader(raw)))
			if !strings.Contains(w.Body.String(), `"kind":"error"`) || strings.Contains(w.Body.String(), `"kind":"finish"`) {
				t.Fatal(w.Body.String())
			}
			want := 1
			if mode == "malformed" {
				want = 3
			}
			if calls != want {
				t.Fatalf("calls %d want %d", calls, want)
			}
		})
	}
}

func TestAgentCommandCancellationAndExitCode(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows shell")
	}
	root := t.TempDir()
	out, e := agentRunCommand(context.Background(), root, "Write-Output 'before'; exit 7")
	if e == nil || !strings.Contains(out, "before") {
		t.Fatal(out, e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, e = agentRunCommand(ctx, root, "Start-Sleep -Seconds 20; Set-Content -LiteralPath 'should-not-exist' -Value 'bad'")
	if e == nil || time.Since(start) > 6*time.Second {
		t.Fatal("command did not stop", e, time.Since(start))
	}
	if _, e = os.Stat(filepath.Join(root, "should-not-exist")); !os.IsNotExist(e) {
		t.Fatal("canceled command continued")
	}
}

func TestAgentPauseResumeStopAndSingleRun(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows feature")
	}
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			withTempConfig(t, Config{})
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/wbllm/info" {
					fmt.Fprint(w, `{"title":"Qwen3.8","ready":true,"llm_up":true,"llm":{"name":"qwen3.8","ctx":32768}}`)
					return
				}
				calls++
				action := agentAction{Tool: "ask_user", Message: "请确认测试目标"}
				if calls == 2 {
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					raw, _ := json.Marshal(body)
					if !strings.Contains(string(raw), "CONFIRMED_TARGET") {
						t.Error("clarification not passed")
					}
					action = agentAction{Tool: "finish", Message: "目标已确认"}
				}
				raw, _ := json.Marshal(action)
				json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(raw)}, "done": true})
			}))
			defer srv.Close()
			cfg := getConfig()
			cfg.Mode = "gateway"
			cfg.Proxy = "direct"
			cfg.GateURL = srv.URL
			saveConfig(cfg)
			raw, _ := json.Marshal(map[string]any{"root": t.TempDir(), "task": "test", "approved": true})
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				agentRun(w, httptest.NewRequest("POST", "/api/ai/agent/run", bytes.NewReader(raw)))
			}()
			deadline := time.Now().Add(4 * time.Second)
			id := ""
			for time.Now().Before(deadline) {
				agentControl.Lock()
				if agentControl.awaiting {
					id = agentControl.id
				}
				agentControl.Unlock()
				if id != "" {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if id == "" {
				t.Fatal("task did not pause")
			}
			other := httptest.NewRecorder()
			agentRun(other, httptest.NewRequest("POST", "/api/ai/agent/run", bytes.NewReader(raw)))
			if other.Code != 409 {
				t.Fatal("concurrent task allowed", other.Code)
			}
			wrong := httptest.NewRecorder()
			agentAnswer(wrong, httptest.NewRequest("POST", "/api/ai/agent/answer", strings.NewReader(`{"id":"wrong","text":"bad"}`)))
			if wrong.Code != 409 {
				t.Fatal("wrong task accepted")
			}
			answer, _ := json.Marshal(map[string]string{"id": id, "text": "CONFIRMED_TARGET"})
			control := httptest.NewRecorder()
			if stop {
				agentStop(control, httptest.NewRequest("POST", "/api/ai/agent/stop", bytes.NewReader(answer)))
			} else {
				agentAnswer(control, httptest.NewRequest("POST", "/api/ai/agent/answer", bytes.NewReader(answer)))
			}
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("task did not finish/stop")
			}
			kind := "finish"
			if stop {
				kind = "stopped"
			}
			if !strings.Contains(w.Body.String(), `"kind":"`+kind+`"`) {
				t.Fatal(w.Body.String())
			}
		})
	}
}
