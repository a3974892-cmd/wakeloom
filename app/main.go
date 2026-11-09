package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
)

func main() {
	if runtime.GOOS == "linux" {
		ensureSingleInstance()
	} else if os.Getenv("CKWB_HEADLESS") == "" {
		ensureSingleInstance()
	}
	initDataDir()
	hadConfig := configExisted()
	loadConfig()
	loadGoogleToken()
	initDiagnostics()
	go refreshDesktopIcon()
	defer func() {
		if p := recover(); p != nil {
			diagEvent("error", "app", fmt.Sprintf("panic: %v", p))
			panic(p)
		}
	}()
	migrateOnboarding(hadConfig)
	store.load()
	startLocalGate()
	go cleanupOldExe()
	go worker()
	go ud.loop()

	addr := "127.0.0.1:0"
	if p := os.Getenv("CKWB_PORT"); p != "" {
		addr = "127.0.0.1:" + p
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fatal("无法启动本地服务：" + err.Error())
	}
	go http.Serve(ln, newServer())
	url := fmt.Sprintf("http://%s/?k=%s", ln.Addr().String(), apiKey)
	log.Println("workbench at", url)
	if os.Getenv("CKWB_HEADLESS") != "" {
		fmt.Println(url)
		select {}
	}
	runWindow(url)
}
