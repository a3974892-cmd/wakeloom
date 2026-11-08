package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeComfy is ComfyUI's userdata API over an in-memory user/default/ folder.
type fakeComfy struct {
	mu       sync.Mutex
	files    map[string][]byte
	mod      map[string]float64
	clock    float64
	v2       bool          // serve /api/v2/userdata (else only the v1 list)
	delay    time.Duration // slow downloads (stop timeout test)
	gets     []string
	puts     []string
	badPaths []string // extra listed entries with unsafe paths
	t        *testing.T
}

func newFakeComfy(t *testing.T, files map[string]string) (*fakeComfy, *udRemote) {
	f := &fakeComfy{files: map[string][]byte{}, mod: map[string]float64{}, v2: true, t: t}
	for p, c := range files {
		f.set(p, c)
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, &udRemote{base: srv.URL, hc: srv.Client()}
}

func (f *fakeComfy) set(p, c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clock++
	f.files[p], f.mod[p] = []byte(c), 1.7e9+f.clock
}

func (f *fakeComfy) del(p string) { f.mu.Lock(); delete(f.files, p); delete(f.mod, p); f.mu.Unlock() }

func (f *fakeComfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw := r.URL.EscapedPath()
	switch {
	case raw == "/api/v2/userdata" && f.v2:
		dir := r.URL.Query().Get("path")
		seen := map[string]bool{}
		out := []map[string]any{}
		for p, b := range f.files {
			rest := p
			if dir != "" {
				if !strings.HasPrefix(p, dir+"/") {
					continue
				}
				rest = strings.TrimPrefix(p, dir+"/")
			}
			name, _, sub := strings.Cut(rest, "/")
			full := strings.TrimPrefix(dir+"/"+name, "/")
			if sub {
				if !seen[full] {
					seen[full] = true
					out = append(out, map[string]any{"name": name, "path": full, "type": "directory"})
				}
				continue
			}
			out = append(out, map[string]any{"name": name, "path": full, "type": "file", "size": len(b), "modified": f.mod[p]})
		}
		if dir == "" {
			for _, bp := range f.badPaths {
				out = append(out, map[string]any{"name": "x", "path": bp, "type": "file", "size": 3, "modified": 1})
			}
		}
		json.NewEncoder(w).Encode(out)
	case raw == "/api/userdata" && r.URL.Query().Get("dir") == "." && r.URL.Query().Get("recurse") == "true":
		out := []map[string]any{}
		for p, b := range f.files {
			out = append(out, map[string]any{"path": p, "size": len(b), "modified": f.mod[p]})
		}
		json.NewEncoder(w).Encode(out)
	case strings.HasPrefix(raw, "/api/userdata/"):
		enc := strings.TrimPrefix(raw, "/api/userdata/")
		if strings.Contains(enc, "/") { // ComfyUI's {file} route only matches one segment: "/" must be %2F
			http.NotFound(w, r)
			return
		}
		p, _ := url.PathUnescape(enc)
		switch r.Method {
		case "GET":
			b, ok := f.files[p]
			if !ok {
				http.NotFound(w, r)
				return
			}
			f.gets = append(f.gets, p)
			if f.delay > 0 {
				f.mu.Unlock()
				select {
				case <-time.After(f.delay):
				case <-r.Context().Done():
				}
				f.mu.Lock()
			}
			w.Write(b)
		case "POST":
			if r.URL.Query().Get("overwrite") != "true" {
				if _, ok := f.files[p]; ok {
					http.Error(w, "exists", 409)
					return
				}
			}
			b, _ := io.ReadAll(r.Body)
			f.clock++
			f.files[p], f.mod[p] = b, 1.7e9+f.clock
			f.puts = append(f.puts, p)
			json.NewEncoder(w).Encode(map[string]string{"path": p})
		}
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeComfy) take() (gets, puts []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gets, puts = f.gets, f.puts
	f.gets, f.puts = nil, nil
	sort.Strings(gets)
	sort.Strings(puts)
	return
}

func readLocal(t *testing.T, s *udStore, p string) string {
	b, err := os.ReadFile(s.local(p))
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestUserdataListGetPutEncoding(t *testing.T) {
	f, r := newFakeComfy(t, map[string]string{"comfy.settings.json": "{}", "workflows/sub dir/a b.json": "A", "workflows/x.json": "X"})
	ctx := context.Background()
	for _, v2 := range []bool{true, false} {
		f.v2 = v2
		list, err := r.list(ctx)
		if err != nil {
			t.Fatal(v2, err)
		}
		var ps []string
		for _, x := range list {
			ps = append(ps, x.Path)
		}
		sort.Strings(ps)
		if strings.Join(ps, ",") != "comfy.settings.json,workflows/sub dir/a b.json,workflows/x.json" {
			t.Errorf("v2=%v list: %v", v2, ps)
		}
	}
	if udEscape("workflows/sub dir/a b.json") != "workflows%2Fsub%20dir%2Fa%20b.json" {
		t.Error(udEscape("workflows/sub dir/a b.json"))
	}
	if b, err := r.get(ctx, "workflows/sub dir/a b.json"); err != nil || string(b) != "A" {
		t.Fatal(string(b), err)
	}
	if err := r.put(ctx, "workflows/new/n.json", []byte("N")); err != nil || string(f.files["workflows/new/n.json"]) != "N" {
		t.Fatal(err)
	}
}

func TestUserdataBackupIncrementalAndTrash(t *testing.T) {
	f, r := newFakeComfy(t, map[string]string{"comfy.settings.json": "{}", "workflows/a.json": "A1", "workflows/b.json": "B1",
		"workflows/x.tmp": "temp", "ComfyUI-Manager/cache/big.json": "cache"})
	s := &udStore{dir: t.TempDir()}
	ctx := context.Background()
	res, err := s.backup(ctx, r, nil, true)
	if err != nil || res.Downloaded != 3 || res.Files != 3 {
		t.Fatalf("first backup: %+v %v", res, err)
	}
	if readLocal(t, s, "workflows/a.json") != "A1" || readLocal(t, s, "workflows/x.tmp") != "<missing>" {
		t.Error("files not saved / temp file saved")
	}
	f.take()
	// nothing changed: nothing downloaded
	if res, _ := s.backup(ctx, r, nil, true); res.Downloaded != 0 {
		t.Errorf("unchanged backup downloaded %d", res.Downloaded)
	}
	// one change, one deletion
	f.set("workflows/a.json", "A2")
	f.del("workflows/b.json")
	f.take()
	res, err = s.backup(ctx, r, nil, true)
	gets, _ := f.take()
	if err != nil || res.Downloaded != 1 || res.Trashed != 1 || strings.Join(gets, ",") != "workflows/a.json" {
		t.Fatalf("incremental: %+v %v gets=%v", res, err, gets)
	}
	if readLocal(t, s, "workflows/a.json") != "A2" || readLocal(t, s, "workflows/b.json") != "<missing>" {
		t.Error("local copy not updated / deleted file still there")
	}
	trashed := filepath.Join(s.trashDir(time.Now()), "workflows", "b.json")
	if b, err := os.ReadFile(trashed); err != nil || string(b) != "B1" {
		t.Errorf("deleted file not in trash: %v", err)
	}
	m := s.load()
	if _, ok := m.Files["workflows/b.json"]; ok || m.LastBackup == 0 || m.Files["workflows/a.json"].SHA256 != shaHex([]byte("A2")) {
		t.Errorf("manifest: %+v", m)
	}
	// without allowDelete (session not restored) nothing is trashed
	f.del("workflows/a.json")
	if res, _ := s.backup(ctx, r, nil, false); res.Trashed != 0 || readLocal(t, s, "workflows/a.json") != "A2" {
		t.Error("deleted without a restore")
	}
	// trash older than 30 days is purged
	old := s.trashDir(time.Now().AddDate(0, 0, -31))
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "x.json"), []byte("x"), 0o644)
	s.backup(ctx, r, nil, false)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old trash kept")
	}
}

