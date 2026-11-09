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

func TestAgentFailureRecoveryAndHumanResume(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows feature")
	}
	for _, family := range []string{"Qwen3.8", "DeepSeek-R1-70B-Q5"} {
		for _, mode := range []string{"repeat", "different_failures", "stop", "alternative", "abandon"} {
			t.Run(family+"/"+mode, func(t *testing.T) {
				withTempConfig(t, Config{})
				root := t.TempDir()
				if err := os.WriteFile(filepath.Join(root, "actual.txt"), []byte("VERIFIED_CONTENT"), 0600); err != nil {
					t.Fatal(err)
				}
				calls := 0
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/wbllm/info" {
						json.NewEncoder(w).Encode(map[string]any{"title": family, "ready": true, "llm_up": true, "llm": map[string]any{"name": family, "ctx": 32768}})
						return
					}
					var body struct {
						Messages []struct {
							Content string `json:"content"`
						} `json:"messages"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
						return
					}
					calls++
					prompt := body.Messages[0].Content
					if calls > 1 && !strings.Contains(prompt, "RECOVERY REQUIRED") {
						t.Error("no recovery guidance")
					}
					a := agentAction{Tool: "read_file", Path: "absent.txt"}
					// Changing explanatory prose must not bypass repeated-action protection.
					a.Message = fmt.Sprintf("attempt %d", calls)
					if mode == "different_failures" {
						a.Path = fmt.Sprintf("absent-%d.txt", calls)
					}
					resumeAt := 4
					if mode == "abandon" {
						resumeAt = 3
						if calls == 2 {
							a = agentAction{Tool: "finish", Message: "无法执行，请手动处理"}
						}
					}
					if mode == "alternative" {
						if calls == 2 {
							a = agentAction{Tool: "list_files", Path: "."}
						}
						if calls == 3 {
							a = agentAction{Tool: "read_file", Path: "actual.txt"}
						}
						if calls == 4 {
							if !strings.Contains(prompt, "VERIFIED_CONTENT") {
								t.Error("alternative did not receive result")
							}
							a = agentAction{Tool: "finish", Message: "已用实际文件完成检查"}
						}
					} else if calls >= resumeAt {
						if !strings.Contains(prompt, "ACTUAL USER CLARIFICATION: USE_ACTUAL") || !strings.Contains(prompt, "ORIGINAL_GOAL") || !strings.Contains(prompt, "RESULT ERROR") {
							t.Error("resume lost user answer, goal or error")
						}
						a = agentAction{Tool: "read_file", Path: "actual.txt"}
						if calls == resumeAt+1 {
							a = agentAction{Tool: "finish", Message: "已按补充信息读取文件"}
						}
					}
					if calls > 5 {
						t.Error("unbounded recovery")
						a = agentAction{Tool: "finish", Message: "test guard"}
					}
					raw, _ := json.Marshal(a)
					json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": string(raw)}, "done": true})
				}))
				defer srv.Close()
				cfg := getConfig()
				cfg.Mode = "gateway"
				cfg.Proxy = "direct"
				cfg.GateURL = srv.URL
				saveConfig(cfg)
				raw, _ := json.Marshal(map[string]any{"root": root, "task": "ORIGINAL_GOAL", "approved": true})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				w := httptest.NewRecorder()
				done := make(chan struct{})
				go func() {
					defer close(done)
					agentRun(w, httptest.NewRequest("POST", "/api/ai/agent/run", bytes.NewReader(raw)).WithContext(ctx))
				}()
				if mode != "alternative" {
					id := ""
					deadline := time.Now().Add(4 * time.Second)
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
						cancel()
						<-done
						t.Fatal("supervisor did not request help")
					}
					control := httptest.NewRecorder()
					answer, _ := json.Marshal(map[string]string{"id": id, "text": "USE_ACTUAL actual.txt"})
					if mode == "stop" {
						agentStop(control, httptest.NewRequest("POST", "/api/ai/agent/stop", bytes.NewReader(answer)))
					} else {
						agentAnswer(control, httptest.NewRequest("POST", "/api/ai/agent/answer", bytes.NewReader(answer)))
					}
					if control.Code != 200 {
						cancel()
						<-done
						t.Fatal(control.Code)
					}
				}
				select {
				case <-done:
				case <-time.After(4 * time.Second):
					cancel()
					<-done
					t.Fatal("recovery did not finish")
				}
				log := w.Body.String()
				if !strings.Contains(log, `"kind":"recovery"`) {
					t.Fatal(log)
				}
				if mode == "stop" {
					if !strings.Contains(log, `"kind":"stopped"`) || calls != 3 {
						t.Fatal(log)
					}
				} else if !strings.Contains(log, `"kind":"finish"`) {
					t.Fatal(log)
				}
				if mode == "repeat" || mode == "stop" {
					// One execution, then two blocked proposals. The failed action never runs twice.
					n := 0
					for _, line := range strings.Split(log, "\n") {
						if strings.Contains(line, `"kind":"action"`) && strings.Contains(line, "absent.txt") {
							n++
						}
					}
					if n != 1 {
						t.Fatalf("repeated failed tool executed %d times", n)
					}
				}
				if mode == "alternative" && strings.Contains(log, `"kind":"ask_user"`) {
					t.Fatal("successful alternative incorrectly paused")
				}
			})
		}
	}
}

func TestAgentPowerShellFailureAdvice(t *testing.T) {
	if !strings.Contains(agentRecoveryAdvice("ParserError InvalidEndOfLine"), "PowerShell 5.1") {
		t.Fatal("missing syntax advice")
	}
	if !strings.Contains(agentRecoveryAdvice("file does not exist"), "占位符") {
		t.Fatal("missing path advice")
	}
}

func TestAgentFailureKey(t *testing.T) {
	a := agentAction{Tool: "run_command", Command: "  Get-Item missing  ", Message: "try one"}
	b := agentAction{Tool: "run_command", Command: "Get-Item missing", Message: "try again", Path: "ignored"}
	if agentFailureKey(a) != agentFailureKey(b) {
		t.Fatal("irrelevant prose or whitespace bypassed repeat detection")
	}
	b.Command = "Get-Item actual"
	if agentFailureKey(a) == agentFailureKey(b) {
		t.Fatal("changed action must be allowed")
	}
}
