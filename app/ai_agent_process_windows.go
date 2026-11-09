//go:build windows

package main

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

func agentHide(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

// A job closes all descendants even if a command starts a child then exits.
// This is lifecycle management, not a privilege/filesystem sandbox.
func agentManagedStart(cmd *exec.Cmd) (func(), error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	closeJob := func() { _ = windows.CloseHandle(job) }
	var limits windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		closeJob()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		closeJob()
		return nil, err
	}
	handle, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, handle)
		_ = windows.CloseHandle(handle)
	}
	if err != nil {
		agentKillTree(cmd)
		_ = cmd.Wait()
		closeJob()
		return nil, fmt.Errorf("无法建立任务进程清理机制：%w", err)
	}
	return closeJob, nil
}
func agentKillTree(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	killer := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "taskkill.exe"), "/PID", fmt.Sprint(cmd.Process.Pid), "/T", "/F")
	agentHide(killer)
	_ = killer.Run()
	_ = cmd.Process.Kill()
}
func agentRunCommand(ctx context.Context, root, command string) (string, error) {
	text := "[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); $OutputEncoding = [Console]::OutputEncoding; $ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue';\n" + command + "\nif ($LASTEXITCODE) { exit $LASTEXITCODE }"
	u := utf16.Encode([]rune(text))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		binary.LittleEndian.PutUint16(b[2*i:], v)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"), "-NoLogo", "-NoProfile", "-NonInteractive", "-OutputFormat", "Text", "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Dir = root
	agentHide(cmd)
	output := &agentOutput{}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Cancel = func() error { agentKillTree(cmd); return nil }
	cmd.WaitDelay = 3 * time.Second
	cleanup, err := agentManagedStart(cmd)
	if err != nil {
		return output.String(), err
	}
	defer cleanup()
	err = cmd.Wait()
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return output.String(), err
}

func agentBrowserExecutable() string {
	for _, base := range []string{os.Getenv("ProgramFiles(x86)"), os.Getenv("ProgramFiles"), os.Getenv("LOCALAPPDATA")} {
		for _, rel := range []string{"Microsoft/Edge/Application/msedge.exe", "Google/Chrome/Application/chrome.exe"} {
			p := filepath.Join(base, filepath.FromSlash(rel))
			if st, e := os.Stat(p); e == nil && !st.IsDir() {
				return p
			}
		}
	}
	return ""
}
