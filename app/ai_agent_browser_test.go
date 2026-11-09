package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAgentBrowserDOM(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("CKWB_TEST_AGENT_BROWSER") != "1" {
		t.Skip("opt-in isolated browser integration")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><h1>Agent test</h1><input aria-label="Name"><input type="password" aria-label="Password"><button onclick="document.querySelector('p').textContent=document.querySelector('input').value">Apply</button><p>waiting</p></body></html>`)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	b, e := startAgentBrowser(ctx, t.TempDir(), true)
	if e != nil {
		t.Fatal(e)
	}
	defer b.close()
	if _, e = b.action(ctx, agentAction{Tool: "browser_open", URL: "javascript:alert(1)"}); e == nil {
		t.Fatal("javascript URL accepted")
	}
	text, e := b.action(ctx, agentAction{Tool: "browser_open", URL: srv.URL})
	if e != nil {
		t.Fatal(e)
	}
	ref := func(text, name string) string {
		var s struct {
			Elements []struct{ Ref, Name string } `json:"elements"`
		}
		if e := json.Unmarshal([]byte(text), &s); e != nil {
			t.Fatal(text, e)
		}
		for _, el := range s.Elements {
			if el.Name == name {
				return el.Ref
			}
		}
		t.Fatalf("element %s absent: %s", name, text)
		return ""
	}
	old := ref(text, "Name")
	text, e = b.action(ctx, agentAction{Tool: "browser_type", Ref: old, Text: `value " escaped; 中文`})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = b.action(ctx, agentAction{Tool: "browser_click", Ref: old}); e == nil {
		t.Fatal("stale ref used")
	}
	text, e = b.action(ctx, agentAction{Tool: "browser_click", Ref: ref(text, "Apply")})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(text, `escaped; 中文`) {
		t.Fatal("button did not run", text)
	}
	if _, e = b.action(ctx, agentAction{Tool: "browser_type", Ref: ref(text, "Password"), Text: "secret"}); e == nil {
		t.Fatal("password entry allowed")
	}
	b.close()
	select {
	case <-b.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("browser process did not close")
	}
}
