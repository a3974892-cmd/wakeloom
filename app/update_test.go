package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{{"1.12.0", "1.11", true}, {"v1.12.1", "1.12.0", true}, {"1.12.0", "1.12.0", false}, {"1.9", "1.10", false}, {"2.0", "1.99.9", true}} {
		if newer(c.a, c.b) != c.want {
			t.Fatalf("%s > %s should be %v", c.a, c.b, c.want)
		}
	}
}

func TestApplyUpdate(t *testing.T) {
	payload := []byte("NEW-EXE-BYTES")
	sum := sha256.Sum256(payload)
	var srv *httptest.Server
	bad := false
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest.json":
			h := hex.EncodeToString(sum[:])
			if bad {
				h = "00" + h[2:]
			}
			fmt.Fprintf(w, `{"version":"99.0.0","url":"%s/app.exe","sha256":"%s","notes":"x","os":%q,"arch":%q}`, srv.URL, h, runtime.GOOS, runtime.GOARCH)
		case "/app.exe":
			w.Write(payload)
		}
	}))
	defer srv.Close()
	transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test server cert
	defer func() { transport.TLSClientConfig = nil }()
	dataDir = t.TempDir()
	cfg = Config{UpdateURL: srv.URL + "/latest.json"}
	dir := t.TempDir()
	exe := filepath.Join(dir, "app.exe")
	os.WriteFile(exe, []byte("OLD"), 0o755)

	bad = true
	if err := applyUpdate(exe, false); err == nil {
		t.Fatal("bad checksum must fail")
	}
	if b, _ := os.ReadFile(exe); string(b) != "OLD" {
		t.Fatal("exe must be untouched after failed verify")
	}
	bad = false
	if err := applyUpdate(exe, false); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != string(payload) {
		t.Fatalf("exe not replaced: %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "OLD" {
		t.Fatal("old exe not kept aside")
	}
	if runtime.GOOS == "linux" {
		st, err := os.Stat(exe)
		if err != nil || st.Mode().Perm()&0o111 == 0 {
			t.Fatal("updated Linux binary must be executable")
		}
	}
}

func TestReleasePlatforms(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		if platformUpdateURL("linux", arch) == defaultUpdateURL {
			t.Fatal("Linux must not download Windows manifest")
		}
		for _, r := range []Release{{}, {OS: "windows", Arch: arch}, {OS: "linux", Arch: "wrong"}} {
			if validateReleasePlatform(&r, "linux", arch) == nil {
				t.Fatal("incompatible release accepted")
			}
		}
		if err := validateReleasePlatform(&Release{OS: "linux", Arch: arch}, "linux", arch); err != nil {
			t.Fatal(err)
		}
	}
	if err := validateReleasePlatform(&Release{}, "windows", "amd64"); err != nil {
		t.Fatal("legacy Windows manifest must remain compatible")
	}
}
