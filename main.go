package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
)

func main() {
	if os.Getenv("CKWB_HEADLESS") == "" {
		ensureSingleInstance()
	}
	initDataDir()
	lf, _ := os.OpenFile(filepath.Join(dataDir, "workbench.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if lf != nil {
		log.SetOutput(lf)
	}
	loadConfig()
	loadGoogleToken()
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
