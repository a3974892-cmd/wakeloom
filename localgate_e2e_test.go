package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Full local-mode boot against a mock Colab API + a real Jupyter server (root_dir=/) acting as the VM.
// Needs JUPYTER_ROOT_URL and the fake driver at /tmp/wbtest/fake_driver.py; upstream "ComfyUI" on :8188.
func TestLocalGateE2E(t *testing.T) {
	ju := os.Getenv("JUPYTER_ROOT_URL")
	if ju == "" {
		t.Skip("JUPYTER_ROOT_URL not set")
	}
	var unassigned int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at" {
			w.WriteHeader(401)
			return
		}
		io.WriteString(w, ")]}'\n")
		switch {
		case strings.HasPrefix(r.URL.Path, "/tun/m/assign") && r.Method == "GET":
			io.WriteString(w, `{"acc":"G4","nbh":"x","token":"xsrf","variant":"GPU"}`)
		case strings.HasPrefix(r.URL.Path, "/tun/m/assign") && r.Method == "POST":
			if r.Header.Get("X-Goog-Colab-Token") != "xsrf" || r.URL.Query().Get("accelerator") != "G4" {
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, `{"accelerator":"G4","endpoint":"ep1","variant":1,"runtimeProxyInfo":{"token":"pt","tokenExpiresInSeconds":3600,"url":%q}}`, ju)
		case r.URL.Path == "/tun/m/assignments":
			if atomic.LoadInt32(&unassigned) > 0 {
				io.WriteString(w, `{"assignments":[]}`)
			} else {
				fmt.Fprintf(w, `{"assignments":[{"accelerator":"G4","endpoint":"ep1","variant":1,"machineShape":0,"runtimeProxyInfo":{"token":"pt2","tokenExpiresInSeconds":3600,"url":%q}}]}`, ju)
			}
		case strings.HasPrefix(r.URL.Path, "/tun/m/unassign/ep1"):
			if r.Method == "GET" {
				io.WriteString(w, `{"token":"u"}`)
			} else {
				atomic.AddInt32(&unassigned, 1)
			}
		case r.URL.Path == "/tun/m/ccu-info":
			io.WriteString(w, `{"currentBalance":8.65,"consumptionRateHourly":10,"assignmentsCount":1}`)
		}
	}))
	defer mock.Close()
	colabDomain = mock.URL
	bootstrapDriver = "/tmp/wbtest/fake_driver.py"
	dataDir = t.TempDir()
	os.MkdirAll(dataDir+"/profiles", 0o755)
	gTok = &gToken{AccessToken: "at", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour), Email: "me@x"}
	cfg = Config{Mode: "local", LocalProfile: "minimax-h3", JobTimeoutMin: 30, DraftScale: 0.5}
	startLocalGate()
	libSave([]LibItem{{ID: "seedvr2", Kind: "node", Title: "SeedVR2", URL: "https://github.com/numz/ComfyUI-SeedVR2_VideoUpscaler"},
		{ID: "lora-x", Kind: "model", Title: "lora", URL: "hf://a/b/x.safetensors", Dest: "loras/x.safetensors", SizeGB: 1, AutoWake: true}})

	var st map[string]any
	if err := gateJSON("POST", "/_gate/wake", map[string]any{}, &st); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, _ = gateStatus()
		if st["state"] == "ready" || st["state"] == "failed" {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if st["state"] != "ready" {
		lg.mu.Lock()
		t.Fatalf("state %v reason %v logs %v", st["state"], st["reason"], lg.logs)
	}
	if st["accelerator"] != "G4" {
		t.Fatalf("accel %v", st["accelerator"])
	}
	lg.mu.Lock()
	logs := strings.Join(lg.logs, "\n")
	lg.mu.Unlock()
	if !strings.Contains(logs, `boot extras ["lora-x"]`) {
		t.Fatalf("auto_wake extra not merged: %s", logs)
	}
	// proxy to "ComfyUI" through the auth proxy
	var x map[string]any
	if err := gateJSON("GET", "/x?a=1", nil, &x); err != nil || x["ok"] != true {
		t.Fatalf("proxy: %v %v", x, err)
	}
	// unauthenticated access is refused
	resp, _ := http.Get(lg.baseURL() + "/_gate/status")
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	// on-demand install
	if err := gateJSON("POST", "/_gate/library/install", map[string]any{"ids": []string{"seedvr2"}}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		var l map[string]any
		gateJSON("GET", "/_gate/library", nil, &l)
		b, _ := json.Marshal(l["installed"])
		if strings.Contains(string(b), "seedvr2") && len(l["fetching"].([]any)) == 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
		if i == 99 {
			t.Fatalf("install did not finish: %v", l)
		}
	}
	// heartbeat path
	lg.mu.Lock()
	rt := lg.rt
	lg.mu.Unlock()
	var out []string
	rt.Exec(t.Context(), "print(open('/content/kit_tunnel_url').read())", func(l string) { out = append(out, l) })
	if len(out) == 0 || out[0] != "http://127.0.0.1:8189" {
		t.Fatalf("tunnel file %v", out)
	}
	// stop releases the VM
	gateJSON("POST", "/_gate/stop", nil, nil)
	for i := 0; i < 50 && atomic.LoadInt32(&unassigned) == 0; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if atomic.LoadInt32(&unassigned) == 0 {
		t.Fatal("not unassigned")
	}
	st, _ = gateStatus()
	if st["state"] != "sleeping" {
		t.Fatalf("after stop: %v", st["state"])
	}
}
