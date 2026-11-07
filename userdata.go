package main

// Back up ComfyUI's user data (user/default/: saved workflows, comfy.settings.json, subgraphs, templates …)
// to this PC and put it back on the next boot. Every Colab VM is new, so without this whatever the user
// saved in the ComfyUI web page is gone after a stop.
//
// Only ComfyUI's own userdata API is used, through the same gate as everything else (baseURL + addAuth):
//   list      GET  /api/v2/userdata?path=<dir>            (fallback: GET /api/userdata?dir=.&recurse=true&full_info=true)
//   download  GET  /api/userdata/<path with / as %2F>
//   upload    POST /api/userdata/<path with / as %2F>?overwrite=true   (body = file content)
// The VPS gateway forwards the raw path (keeps %2F) and counts every forwarded request as activity, which
// can postpone its idle stop; it has no "don't count this" switch, so syncs run at most every 5 minutes and
// only while the GPU is ready. The local gate (local mode) does not count requests marked X-WB-Sync.
//
// Local layout (<dataDir>/comfy-user/): files/<relative path>, manifest.json, .trash/<YYYY-MM-DD>/<path>.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	udMaxFile    int64 = 20 << 20  // skip single files bigger than this
	udMaxTotal   int64 = 200 << 20 // stop adding files beyond this total
	udInterval         = 5 * time.Minute
	udStopWait         = 20 * time.Second
	udTrashDays        = 30
	udTick             = 20 * time.Second
	udRestoreTry       = 3
)

const udSyncHeader = "X-WB-Sync" // the local gate does not count these requests as activity

// ---------------- paths ----------------

var (
	udDrive    = regexp.MustCompile(`^[A-Za-z]:`)
	udBadChars = regexp.MustCompile(`[<>:"|?*\x00-\x1f\\]`)
	udReserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])(\..*)?$`)
)

// udValidPath accepts a relative, forward-slash path that stays inside the backup folder on any OS.
func udValidPath(p string) error {
	switch {
	case p == "" || len(p) > 400:
		return errors.New("空路径或路径太长")
	case strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || udDrive.MatchString(p):
		return fmt.Errorf("不能是绝对路径：%q", p)
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "" || part == "." || part == "..":
			return fmt.Errorf("路径不合法：%q", p)
		case udBadChars.MatchString(part) || udReserved.MatchString(part) || strings.HasSuffix(part, ".") || strings.HasSuffix(part, " "):
			return fmt.Errorf("路径里有 Windows 不支持的名字：%q", p)
		}
	}
	return nil
}

// udSkip: temp / cache files that are not worth keeping.
func udSkip(p string) bool {
	parts := strings.Split(strings.ToLower(p), "/")
	for _, d := range parts[:len(parts)-1] {
		switch d {
		case "__pycache__", ".cache", "cache", "tmp", "temp", ".git":
			return true
		}
	}
	base := parts[len(parts)-1]
	switch base {
	case ".ds_store", "thumbs.db", "desktop.ini":
		return true
	}
	for _, ext := range []string{".tmp", ".temp", ".part", ".partial", ".lock", ".log", ".swp", ".pyc", "~"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return strings.HasPrefix(base, "~$")
}

// ---------------- remote (ComfyUI userdata API) ----------------

type udFile struct {
	Path string
	Size int64
	Mod  float64 // ComfyUI's modification time; only compared for equality
}

type udRemote struct {
	base string
	auth func(*http.Request)
	hc   *http.Client
}

func gateRemote() *udRemote {
	return &udRemote{base: baseURL(), hc: &http.Client{Transport: transport, Timeout: 90 * time.Second},
		auth: func(r *http.Request) { addAuth(r); r.Header.Set(udSyncHeader, "1") }}
}

// udEscape keeps the path one URL segment: "a/b.json" -> "a%2Fb.json" (ComfyUI's route needs the %2F).
func udEscape(p string) string { return url.PathEscape(p) }

const udMaxList = 32 << 20 // a file listing is JSON, not a user file: its own read limit

func (r *udRemote) do(ctx context.Context, method, u string, body []byte, limit int64) ([]byte, int, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+u, rd)
	if err != nil {
		return nil, 0, err
	}
	if r.auth != nil {
		r.auth(req)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") && method == "GET" && strings.HasPrefix(u, "/api/v") {
		return nil, resp.StatusCode, errors.New("返回了网页而不是数据（网关拦截或 ComfyUI 未就绪）")
	}
	return b, resp.StatusCode, nil
}

type udListEntry struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"`
	Type     string  `json:"type"`
	Size     int64   `json:"size"`
	Modified float64 `json:"modified"`
}

