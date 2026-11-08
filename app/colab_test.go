package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// Runs against a real Jupyter server when JUPYTER_TEST_URL is set (auth disabled).
func TestRuntimeAgainstJupyter(t *testing.T) {
	u := os.Getenv("JUPYTER_TEST_URL")
	if u == "" {
		t.Skip("JUPYTER_TEST_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	rt := &Runtime{A: &Assignment{Proxy: ProxyInfo{URL: u, Token: "x"}}}
	var lines []string
	err := rt.Exec(ctx, "import time\nfor i in range(3):\n    print('tick', i, flush=True); time.sleep(0.2)\nprint('no newline at end', end='')", func(l string) { lines = append(lines, l) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "|") != "tick 0|tick 1|tick 2|no newline at end" {
		t.Fatalf("lines %q", lines)
	}
	if err := rt.Upload(ctx, []byte("hello"), "/sub_up.txt"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("/tmp/jroot/sub_up.txt")
	if err != nil || string(b) != "hello" {
		t.Fatalf("upload: %q %v", b, err)
	}
	err = rt.Exec(ctx, "raise ValueError('boom')", func(string) {})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("want error, got %v", err)
	}
	// kernel reused across calls
	var out []string
	rt.Exec(ctx, "print(1+1)", func(l string) { out = append(out, l) })
	if strings.Join(out, "") != "2" {
		t.Fatalf("out %q", out)
	}
}
