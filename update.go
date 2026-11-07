package main

// Self-update: read a small latest.json from the update URL, download the new exe, verify its
// SHA-256, swap it in place of the running exe and restart. Settings live in %APPDATA% and are
// untouched.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const AppVersion = "1.21.1"
const defaultUpdateURL = "https://github.com/a3974892-cmd/wakeloom/releases/latest/download/latest.json"

type Release struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
	Notes   string `json:"notes"`
}

var upd struct {
	mu       sync.Mutex
	state    string // idle | downloading | ready | error
	progress float64
	msg      string
}

func updateURL() string {
	// the pre-GitHub default saved in older settings counts as "not set", so everyone moves to GitHub Releases
	if u := strings.TrimSpace(getConfig().UpdateURL); u != "" && u != "https://dl.xgrok.uk/workbench/latest.json" {
		return u
	}
	return defaultUpdateURL
}

// newer reports whether version a > b (dotted numbers, an optional leading "v").
func newer(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 4; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}

func fetchRelease(ctx context.Context) (*Release, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", updateURL(), nil)
	req.Header.Set("User-Agent", "Wakeloom/"+AppVersion)
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := (&http.Client{Transport: transport, Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上更新服务器：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("更新服务器返回 %d", resp.StatusCode)
	}
	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return nil, errors.New("更新信息格式不对")
	}
	if r.Version == "" || !strings.HasPrefix(r.URL, "https://") || len(r.SHA256) != 64 {
		return nil, errors.New("更新信息不完整（需要 version / https 下载地址 / sha256）")
	}
	return &r, nil
}

func updateCheckHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	rel, err := fetchRelease(ctx)
	upd.mu.Lock()
	st := map[string]any{"current": AppVersion, "state": upd.state, "progress": upd.progress, "message": upd.msg, "url": updateURL()}
	upd.mu.Unlock()
	if err != nil {
		st["error"] = err.Error()
		jsonOut(w, 200, st)
		return
	}
	st["latest"] = rel
	st["available"] = newer(rel.Version, AppVersion)
	jsonOut(w, 200, st)
}

func updateApplyHandler(w http.ResponseWriter, r *http.Request) {
	upd.mu.Lock()
	if upd.state == "downloading" {
		upd.mu.Unlock()
		jsonOut(w, 409, map[string]string{"error": "正在下载"})
		return
	}
	upd.state, upd.progress, upd.msg = "downloading", 0, "准备下载…"
	upd.mu.Unlock()
	go func() {
		exe, err := os.Executable()
		if err == nil {
			exe, _ = filepath.EvalSymlinks(exe)
			err = applyUpdate(exe, true)
		}
		upd.mu.Lock()
		if err != nil {
			upd.state, upd.msg = "error", err.Error()
		}
		upd.mu.Unlock()
	}()
	jsonOut(w, 202, map[string]bool{"ok": true})
}

func updateStatusHandler(w http.ResponseWriter, r *http.Request) {
	upd.mu.Lock()
	defer upd.mu.Unlock()
	jsonOut(w, 200, map[string]any{"state": upd.state, "progress": upd.progress, "message": upd.msg})
}

type progressReader struct {
	r     io.Reader
	n     int64
	total int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.n += int64(n)
	if p.total > 0 {
		upd.mu.Lock()
		upd.progress = float64(p.n) / float64(p.total)
		upd.msg = fmt.Sprintf("下载中 %.1f / %.1f MB", float64(p.n)/1e6, float64(p.total)/1e6)
		upd.mu.Unlock()
	}
	return n, err
}

func applyUpdate(exe string, restart bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	rel, err := fetchRelease(ctx)
	if err != nil {
		return err
	}
	if !newer(rel.Version, AppVersion) {
		return errors.New("已经是最新版本")
	}
	dir := filepath.Dir(exe)
	tmp := filepath.Join(dir, "."+filepath.Base(exe)+".new")
	req, _ := http.NewRequestWithContext(ctx, "GET", rel.URL, nil)
	req.Header.Set("User-Agent", "Wakeloom/"+AppVersion)
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("下载失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("下载失败：HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("没法写入程序所在文件夹：%w", err)
	}
	h := sha256.New()
	total := resp.ContentLength
	if total <= 0 {
		total = rel.Size
	}
	_, err = io.Copy(io.MultiWriter(f, h), &progressReader{r: resp.Body, total: total})
	f.Close()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("下载中断：%w", err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, rel.SHA256) {
		os.Remove(tmp)
		return errors.New("下载的文件校验不通过（可能被篡改或没下完整），已放弃更新")
	}
	upd.mu.Lock()
	upd.msg = "安装中…"
	upd.mu.Unlock()
	// Windows lets a running exe be renamed but not overwritten: move it aside, put the new one in place.
	old := exe + ".old"
	os.Remove(old)
	if err := os.Rename(exe, old); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("替换程序失败：%w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(old, exe)
		return fmt.Errorf("替换程序失败：%w", err)
	}
	upd.mu.Lock()
	upd.state, upd.progress, upd.msg = "ready", 1, "更新完成，正在重启…"
	upd.mu.Unlock()
	if !restart {
		return nil
	}
	go func() {
		time.Sleep(800 * time.Millisecond)
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "CKWB_UPDATED=1")
		cmd.Dir = dir
		if err := cmd.Start(); err != nil {
			return
		}
		os.Exit(0)
	}()
	return nil
}

// cleanupOldExe removes the previous version left behind by an update.
func cleanupOldExe() {
	if exe, err := os.Executable(); err == nil {
		for i := 0; i < 10; i++ {
			if err := os.Remove(exe + ".old"); err == nil || os.IsNotExist(err) || runtime.GOOS != "windows" {
				return
			}
			time.Sleep(time.Second)
		}
	}
}