// list returns every file under user/default/ (v2 API, walking sub-folders; v1 as fallback).
func (r *udRemote) list(ctx context.Context) ([]udFile, error) {
	var out []udFile
	dirs := []string{""}
	for n := 0; len(dirs) > 0; n++ {
		if n > 2000 {
			return nil, errors.New("目录太多")
		}
		dir := dirs[0]
		dirs = dirs[1:]
		b, code, err := r.do(ctx, "GET", "/api/v2/userdata?path="+url.QueryEscape(dir), nil, udMaxList)
		if err != nil {
			return nil, err
		}
		if code == 404 && dir == "" {
			return r.listV1(ctx)
		}
		if code >= 400 {
			return nil, fmt.Errorf("列出用户文件失败：HTTP %d", code)
		}
		var es []udListEntry
		if err := json.Unmarshal(b, &es); err != nil {
			if dir == "" {
				return r.listV1(ctx)
			}
			return nil, errors.New("ComfyUI 返回的文件列表看不懂")
		}
		for _, e := range es {
			p := e.Path
			if p == "" {
				p = strings.TrimPrefix(dir+"/"+e.Name, "/")
			}
			p = strings.ReplaceAll(p, "\\", "/")
			if e.Type == "directory" {
				if udValidPath(p) == nil && !udSkip(p+"/x") {
					dirs = append(dirs, p)
				}
				continue
			}
			out = append(out, udFile{Path: p, Size: e.Size, Mod: e.Modified})
		}
	}
	return out, nil
}

func (r *udRemote) listV1(ctx context.Context) ([]udFile, error) {
	b, code, err := r.do(ctx, "GET", "/api/userdata?dir=.&recurse=true&full_info=true&split=false", nil, udMaxList)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("列出用户文件失败：HTTP %d（ComfyUI 版本太旧？）", code)
	}
	var es []udListEntry
	if err := json.Unmarshal(b, &es); err != nil {
		return nil, errors.New("ComfyUI 返回的文件列表看不懂")
	}
	out := make([]udFile, 0, len(es))
	for _, e := range es {
		out = append(out, udFile{Path: strings.TrimPrefix(strings.ReplaceAll(e.Path, "\\", "/"), "./"), Size: e.Size, Mod: e.Modified})
	}
	return out, nil
}

func (r *udRemote) get(ctx context.Context, p string) ([]byte, error) {
	b, code, err := r.do(ctx, "GET", "/api/userdata/"+udEscape(p), nil, udMaxFile+1)
	if err != nil {
		return nil, err
	}
	if code >= 400 {
		return nil, fmt.Errorf("下载 %s 失败：HTTP %d", p, code)
	}
	if int64(len(b)) > udMaxFile {
		return nil, fmt.Errorf("%s 超过 %d MB", p, udMaxFile>>20)
	}
	return b, nil
}

func (r *udRemote) put(ctx context.Context, p string, data []byte) error {
	_, code, err := r.do(ctx, "POST", "/api/userdata/"+udEscape(p)+"?overwrite=true", data, 1<<20)
	if err != nil {
		return err
	}
	if code >= 400 {
		return fmt.Errorf("上传 %s 失败：HTTP %d", p, code)
	}
	return nil
}

// ---------------- local store ----------------

type udEntry struct {
	SHA256 string  `json:"sha256"`
	Size   int64   `json:"size"`
	Mod    float64 `json:"modified"`                // remote modification time when last saved
	Saved  int64   `json:"saved"`                   // unix time of the local copy
	Origin string  `json:"origin_sha256,omitempty"` // content this file had on a freshly booted VM
}