func TestUserdataRestoreOriginRule(t *testing.T) {
	ctx := context.Background()
	s := &udStore{dir: t.TempDir()}
	// boot 1: the VM ships official.json v1 and default settings; the user edits settings and adds a workflow
	f, r := newFakeComfy(t, map[string]string{"workflows/official.json": "v1", "comfy.settings.json": "default"})
	n, fresh, err := s.restore(ctx, r, "boot1")
	if err != nil || n != 0 {
		t.Fatal(n, err)
	}
	f.set("comfy.settings.json", "mine")
	f.set("workflows/my.json", "M")
	if _, err := s.backup(ctx, r, fresh, true); err != nil {
		t.Fatal(err)
	}
	m := s.load()
	if m.Files["workflows/official.json"].Origin != shaHex([]byte("v1")) || m.Files["comfy.settings.json"].Origin != "" {
		t.Errorf("origins after boot 1: %+v %+v", m.Files["workflows/official.json"], m.Files["comfy.settings.json"])
	}
	// boot 2: a new VM ships official.json v2
	f2, r2 := newFakeComfy(t, map[string]string{"workflows/official.json": "v2", "comfy.settings.json": "default"})
	n, fresh, err = s.restore(ctx, r2, "boot2")
	_, puts := f2.take()
	if err != nil || n != 2 || strings.Join(puts, ",") != "comfy.settings.json,workflows/my.json" {
		t.Fatalf("restore: n=%d puts=%v err=%v", n, puts, err)
	}
	if string(f2.files["workflows/official.json"]) != "v2" || string(f2.files["comfy.settings.json"]) != "mine" {
		t.Error("untouched official workflow overwritten or user settings not restored")
	}
	if s.load().Session != "boot2" {
		t.Error("session not recorded")
	}
	// the next backup picks up v2 (and does not re-download what was just uploaded)
	res, _ := s.backup(ctx, r2, fresh, true)
	gets, _ := f2.take()
	if res.Downloaded != 1 || strings.Join(gets, ",") != "workflows/official.json" || readLocal(t, s, "workflows/official.json") != "v2" {
		t.Errorf("after restore: %+v gets=%v", res, gets)
	}
	// boot 3: the user had edited official.json -> local wins
	f2.set("workflows/official.json", "v2-edited")
	s.backup(ctx, r2, fresh, true)
	f3, r3 := newFakeComfy(t, map[string]string{"workflows/official.json": "v3"})
	if _, _, err := s.restore(ctx, r3, "boot3"); err != nil || string(f3.files["workflows/official.json"]) != "v2-edited" {
		t.Errorf("edited file not restored: %s %v", f3.files["workflows/official.json"], err)
	}
}

