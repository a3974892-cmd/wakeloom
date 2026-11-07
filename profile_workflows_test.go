package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinWorkflowDownload(t *testing.T) {
	withTempConfig(t, Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/profile-workflows/{profile}/{workflow}", profileWorkflowFile)
	for _, tc := range []struct {
		profile, workflow string
		status            int
	}{
		{"qwen-image", "qwen21-turbo-t2i", 200},
		{"qwen-image", "qwen21-turbo-edit", 200},
		{"qwen-image", "not-found", 404},
		{"no-profile", "qwen21-turbo-t2i", 404},
		{"custom-private", "qwen21-turbo-t2i", 404},
	} {
		t.Run(tc.profile+tc.workflow, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/profile-workflows/"+tc.profile+"/"+tc.workflow, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d", w.Code)
			}
			if w.Code == 200 {
				if !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
					t.Fatal("not a download")
				}
				var graph map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &graph); err != nil || graph["nodes"] == nil {
					t.Fatalf("not an original UI workflow: %v", err)
				}
			}
		})
	}
}

func TestQwenWorkflowSummaryExposesDownloads(t *testing.T) {
	withTempConfig(t, Config{})
	for _, p := range localProfilesSummary()["profiles"].([]map[string]any) {
		if p["id"] != "qwen-image" {
			continue
		}
		if p["ai"] != nil {
			t.Fatal("Qwen must keep its own workflow, not an SDXL/Chroma graph")
		}
		ws := p["workflows"].([]map[string]any)
		if len(ws) != 4 || p["app_default"] != "qwen21-turbo-t2i" {
			t.Fatal("missing Qwen workflows")
		}
		for _, w := range ws {
			if w["embedded"] != true {
				t.Fatalf("missing workflow file: %v", w)
			}
		}
		return
	}
	t.Fatal("Qwen profile missing")
}

func TestJobRejectsDifferentRunningProfile(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/_gate/status" {
			t.Errorf("wrong-profile task reached %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"state":"ready","profile":"qwen-image"}`))
	}))
	defer srv.Close()
	withTempConfig(t, Config{Mode: "gateway", GateURL: srv.URL, Proxy: "direct"})
	if err := os.MkdirAll(filepath.Join(dataDir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := saveTemplate(&Template{ID: "ai-chroma1-hd", Profile: "chroma1-hd"}); err != nil {
		t.Fatal(err)
	}
	oldStore, oldHTTP := store, httpc
	store = &JobStore{kick: make(chan struct{}, 1)}
	httpc = srv.Client()
	defer func() { store = oldStore; httpc = oldHTTP }()
	j := &Job{TemplateID: "ai-chroma1-hd", Status: "queued"}
	store.jobs = []*Job{j}
	if submit(j) || j.Status != "failed" || !strings.Contains(j.Message, "qwen-image") || !strings.Contains(j.Message, "chroma1-hd") {
		t.Fatalf("unexpected job result: %+v", j)
	}
}