type udManifest struct {
	Files       map[string]*udEntry `json:"files"`
	LastBackup  int64               `json:"last_backup"`
	LastRestore int64               `json:"last_restore"`
	Session     string              `json:"session"` // boot session the last restore ran for
	Skipped     []string            `json:"skipped,omitempty"`
}

type udStore struct {
	dir string
	mu  sync.Mutex // one backup / restore at a time
}

func (s *udStore) filesDir() string      { return filepath.Join(s.dir, "files") }
func (s *udStore) local(p string) string { return filepath.Join(s.filesDir(), filepath.FromSlash(p)) }
func (s *udStore) manifestPath() string  { return filepath.Join(s.dir, "manifest.json") }
func (s *udStore) trashDir(t time.Time) string {
	return filepath.Join(s.dir, ".trash", t.Format("2006-01-02"))
}

func (s *udStore) load() *udManifest {
	m := &udManifest{Files: map[string]*udEntry{}}
	if b, err := os.ReadFile(s.manifestPath()); err == nil {
		json.Unmarshal(b, m)
	}
	if m.Files == nil {
		m.Files = map[string]*udEntry{}
	}
	for p := range m.Files { // never trust paths from disk either
		if udValidPath(p) != nil {
			delete(m.Files, p)
		}
	}
	return m
}

func (s *udStore) save(m *udManifest) error {
	os.MkdirAll(s.dir, 0o755)
	return writeJSON(s.manifestPath(), m)
}

// inside makes sure a validated path really lands under files/ (belt and braces).
func (s *udStore) inside(p string) (string, error) {
	if err := udValidPath(p); err != nil {
		return "", err
	}
	full := s.local(p)
	rel, err := filepath.Rel(s.filesDir(), full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("路径越界：%q", p)
	}
	return full, nil
}

