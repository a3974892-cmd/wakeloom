//go:build windows

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

//go:embed winres/icon.png
var desktopIconPNG []byte

//go:embed winres/refresh-shortcuts.ps1
var desktopShortcutScript string

// A content-addressed icon avoids reusing Explorer's old executable icon entry.
// Only shortcuts targeting this exact executable are changed, never other installs.
func refreshDesktopIcon() {
	if os.Getenv("CKWB_HEADLESS") != "" {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	iconDir := filepath.Join(dataDir, "icons")
	if err := os.MkdirAll(iconDir, 0700); err != nil {
		return
	}
	digest := sha256.Sum256(desktopIconPNG)
	iconPath := filepath.Join(iconDir, fmt.Sprintf("wakeloom-%x.ico", digest[:12]))
	// ICO directory with one PNG-compressed 256 x 256, 32-bit entry.
	ico := make([]byte, 22, 22+len(desktopIconPNG))
	binary.LittleEndian.PutUint16(ico[2:], 1)
	binary.LittleEndian.PutUint16(ico[4:], 1)
	binary.LittleEndian.PutUint16(ico[10:], 1)
	binary.LittleEndian.PutUint16(ico[12:], 32)
	binary.LittleEndian.PutUint32(ico[14:], uint32(len(desktopIconPNG)))
	binary.LittleEndian.PutUint32(ico[18:], 22)
	ico = append(ico, desktopIconPNG...)
	if previous, _ := os.ReadFile(iconPath); !bytes.Equal(previous, ico) {
		if err := os.WriteFile(iconPath, ico, 0600); err != nil {
			diagEvent("warn", "desktop", "无法写入桌面图标缓存")
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	changed, failed, err := repairDesktopShortcuts(ctx, exe, iconPath, nil)
	if err != nil || failed != 0 {
		diagEvent("warn", "desktop", "部分桌面快捷方式图标未能刷新；下次启动将重试")
	}
	for _, path := range changed {
		notifyDesktopItem(path)
	}
	// Portable EXEs can live directly on the desktop (without any .lnk).
	// Invalidate the Shell icon cache once per version/path, without restarting
	// Explorer or deleting its cache databases.
	key := sha256.Sum256([]byte(strings.ToLower(exe) + AppVersion + iconPath))
	marker := filepath.Join(iconDir, fmt.Sprintf("refreshed-%x", key[:12]))
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		notifyDesktopItem(exe)
		syscall.NewLazyDLL("shell32.dll").NewProc("SHChangeNotify").Call(0x08000000, 0x3000, 0, 0)
		_ = os.WriteFile(marker, []byte(AppVersion), 0600)
	}
	if len(changed) > 0 {
		diagEvent("info", "desktop", fmt.Sprintf("已刷新 %d 个桌面快捷方式图标", len(changed)))
	}
}

func notifyDesktopItem(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err == nil {
		// SHCNE_UPDATEITEM, SHCNF_PATHW | SHCNF_FLUSHNOWAIT.
		syscall.NewLazyDLL("shell32.dll").NewProc("SHChangeNotify").Call(0x2000, 0x3005, uintptr(unsafe.Pointer(p)), 0)
	}
}

func repairDesktopShortcuts(ctx context.Context, exe, icon string, folders []string) ([]string, int, error) {
	options, err := json.Marshal(struct {
		Exe     string   `json:"exe"`
		Icon    string   `json:"icon"`
		Folders []string `json:"folders"`
	}{exe, icon, folders})
	if err != nil {
		return nil, 0, err
	}
	chars := utf16.Encode([]rune(desktopShortcutScript))
	encoded := make([]byte, len(chars)*2)
	for i, c := range chars {
		binary.LittleEndian.PutUint16(encoded[i*2:], c)
	}
	// File paths are JSON data, never PowerShell source. Use the system binary.
	cmd := exec.CommandContext(ctx, filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		"-NoLogo", "-NoProfile", "-NonInteractive", "-EncodedCommand", base64.StdEncoding.EncodeToString(encoded))
	cmd.Env = append(os.Environ(), "WAKELOOM_ICON_OPTIONS="+string(options))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	out, err := cmd.Output()
	if err != nil {
		return nil, 0, err
	}
	var result struct {
		Changed []string `json:"changed"`
		Failed  int      `json:"failed"`
	}
	err = json.Unmarshal(bytes.TrimPrefix(out, []byte{0xef, 0xbb, 0xbf}), &result)
	return result.Changed, result.Failed, err
}
