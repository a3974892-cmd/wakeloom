package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAIVisionInstallerOnlyRunsForReadyLocalSession(t *testing.T) {
	withTempConfig(t, Config{Mode: "gateway"})
	w := httptest.NewRecorder()
	aiVisionEnable(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 409 {
		t.Fatal("gateway accepted local installer")
	}
	cfg := getConfig()
	cfg.Mode = "local"
	saveConfig(cfg)
	oldGate, oldRunner := lg, aiVisionExecute
	defer func() {
		lg = oldGate
		aiVisionExecute = oldRunner
		aiVisionJob.Lock()
		aiVisionJob.state = aiVisionJobState{}
		aiVisionJob.Unlock()
	}()
	lg = &LocalGate{state: "sleeping"}
	w = httptest.NewRecorder()
	aiVisionEnable(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 409 {
		t.Fatal("sleeping session accepted")
	}
	lg = &LocalGate{state: "ready", profile: "chroma1-hd", rt: &Runtime{}}
	w = httptest.NewRecorder()
	aiVisionEnable(w, httptest.NewRequest("POST", "/", nil))
	if w.Code != 400 {
		t.Fatal("unsupported model accepted")
	}
	for _, fail := range []bool{false, true} {
		lg = &LocalGate{state: "ready", profile: "qwen38-27b-chat", rt: &Runtime{}}
		aiVisionJob.Lock()
		aiVisionJob.state = aiVisionJobState{}
		aiVisionJob.Unlock()
		release := make(chan struct{})
		calls := make(chan bool, 1)
		aiVisionExecute = func(_ context.Context, _ *Runtime, onLine func(string)) error {
			calls <- true
			onLine(`WB_VISION {"phase":"download","text":"fixture"}`)
			<-release
			if fail {
				return errors.New("fixture failure")
			}
			onLine(`WB_VISION {"phase":"ready","text":"image inference checked"}`)
			return nil
		}
		w = httptest.NewRecorder()
		aiVisionEnable(w, httptest.NewRequest("POST", "/", nil))
		if w.Code != 202 {
			t.Fatal(w.Code, w.Body.String())
		}
		select {
		case <-calls:
		case <-time.After(time.Second):
			t.Fatal("job not started")
		}
		w = httptest.NewRecorder()
		aiVisionEnable(w, httptest.NewRequest("POST", "/", nil))
		if w.Code != 202 || !aiVisionInstalling() {
			t.Fatal("duplicate job not coalesced")
		}
		close(release)
		deadline := time.Now().Add(time.Second)
		for aiVisionInstalling() && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		aiVisionJob.Lock()
		state := aiVisionJob.state
		aiVisionJob.Unlock()
		if state.Done == fail || state.Running || fail && (state.Error == "") {
			t.Fatal(state)
		}
	}
}

func TestAIVisionUpdateDownloadContainsOnlyPublicComponents(t *testing.T) {
	w := httptest.NewRecorder()
	aiVisionDownload(w, httptest.NewRequest("GET", "/", nil))
	z, e := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if e != nil || len(z.File) != 4 {
		t.Fatal(e)
	}
	names := map[string]bool{}
	for _, f := range z.File {
		names[f.Name] = true
	}
	for _, name := range []string{"gate-ai.py", "enable-qwen-vision.py", "enable-vision-on-vps.py", "使用说明.txt"} {
		if !names[name] {
			t.Fatal(name)
		}
	}
	withTempConfig(t, Config{})
	server := newServer()
	w = httptest.NewRecorder()
	server.ServeHTTP(w, httptest.NewRequest("GET", "/api/ai/vision/update", nil))
	if w.Code != 403 {
		t.Fatal("private API key not required", w.Code)
	}
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/ai/vision/status", nil)
	aiVisionStatus(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if !strings.Contains(w.Body.String(), "can_install") {
		t.Fatal(out)
	}
}
