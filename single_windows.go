//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ensureSingleInstance stops a second copy of the workbench: two copies share the same job
// queue on disk, so both would pick up (and download, or even submit) the same jobs.
// Right after a self-update the old process may still be exiting, so wait for it briefly.
func ensureSingleInstance() {
	k32 := syscall.NewLazyDLL("kernel32.dll")
	create, closeH := k32.NewProc("CreateMutexW"), k32.NewProc("CloseHandle")
	name, _ := syscall.UTF16PtrFromString("Local\\ComfyColabWorkbench.single")
	tries := 1
	if os.Getenv("CKWB_UPDATED") != "" {
		tries = 40
	}
	for i := 0; i < tries; i++ {
		h, _, err := create.Call(0, 0, uintptr(unsafe.Pointer(name)))
		if h != 0 && err != syscall.Errno(183) { // not ERROR_ALREADY_EXISTS: we own it
			return
		}
		if h != 0 {
			closeH.Call(h)
		}
		time.Sleep(500 * time.Millisecond)
	}
	fatal("Wakeloom 已经在运行了（可能在任务栏或被其他窗口挡住）。\n\n同时开两个会让同一个任务被处理两次，所以这次不再打开新的。\n如果确实看不到窗口，请在任务管理器里结束 Wakeloom（或旧名 ComfyWorkbench）后再打开。")
}
