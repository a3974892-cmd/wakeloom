//go:build windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestDesktopIconMigrationPreservesShortcutTargets(t *testing.T) {
	// Exercise real Windows .lnk persistence in an isolated directory, including
	// Unicode and shell metacharacters; never touch the real desktop.
	dir := filepath.Join(t.TempDir(), "旧 用户 '$()")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "Wakeloom.exe")
	icon := filepath.Join(dir, "new-icon.ico")
	for _, p := range []string{exe, icon, filepath.Join(dir, "Other.exe")} {
		if err := os.WriteFile(p, []byte("test fixture, not executable"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(script string) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32/WindowsPowerShell/v1.0/powershell.exe"), "-NoProfile", "-NonInteractive", "-Command", script)
		cmd.Env = append(os.Environ(), "WB_ICON_FIXTURE="+dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("fixture: %v: %s", err, out)
		}
		return out
	}
	run(`$ErrorActionPreference='Stop'; $s=New-Object -ComObject WScript.Shell
foreach ($name in @('Current','Other')) {
 $l=$s.CreateShortcut((Join-Path $env:WB_ICON_FIXTURE ($name+'.lnk')))
 $l.TargetPath=Join-Path $env:WB_ICON_FIXTURE $(if ($name -eq 'Current') {'Wakeloom.exe'} else {'Other.exe'})
 $l.Arguments='--original-argument'; $l.WorkingDirectory=$env:WB_ICON_FIXTURE
 $l.Description='keep this'; $l.IconLocation='shell32.dll,4'; $l.WindowStyle=3; $l.Save()
}`)
	other := filepath.Join(dir, "Other.lnk")
	before, err := os.ReadFile(other)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	changed, failed, err := repairDesktopShortcuts(ctx, strings.ToUpper(exe), icon, []string{dir})
	if err != nil || failed != 0 || len(changed) != 1 || filepath.Base(changed[0]) != "Current.lnk" {
		t.Fatalf("migration: changed=%v failed=%d err=%v", changed, failed, err)
	}
	after, _ := os.ReadFile(other)
	if string(before) != string(after) {
		t.Fatal("unrelated shortcut changed")
	}
	output := run(`[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false)
$s=New-Object -ComObject WScript.Shell; $l=$s.CreateShortcut((Join-Path $env:WB_ICON_FIXTURE 'Current.lnk'))
@{target=$l.TargetPath; args=$l.Arguments; cwd=$l.WorkingDirectory; description=$l.Description; icon=$l.IconLocation; style=$l.WindowStyle} | ConvertTo-Json -Compress`)
	var got struct {
		Target, Args, Cwd, Description, Icon string
		Style                                int
	}
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatalf("decode: %v: %s", err, output)
	}
	sameFile := func(a, b string) bool {
		x, xe := os.Stat(a)
		y, ye := os.Stat(b)
		return xe == nil && ye == nil && os.SameFile(x, y)
	}
	if !sameFile(got.Target, exe) || got.Args != "--original-argument" || !sameFile(got.Cwd, dir) || got.Description != "keep this" || got.Style != 3 || !strings.HasSuffix(got.Icon, ",0") || !sameFile(strings.TrimSuffix(got.Icon, ",0"), icon) {
		t.Fatalf("shortcut properties changed: %+v", got)
	}
	changed, failed, err = repairDesktopShortcuts(ctx, exe, icon, []string{dir})
	if err != nil || failed != 0 || len(changed) != 0 {
		t.Fatalf("not idempotent: %v %d %v", changed, failed, err)
	}
}