func TestUserdataUnsafeAndLimits(t *testing.T) {
	old1, old2 := udMaxFile, udMaxTotal
	udMaxFile, udMaxTotal = 100, 250
	t.Cleanup(func() { udMaxFile, udMaxTotal = old1, old2 })
	f, r := newFakeComfy(t, map[string]string{"a.json": strings.Repeat("a", 90), "b.json": strings.Repeat("b", 90),
		"big.json": strings.Repeat("x", 101), "c.json": strings.Repeat("c", 90)})
	f.badPaths = []string{"../evil.json", "/etc/passwd", "C:/Windows/x.json", "workflows/../../x.json", `a\..\b.json`, "con.json"}
	s := &udStore{dir: t.TempDir()}
	res, err := s.backup(context.Background(), r, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	gets, _ := f.take()
	if strings.Join(gets, ",") != "a.json,b.json" {
		t.Errorf("downloaded %v", gets)
	}
	joined := strings.Join(res.Skipped, "\n")
	for _, want := range []string{"big.json", "总量超过", "c.json", "../evil.json", "C:/Windows/x.json", "con.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("skipped list lacks %q:\n%s", want, joined)
		}
	}
	// nothing escaped the backup folder
	filepath.Walk(filepath.Dir(s.dir), func(p string, info os.FileInfo, err error) error {
		if err == nil && strings.Contains(p, "evil") {
			t.Error("unsafe path written:", p)
		}
		return nil
	})
	for _, p := range []string{"..", "a/../b", "/x", `\x`, "C:x", "a//b", "a/./b", "a\\b", "aux", "x:y", "a/b. ", ""} {
		if udValidPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
		if _, err := s.inside(p); err == nil {
			t.Errorf("inside(%q) accepted", p)
		}
	}
	for _, p := range []string{"comfy.settings.json", "workflows/子目录/工作流 1.json", "subgraphs/a-b_c.json"} {
		if err := udValidPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
}

// syncer wired to a fake gate status + fake ComfyUI
func testSyncer(t *testing.T, r *udRemote) (*udSyncer, *[]string, *string) {
	state := "ready"
	var logs []string
	u := &udSyncer{store: &udStore{dir: t.TempDir()}, remote: func() *udRemote { return r },
		status: func() (map[string]any, error) {
			return map[string]any{"state": state, "session_started": 123.0, "profile": "p"}, nil
		},
		log: func(s string) { logs = append(logs, s) }}
	return u, &logs, &state
}

func TestUserdataSyncerStopTimeout(t *testing.T) {
	f, r := newFakeComfy(t, map[string]string{"workflows/a.json": "A"})
	u, logs, state := testSyncer(t, r)
	ctx := context.Background()
	// before restore: no backup (would overwrite the backup with VM defaults)
	if _, err := u.backup(ctx); err == nil {
		t.Error("backup before restore allowed")
	}
	u.tick(ctx) // restore
	if len(*logs) != 1 || !strings.Contains((*logs)[0], "已恢复 0 个") {
		t.Fatalf("log: %v", *logs)
	}
	u.tick(ctx) // first backup is due right after the restore
	if st := u.statusMap(); st["files"] != 1 || st["error"] != "" {
		t.Fatalf("status: %v", st)
	}
	// slow ComfyUI: the stop backup gives up after udStopWait and warns
	old := udStopWait
	udStopWait = 300 * time.Millisecond
	t.Cleanup(func() { udStopWait = old })
	f.set("workflows/a.json", "A2")
	f.mu.Lock()
	f.delay = 5 * time.Second
	f.mu.Unlock()
	t0 := time.Now()
	warn := u.beforeStop()
	if time.Since(t0) > 3*time.Second || !strings.Contains(warn, "部分工作流可能未备份") {
		t.Errorf("stop backup: %q after %v", warn, time.Since(t0))
	}
	// not ready / disabled: nothing to do
	*state = "sleeping"
	if u.beforeStop() != "" {
		t.Error("warning while asleep")
	}
}

func TestSSEInjectsAppLog(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: log\ndata: \"gate line\"\n\n"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer up.Close()
	cfgMu.Lock()
	oldCfg := cfg
	cfg = Config{GateURL: up.URL, Mode: "gateway"}
	cfgMu.Unlock()
	defer func() { cfgMu.Lock(); cfg = oldCfg; cfgMu.Unlock() }()
	appLog.mu.Lock()
	appLog.lines = []string{"WB_USERDATA earlier"}
	appLog.mu.Unlock()
	srv := httptest.NewServer(http.HandlerFunc(sseProxy))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		udLog("WB_USERDATA 已恢复 3 个工作流/设置文件")
	}()
	var got strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(got.String(), "已恢复 3") {
		n, err := resp.Body.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Log(err)
			}
			break
		}
	}
	s := got.String()
	if !strings.Contains(s, "gate line") || !strings.Contains(s, "earlier") || !strings.Contains(s, "已恢复 3") ||
		strings.Index(s, "gate line") > strings.Index(s, "earlier") {
		t.Errorf("stream:\n%s", s)
	}
}
