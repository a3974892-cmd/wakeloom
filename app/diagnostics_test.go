package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func diagnosticTestStore(t *testing.T, limit int64) *diagnosticStore {
	t.Helper()
	old := diagnostics.Load()
	d := &diagnosticStore{dir: t.TempDir(), maxBytes: limit}
	diagnostics.Store(d)
	t.Cleanup(func() { diagnostics.Store(old) })
	return d
}
func TestDiagnosticRedactionBeforePersistenceAndExport(t *testing.T) {
	d := diagnosticTestStore(t, 2*1024*1024)
	rememberDiagnosticSecrets("my-private-key-123456", "query+secret/789")
	for _, v := range []string{"Authorization: Bearer fresh-unknown-secret", "Cookie: first=private-one; second=private-two", `"authorization":"Bearer private-three"`} {
		redacted := redactDiagnostic(v)
		if strings.Contains(redacted, "private-") || strings.Contains(redacted, "fresh-unknown") {
			t.Fatalf("header leak: %s", redacted)
		}
	}
	text := `HTTP 503 bootstrap failed user@example.com C:\Users\Alice\models\x.gguf https://alice:password@api.example.com/download?token=opaque&X-Amz-Signature=secret Bearer bearer-private-123 api_key="my-private-key-123456" hf_abcdefgh12345678 /home/alice/project ya29.oauth-secret query%2Bsecret%2F789`
	d.add("error", "local", text)
	raw, err := os.ReadFile(filepath.Join(d.dir, "diagnostics.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"user@example.com", "Alice", "/home/alice", "bearer-private", "my-private-key", "hf_abcdef", "oauth-secret", "X-Amz", "alice:password", "query%2Bsecret"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("secret persisted: %s", secret)
		}
	}
	rows, warn := d.snapshot()
	if warn != "" || len(rows) != 1 {
		t.Fatalf("snapshot: %v %s", rows, warn)
	}
	if !strings.Contains(rows[0].Message, "HTTP 503 bootstrap failed") || !strings.Contains(rows[0].Message, "x.gguf") {
		t.Fatal("useful diagnostics were lost")
	}
	// Secret replacement must happen before a long line is truncated.
	d.add("error", "api", strings.Repeat("x", 15998)+"my-private-key-123456"+strings.Repeat("y", 80000))
	raw, _ = os.ReadFile(filepath.Join(d.dir, "diagnostics.jsonl"))
	if strings.Contains(string(raw), "my-private") {
		t.Fatal("partial secret after truncation")
	}
}
func TestDiagnosticRotationRestartAndConcurrentWrites(t *testing.T) {
	d := diagnosticTestStore(t, 900)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d.add("info", "test", fmt.Sprintf("line-%02d %s", i, strings.Repeat("x", 160)))
		}(i)
	}
	wg.Wait()
	d.add("error", "local", "final bootstrap failure")
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("expected bounded rotation, got %d files", len(entries))
	}
	for _, e := range entries {
		st, _ := e.Info()
		if st.Size() > 900 {
			t.Fatalf("oversized log: %d", st.Size())
		}
	}
	restarted := &diagnosticStore{dir: d.dir, maxBytes: 900}
	rows, warn := restarted.snapshot()
	if warn != "" || len(rows) == 0 || rows[len(rows)-1].Message != "final bootstrap failure" {
		t.Fatalf("restart lost latest log: %v %s", rows, warn)
	}
}
func TestDiagnosticRoutesRequireKeyAndDoNotIncludeRequestBodies(t *testing.T) {
	d := diagnosticTestStore(t, 1<<20)
	srv := newServer()
	denied := httptest.NewRecorder()
	srv.ServeHTTP(denied, httptest.NewRequest("GET", "/api/diagnostics", nil))
	if denied.Code != 403 {
		t.Fatal("unauthenticated diagnostics access")
	}
	request := httptest.NewRequest("POST", "/api/diagnostics/client", strings.NewReader(`{"message":"UI failure with sk-privatekey123456789", "stack":"at renderStatus (http://127.0.0.1:88/app.js?k=private)", "config":"must-not-store-this"}`))
	request.Header.Set("X-Key", apiKey)
	response := httptest.NewRecorder()
	srv.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	req := httptest.NewRequest("GET", "/api/diagnostics", nil)
	req.Header.Set("X-Key", apiKey)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("response %d", w.Code)
	}
	var payload struct {
		Report  string            `json:"report"`
		Entries []diagnosticEntry `json:"entries"`
	}
	if json.Unmarshal(w.Body.Bytes(), &payload) != nil {
		t.Fatal("invalid JSON")
	}
	if len(payload.Entries) != 1 || !strings.Contains(payload.Report, "UI failure") {
		t.Fatal("missing event")
	}
	for _, s := range []string{"sk-privatekey", "must-not-store-this", "?k=private"} {
		if strings.Contains(payload.Report, s) {
			t.Fatalf("report leaked %s", s)
		}
	}
	oversized := httptest.NewRequest("POST", "/api/diagnostics/client", strings.NewReader(`{"message":"`+strings.Repeat("x", 17000)+`"}`))
	oversized.Header.Set("X-Key", apiKey)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, oversized)
	if w.Code != 400 {
		t.Fatal("oversized event accepted")
	}
	rows, _ := d.snapshot()
	if len(rows) != 1 {
		t.Fatal("diagnostic reads recursively generated events")
	}
}
func TestDiagnosticHTTPPreservesStreamingAndRecordsFailure(t *testing.T) {
	d := diagnosticTestStore(t, 1<<20)
	stream := diagnosticHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: log\ndata: hello\n\n")
		w.(http.Flusher).Flush()
	}))
	w := httptest.NewRecorder()
	stream.ServeHTTP(w, httptest.NewRequest("GET", "/api/gate/events?k=never-log", nil))
	if !w.Flushed || w.Code != 200 || !strings.Contains(w.Body.String(), "data: hello") {
		t.Fatal("streaming changed")
	}
	failure := diagnosticHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 502, map[string]string{"error": "download failed HTTP 504"})
	}))
	w = httptest.NewRecorder()
	failure.ServeHTTP(w, httptest.NewRequest("POST", "/api/gate/wake?token=never-log", strings.NewReader("private request body")))
	rows, _ := d.snapshot()
	if len(rows) != 2 {
		t.Fatalf("expected error and request summary, got %v", rows)
	}
	b, _ := json.Marshal(rows)
	if !strings.Contains(string(b), "HTTP 504") || strings.Contains(string(b), "never-log") || strings.Contains(string(b), "private request") {
		t.Fatal(string(b))
	}
}
func TestDiagnosticGatewayCaptureAndDiskFailure(t *testing.T) {
	for _, s := range []string{"cloudflared-download route 1 HTTP 504", "HTTP Error 503", "HTTP/1.1 401"} {
		if diagnosticLevel(s) != "error" {
			t.Fatalf("missed network failure: %s", s)
		}
	}
	d := diagnosticTestStore(t, 1<<20)
	diagnosticGatewayEvent([]byte("event: log\ndata: \"KIT_FAILED stage=bootstrap cloudflared HTTP 504\"\n\n"))
	diagnosticGatewayEvent([]byte("event: state\ndata: {\"state\":\"failed\",\"profile\":\"qwen\",\"reason\":\"timeout\",\"email\":\"private@example.com\"}\n\n"))
	diagnosticGatewayEvent([]byte("event: progress\ndata: {\"value\":30}\n\n"))
	rows, _ := d.snapshot()
	if len(rows) != 2 || rows[0].Source != "gateway" || rows[0].Level != "error" {
		t.Fatalf("capture %v", rows)
	}
	for _, row := range rows {
		if strings.Contains(row.Message, "private@example") {
			t.Fatal("state payload leaked")
		}
	}
	file := filepath.Join(t.TempDir(), "not-a-directory")
	os.WriteFile(file, []byte("fixture"), 0600)
	broken := &diagnosticStore{dir: file, maxBytes: 1000}
	broken.add("error", "test", "failure")
	_, warn := broken.snapshot()
	if warn == "" {
		t.Fatal("disk failure not visible")
	}
}
