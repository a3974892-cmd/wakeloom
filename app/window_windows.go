//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
)

func fatal(msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	mb := user32.NewProc("MessageBoxW")
	t, _ := syscall.UTF16PtrFromString("Wakeloom")
	m, _ := syscall.UTF16PtrFromString(msg)
	mb.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10)
	os.Exit(1)
}

func runWindow(url string) {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  dataDir + `\webview`,
		WindowOptions: webview2.WindowOptions{
			Title:  "Wakeloom",
			IconId: 1,
			Width:  1320,
			Height: 860,
			Center: true,
		},
	})
	if w == nil {
		// WebView2 runtime missing: fall back to the default browser and keep serving.
		openBrowser(url)
		fatal("没找到 Microsoft Edge WebView2 运行时，已改用浏览器打开工作台（任务队列在后台照常运行）。\n安装 WebView2 后即可使用独立窗口。\n\n使用完毕后点「确定」退出程序。")
		return
	}
	defer w.Destroy()
	w.SetSize(1100, 700, webview2.HintMin)
	w.Navigate(url)
	w.Run()
}
