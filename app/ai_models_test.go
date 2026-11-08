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

func TestImageModelsUseRunningLoaderLists(t *testing.T) {
	for _, tc := range []struct {
		name    string
		names   []string
		status  int
		wantErr bool
	}{
		{"present", []string{"nested/Chroma.safetensors"}, 200, false},
		{"empty index", []string{}, 200, true},
		{"different model", []string{"Other.safetensors"}, 200, true},
		{"disconnected", nil, 503, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/object_info/UNETLoader" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(tc.status)
				json.NewEncoder(w).Encode(map[string]any{"UNETLoader": map[string]any{"input": map[string]any{"required": map[string]any{"unet_name": []any{tc.names}}}}})
			}))
			defer srv.Close()
			withTempConfig(t, Config{Mode: "gateway", GateURL: srv.URL, Proxy: "direct"})
			old := httpc
			httpc = srv.Client()
			defer func() { httpc = old }()
			graph := map[string]map[string]any{"1": {"class_type": "UNETLoader", "inputs": map[string]any{"unet_name": "nested/Chroma.safetensors"}}}
			err := checkImageModels(graph)
			if (err != nil) != tc.wantErr {
				t.Fatalf("got %v", err)
			}
			if tc.name == "empty index" && !strings.Contains(err.Error(), "可用模型 0 个") {
				t.Fatal(err)
			}
			if tc.name == "different model" && !strings.Contains(err.Error(), "当前可用：Other.safetensors") {
				t.Fatal(err)
			}
		})
	}
}

func TestAIModelSummaryPreservesSubfolders(t *testing.T) {
	p := localProfiles()["chroma1-hd"]
	for i := range p.Models {
		p.Models[i].Dest = strings.Replace(p.Models[i].Dest, "/", "/nested/", 1)
	}
	im := p.aiSummary()["image"].(map[string]any)
	for _, key := range []string{"unet_name", "clip_name", "vae_name"} {
		if !strings.HasPrefix(im[key].(string), "nested/") {
			t.Fatalf("lost subfolder: %v", im)
		}
	}
}

func TestAIImageTemplateKeepsProfile(t *testing.T) {
	withTempConfig(t, Config{})
	if err := os.MkdirAll(filepath.Join(dataDir, "templates"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	aiImageTemplateHandler(w, httptest.NewRequest("POST", "/api/ai/image-template", strings.NewReader(`{"profile":"chroma1-hd","arch":"chroma","unet_name":"u","clip_name":"c","vae_name":"v"}`)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	saved, err := loadTemplate("ai-chroma1-hd")
	if err != nil || saved.Profile != "chroma1-hd" {
		t.Fatalf("profile not bound: %v %v", saved, err)
	}
}
