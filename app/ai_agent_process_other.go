//go:build !windows

package main

import (
	"context"
	"errors"
	"os/exec"
)

func agentHide(cmd *exec.Cmd) {}
func agentManagedStart(cmd *exec.Cmd) (func(), error) {
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	return func() { agentKillTree(cmd) }, nil
}
func agentKillTree(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
func agentRunCommand(context.Context, string, string) (string, error) {
	return "", errors.New("自动执行测试版目前仅支持 Windows")
}
func agentBrowserExecutable() string { return "" }
