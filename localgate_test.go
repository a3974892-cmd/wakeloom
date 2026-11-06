package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestKitTarballAndProfiles(t *testing.T) {
	dataDir = t.TempDir()
	ps := localProfiles()
	p, ok := ps["minimax-h3"]
	if !ok {
		t.Fatalf("profiles: %v", sortedProfileIDs(ps))
	}
	sum := localProfilesSummary()
	if sum["default"] == "" {
		t.Fatal("no default")
	}
	tgz, err := buildKitTarball(p, map[string]any{"profile": "minimax-h3", "extras": []LibItem{{ID: "n", Kind: "node", URL: "https://github.com/a/b"}}})
	if err != nil {
		t.Fatal(err)
	}
	out := os.Getenv("KIT_TGZ_OUT")
	if out != "" {
		os.WriteFile(out, tgz, 0o644)
	}
	gz, _ := gzip.NewReader(bytes.NewReader(tgz))
	tr := tar.NewReader(gz)
	names := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		names[h.Name] = true
	}
	for _, want := range []string{"colab/setup.py", "gate/plan.py", "driver.py", "wbproxy.py", "request.json", "profiles/minimax-h3/profile.yaml"} {
		if !names[want] {
			t.Fatalf("missing %s in %v", want, names)
		}
	}
	_ = filepath.Join
}
