package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLinuxConfigAndMigration(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "relative-invalid")
	if configBase() != filepath.Join(os.Getenv("HOME"), ".config") {
		t.Fatal("relative XDG path must be ignored")
	}
	base := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", base)
	oldData := dataDir
	t.Cleanup(func() { dataDir = oldData })
	old := filepath.Join(base, "ComfyColabWorkbench")
	os.MkdirAll(old, 0o700)
	os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"mode":"local"}`), 0o600)
	initDataDir()
	if dataDir != filepath.Join(base, "Wakeloom") {
		t.Fatal(dataDir)
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err != nil || string(b) != `{"mode":"local"}` {
		t.Fatal("old settings lost", err)
	}
}

func TestLinuxInstanceLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "instance.lock")
	f, err := lockInstance(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if second, err := lockInstance(p); err == nil {
		second.Close()
		t.Fatal("second instance accepted")
	}
	f.Close()
	f, err = lockInstance(p)
	if err != nil {
		t.Fatal("stale lock after exit", err)
	}
	f.Close()
}
