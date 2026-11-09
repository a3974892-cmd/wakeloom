//go:build windows

package main

import (
	"context"
	"golang.org/x/sys/windows"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAgentCommandCleansDescendants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	out, _ := agentRunCommand(ctx, t.TempDir(), `$child = Start-Process -FilePath "$PSHOME\powershell.exe" -ArgumentList '-NoProfile','-NonInteractive','-Command','Start-Sleep -Seconds 20' -WindowStyle Hidden -PassThru; Write-Output $child.Id`)
	pid, e := strconv.Atoi(strings.TrimSpace(out))
	if e != nil {
		t.Fatal("child PID missing", out, e)
	}
	time.Sleep(100 * time.Millisecond)
	handle, e := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if e != nil {
		return
	}
	defer windows.CloseHandle(handle)
	var code uint32
	if e = windows.GetExitCodeProcess(handle, &code); e == nil && code == 259 {
		t.Fatal("background child survived command cleanup", pid)
	}
}
