package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Keep the descriptor open for the lifetime of the process. Kernel locks disappear
// on exit (including crashes), so no stale PID or shared TCP port is involved.
var singleFile *os.File

func lockInstance(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func ensureSingleInstance() {
	base := configBase()
	if err := os.MkdirAll(base, 0o700); err != nil {
		fatal("无法创建配置目录：" + err.Error())
	}
	tries := 1
	if os.Getenv("CKWB_UPDATED") != "" {
		tries = 40
	}
	for i := 0; i < tries; i++ {
		f, err := lockInstance(filepath.Join(base, ".wakeloom.lock"))
		if err == nil {
			singleFile = f
			return
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			fatal("无法锁定配置目录：" + err.Error())
		}
		if i+1 < tries {
			time.Sleep(250 * time.Millisecond)
		}
	}
	fatal("Wakeloom 已经在运行，请使用已打开的工作台；关闭原进程后可以重新启动。")
}