func (s *udStore) write(p string, data []byte) error {
	full, err := s.inside(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	tmp := full + ".wbtmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, full)
}

func (s *udStore) toTrash(p string, now time.Time) error {
	full, err := s.inside(p)
	if err != nil {
		return err
	}
	if _, err := os.Stat(full); err != nil {
		return nil
	}
	dst := filepath.Join(s.trashDir(now), filepath.FromSlash(p))
	if _, err := os.Stat(dst); err == nil {
		dst += "." + now.Format("150405")
	}
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.Rename(full, dst); err != nil {
		return err
	}
	// drop now-empty folders under files/
	for d := filepath.Dir(full); d != s.filesDir() && strings.HasPrefix(d, s.filesDir()); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	return nil
}

func (s *udStore) purgeTrash(now time.Time) {
	ents, _ := os.ReadDir(filepath.Join(s.dir, ".trash"))
	for _, e := range ents {
		t, err := time.ParseInLocation("2006-01-02", e.Name(), now.Location())
		if err == nil && now.Sub(t) > time.Duration(udTrashDays)*24*time.Hour {
			os.RemoveAll(filepath.Join(s.dir, ".trash", e.Name()))
		}
	}
}

func shaHex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// filter applies the safety rules and the size limits; skipped describes what was left out.
func udFilter(files []udFile) (keep []udFile, all map[string]udFile, skipped []string) {
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	all = map[string]udFile{}
	var total int64
	for _, f := range files {
		if udValidPath(f.Path) != nil {
			skipped = append(skipped, "路径不安全，已忽略："+f.Path)
			continue
		}
		if udSkip(f.Path) {
			continue
		}
		all[f.Path] = f
		switch {
		case f.Size > udMaxFile:
			skipped = append(skipped, fmt.Sprintf("超过 %d MB，未备份：%s", udMaxFile>>20, f.Path))
		case total+f.Size > udMaxTotal:
			skipped = append(skipped, fmt.Sprintf("总量超过 %d MB，未备份：%s", udMaxTotal>>20, f.Path))
		default:
			total += f.Size
			keep = append(keep, f)
		}
	}
	return
}

type udResult struct {
	Files      int      `json:"files"`      // files in the backup after this run
	Downloaded int      `json:"downloaded"` // changed files fetched
	Trashed    int      `json:"trashed"`
	Skipped    []string `json:"skipped,omitempty"`
}

// backup: incremental download (size + modified time), mirror deletions into .trash.
// fresh = the files as they were on the freshly booted VM (from restore): content saved while it still
// matches is the "original" of that file. allowDelete must only be true once this session was restored.
func (s *udStore) backup(ctx context.Context, r *udRemote, fresh map[string]udFile, allowDelete bool) (udResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := r.list(ctx)
	if err != nil {
		return udResult{}, err
	}
	keep, all, skipped := udFilter(list)
	m := s.load()
	res := udResult{Skipped: skipped}
	now := time.Now()
	var firstErr error
	for _, f := range keep {
		if err := ctx.Err(); err != nil {
			firstErr = err
			break
		}
		e := m.Files[f.Path]
		if e != nil && e.Size == f.Size && e.Mod == f.Mod {
			if full, err := s.inside(f.Path); err == nil {
				if st, err := os.Stat(full); err == nil && st.Size() == e.Size {
					continue // unchanged
				}
			}
		}
		data, err := r.get(ctx, f.Path)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := s.write(f.Path, data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if e == nil {
			e = &udEntry{}
			m.Files[f.Path] = e
		}
		e.SHA256, e.Size, e.Mod, e.Saved = shaHex(data), int64(len(data)), f.Mod, now.Unix()
		if fr, ok := fresh[f.Path]; ok && fr.Size == f.Size && fr.Mod == f.Mod {
			e.Origin = e.SHA256 // still exactly what the new VM came with
		}
		res.Downloaded++
	}
	if allowDelete && firstErr == nil {
		for p := range m.Files {
			if _, ok := all[p]; ok {
				continue
			}
			if err := s.toTrash(p, now); err == nil {
				delete(m.Files, p)
				res.Trashed++
			}
		}
	}
	s.purgeTrash(now)
	res.Files = len(m.Files)
	m.Skipped = skipped
	if firstErr == nil {
		m.LastBackup = now.Unix()
	}
	if err := s.save(m); err != nil && firstErr == nil {
		firstErr = err
	}
	return res, firstErr
}

// restore uploads the local backup to a freshly booted VM. Local wins, except a file the user never
// changed (local content == the original a fresh VM had last time): then the VM's (maybe newer) version stays.
// Returns how many files were uploaded and the fresh VM's file list.
func (s *udStore) restore(ctx context.Context, r *udRemote, session string) (int, map[string]udFile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list, err := r.list(ctx)
	if err != nil {
		return 0, nil, err
	}
	fresh := map[string]udFile{}
	for _, f := range list {
		fresh[f.Path] = f
	}
	m := s.load()
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	uploaded := map[string]bool{}
	var firstErr error
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return len(uploaded), fresh, err
		}
		e := m.Files[p]
		full, err := s.inside(p)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(full)
		if err != nil {
			continue // missing locally: nothing to restore
		}
		local := shaHex(data)
		if rf, ok := fresh[p]; ok {
			remote, err := r.get(ctx, p)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			rsha := shaHex(remote)
			origin := e.Origin
			e.Origin = rsha // what this VM came with = the original from now on
			if rsha == local {
				e.Size, e.Mod = rf.Size, rf.Mod
				continue
			}
			if origin != "" && local == origin {
				e.Mod = -1 // never edited by the user: keep the VM's version; force the next backup to fetch it
				continue
			}
		}
		if err := r.put(ctx, p, data); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		uploaded[p] = true
	}
	// refresh size / modified time of what we uploaded, so the next backup does not download it again
	if len(uploaded) > 0 {
		if after, err := r.list(ctx); err == nil {
			for _, f := range after {
				if e := m.Files[f.Path]; e != nil && uploaded[f.Path] && f.Size == e.Size {
					e.Mod = f.Mod
				}
			}
		}
	}
	if firstErr == nil {
		m.Session, m.LastRestore = session, time.Now().Unix()
	}
	if err := s.save(m); err != nil && firstErr == nil {
		firstErr = err
	}
	return len(uploaded), fresh, firstErr
}

