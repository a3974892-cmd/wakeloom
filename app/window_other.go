//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func runWindow(url string) {
	fmt.Println("Wakeloom 工作台：", url)
	fmt.Println("关闭浏览器标签页后程序仍会运行。请先在工作台停止云端 GPU，再在终端按 Ctrl+C 退出。")
	openBrowser(url)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	<-stop
}