// ---------------- sync controller ----------------

type udSyncer struct {
	store *udStore
	mu    sync.Mutex
	// per boot session
	session    string
	restored   bool
	tries      int
	fresh      map[string]udFile
	restoreErr string
	// last backup
	lastSync time.Time
	lastErr  string
	lastRes  udResult
	running  string // "backup" | "restore" | ""
	remote   func() *udRemote
	status   func() (map[string]any, error)
	log      func(string)
}

var ud = &udSyncer{remote: gateRemote, status: gateStatus, log: udLog}

func userdataEnabled() bool {
	c := getConfig()
	return c.UserdataSync == nil || *c.UserdataSync
}

func (u *udSyncer) init() {
	u.mu.Lock()
	if u.store == nil {
		u.store = &udStore{dir: filepath.Join(dataDir, "comfy-user")}
	}
	u.mu.Unlock()
}

func sessionKey(st map[string]any) string {
	return fmt.Sprint(st["session_started"]) + "|" + fmt.Sprint(st["profile"])
}

// tick: restore once per boot, then back up every udInterval while the GPU is ready.
func (u *udSyncer) tick(ctx context.Context) {
	if !userdataEnabled() {
		return
	}
	st, err := u.status()
	if err != nil || fmt.Sprint(st["state"]) != "ready" {
		return
	}
	u.init()
	key := sessionKey(st)
	u.mu.Lock()
	if key != u.session {
		u.session, u.restored, u.tries, u.fresh, u.restoreErr = key, false, 0, nil, ""
		appLog.mu.Lock()
		appLog.lines = nil // log lines belong to one boot
		appLog.mu.Unlock()
		if u.store.load().Session == key { // the app was restarted during a session it already restored
			u.restored = true
		}
	}
	needRestore := !u.restored && u.tries < udRestoreTry
	due := u.restored && time.Since(u.lastSync) >= udInterval
	u.mu.Unlock()
	switch {
	case needRestore:
		u.restore(ctx, key)
	case due:
		u.backup(ctx)
	}
}

func (u *udSyncer) restore(ctx context.Context, key string) {
	u.mu.Lock()
	u.tries++
	u.running = "restore"
	u.mu.Unlock()
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	n, fresh, err := u.store.restore(rctx, u.remote(), key)
	cancel()
	u.mu.Lock()
	u.running = ""
	if err != nil {
		u.restoreErr = "恢复工作流失败：" + err.Error()
		last := u.tries >= udRestoreTry
		u.mu.Unlock()
		if last {
			u.log("WB_USERDATA 恢复工作流失败（不影响使用，本次开机不再自动备份）：" + err.Error())
		}
		return
	}
	u.restored, u.fresh, u.restoreErr, u.lastSync = true, fresh, "", time.Time{}
	u.mu.Unlock()
	u.log(fmt.Sprintf("WB_USERDATA 已恢复 %d 个工作流/设置文件", n))
}

// backup runs one incremental backup now (needs a ready GPU whose session was restored).
func (u *udSyncer) backup(ctx context.Context) (udResult, error) {
	u.init()
	u.mu.Lock()
	if !u.restored {
		msg := "这次开机还没恢复过工作流，暂不备份（避免用新机器上的默认文件覆盖你的备份）"
		if u.restoreErr != "" {
			msg = u.restoreErr
		}
		u.mu.Unlock()
		return udResult{}, errors.New(msg)
	}
	fresh := u.fresh
	u.running = "backup"
	u.mu.Unlock()
	res, err := u.store.backup(ctx, u.remote(), fresh, true)
	u.mu.Lock()
	u.running = ""
	u.lastRes = res
	if err != nil {
		u.lastErr = err.Error()
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			u.lastErr = "备份超时，部分工作流可能未备份"
		}
	} else {
		u.lastErr, u.lastSync = "", time.Now()
	}
	u.mu.Unlock()
	return res, err
}

// beforeStop backs up (at most udStopWait) before the GPU is stopped. Returns a warning or "".
func (u *udSyncer) beforeStop() string {
	if !userdataEnabled() {
		return ""
	}
	st, err := u.status()
	if err != nil || fmt.Sprint(st["state"]) != "ready" {
		return ""
	}
	u.init()
	u.mu.Lock()
	ok := u.session == sessionKey(st) && u.restored
	u.mu.Unlock()
	if !ok {
		return "" // nothing was restored this session: backing up now could overwrite the backup with VM defaults
	}
	ctx, cancel := context.WithTimeout(context.Background(), udStopWait)
	defer cancel()
	if _, err := u.backup(ctx); err != nil {
		return "部分工作流可能未备份：" + u.statusMap()["error"].(string)
	}
	return ""
}

func (u *udSyncer) statusMap() map[string]any {
	u.init()
	m := u.store.load()
	u.mu.Lock()
	defer u.mu.Unlock()
	errMsg := u.lastErr
	if errMsg == "" {
		errMsg = u.restoreErr
	}
	return map[string]any{"enabled": userdataEnabled(), "last_backup": m.LastBackup, "last_restore": m.LastRestore,
		"files": len(m.Files), "running": u.running, "error": errMsg, "skipped": nonNil(m.Skipped),
		"restored": u.restored, "dir": u.store.dir}
}

func (u *udSyncer) loop() {
	for {
		u.tick(context.Background())
		time.Sleep(udTick)
	}
}

// ---------------- log lines for the startup log ----------------

var appLog struct {
	mu    sync.Mutex
	lines []string
	subs  map[chan string]struct{}
}

// udLog writes a line into the startup log: the local gate's log in local mode; in gateway mode the app
// adds it to the event stream it relays from the gateway (the gateway's own log cannot be written).
func udLog(s string) {
	if localMode() {
		lg.logLine(s)
		return
	}
	appLog.mu.Lock()
	appLog.lines = append(appLog.lines, s)
	if len(appLog.lines) > 50 {
		appLog.lines = appLog.lines[len(appLog.lines)-50:]
	}
	for ch := range appLog.subs {
		select {
		case ch <- s:
		default:
		}
	}
	appLog.mu.Unlock()
}

func appLogSubscribe() (chan string, []string, func()) {
	ch := make(chan string, 16)
	appLog.mu.Lock()
	if appLog.subs == nil {
		appLog.subs = map[chan string]struct{}{}
	}
	appLog.subs[ch] = struct{}{}
	backlog := append([]string(nil), appLog.lines...)
	appLog.mu.Unlock()
	return ch, backlog, func() { appLog.mu.Lock(); delete(appLog.subs, ch); appLog.mu.Unlock() }
}

func sseLogEvent(s string) []byte {
	b, _ := json.Marshal(s)
	return []byte("event: log\ndata: " + string(b) + "\n\n")
}

// ---------------- API ----------------

func registerUserdataRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/userdata/status", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, ud.statusMap())
	})
	api.HandleFunc("POST /api/userdata/backup", func(w http.ResponseWriter, r *http.Request) {
		if st, err := ud.status(); err != nil || fmt.Sprint(st["state"]) != "ready" {
			errOut(w, 409, errors.New("GPU 未就绪，没有可备份的内容"))
			return
		} else {
			ud.init()
			ud.mu.Lock()
			same := ud.session == sessionKey(st)
			ud.mu.Unlock()
			if !same {
				ud.tick(r.Context()) // first look at this session: restore first
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		res, err := ud.backup(ctx)
		if err != nil {
			errOut(w, 500, errors.New(ud.statusMap()["error"].(string)))
			return
		}
		jsonOut(w, 200, map[string]any{"result": res, "status": ud.statusMap()})
	})
	api.HandleFunc("POST /api/userdata/open", func(w http.ResponseWriter, r *http.Request) {
		ud.init()
		d := ud.store.filesDir()
		os.MkdirAll(d, 0o755)
		openPath(d)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
}
