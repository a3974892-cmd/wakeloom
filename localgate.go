package main

// Local mode: a gate that runs inside the workbench itself. It speaks the same HTTP API as
// comfy-gate (/_gate/status, /_gate/events, /_gate/wake, …, plus a reverse proxy to ComfyUI),
// so the rest of the workbench works unchanged; only baseURL() points here instead of the VPS.
// It drives Colab directly (colab.go), ships the comfy-colab-kit setup files in a tarball, and
// reaches ComfyUI through a cloudflared quick tunnel guarded by a per-session key.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed kit
var kitFS embed.FS

var stateOrder = map[string]int{"sleeping": 0, "allocating": 1, "installing": 2, "downloading": 3, "booting": 4, "smoke_testing": 5, "ready": 6}
var stageToState = map[string]string{"bootstrap": "installing", "deps": "installing", "download": "downloading",
	"install_workflows": "downloading", "comfy_boot": "booting", "smoke": "smoke_testing"}

type LocalGate struct {
	mu             sync.Mutex
	key            string
	port           int
	state          string
	reason         string
	since          float64
	sessionStarted float64
	profile        string
	accel          string
	progress       map[string]any
	logs           []string
	subs           map[chan string]struct{}
	asg            *Assignment
	rt             *Runtime
	proxyAt        time.Time
	tunnel         string
	wbKey          string
	ccu            *CCUInfo
	ccuAt          time.Time
	extendsUsed    int
	lastActivity   time.Time
	wsOpen         int
	libInstalled   []string
	fetching       []string
	fetchErr       string
	cancel         context.CancelFunc
	execMu         sync.Mutex
	hbFails        int
	rp             *httputil.ReverseProxy
	stats          *NVSample
	statsWanted    time.Time // the console's load panel asked recently -> sample often
}

// bootstrapDriver is the VM-side entry point (overridden in tests).
var bootstrapDriver = "/content/kit/driver.py"

var lg = &LocalGate{state: "sleeping", progress: map[string]any{}, subs: map[chan string]struct{}{}}

func nowF() float64 { return float64(time.Now().UnixNano()) / 1e9 }

func localMode() bool { return getConfig().Mode == "local" }

func startLocalGate() {
	if v := os.Getenv("CKWB_COLAB_DOMAIN"); v != "" { // testing only
		colabDomain = v
	}
	if v := os.Getenv("CKWB_DRIVER"); v != "" {
		bootstrapDriver = v
	}
	b := make([]byte, 16)
	rand.Read(b)
	lg.key = hex.EncodeToString(b)
	lg.since = nowF()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Println("local gate:", err)
		return
	}
	lg.port = ln.Addr().(*net.TCPAddr).Port
	lg.rp = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			lg.mu.Lock()
			t, k := lg.tunnel, lg.wbKey
			lg.mu.Unlock()
			u, _ := url.Parse(t)
			r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
			r.Header.Set("X-WB-Key", k)
			r.Header.Del("Authorization")
			r.Header.Del("Cookie")
			r.Header.Del("Origin") // ComfyUI rejects cross-origin websockets
			r.Header.Del(udSyncHeader)
			q := r.URL.Query()
			if q.Has("wbk") {
				q.Del("wbk")
				r.URL.RawQuery = q.Encode()
			}
		},
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			jsonOut(w, 502, map[string]string{"error": "backend_unreachable", "message": "连不上 Colab 上的 ComfyUI：" + err.Error()})
		},
	}
	go http.Serve(ln, http.HandlerFunc(lg.serve))
	lg.restore()
	go lg.loop()
}

func (g *LocalGate) baseURL() string { return fmt.Sprintf("http://127.0.0.1:%d", g.port) }

// ---- events ----

func (g *LocalGate) publish(ev string, v any) {
	b, _ := json.Marshal(v)
	msg := "event: " + ev + "\ndata: " + string(b) + "\n\n"
	g.mu.Lock()
	for ch := range g.subs {
		select {
		case ch <- msg:
		default:
		}
	}
	g.mu.Unlock()
}

func (g *LocalGate) logLine(s string) {
	g.mu.Lock()
	g.logs = append(g.logs, s)
	if len(g.logs) > 3000 {
		g.logs = g.logs[len(g.logs)-2000:]
	}
	g.mu.Unlock()
	g.publish("log", s)
}

func (g *LocalGate) setState(s, reason string) {
	g.mu.Lock()
	g.state, g.reason, g.since = s, reason, nowF()
	if s == "sleeping" {
		g.sessionStarted = 0
	}
	g.mu.Unlock()
	g.persist()
	g.publish("state", g.status())
}

// ---- persistence (re-adopt a running VM after the app restarts) ----

type lgPersist struct {
	Endpoint       string    `json:"endpoint"`
	Accel          string    `json:"accel"`
	Profile        string    `json:"profile"`
	SessionStarted float64   `json:"session_started"`
	Tunnel         string    `json:"tunnel"`
	WBKey          string    `json:"wb_key"`
	Extends        int       `json:"extends"`
	LibInstalled   []string  `json:"lib_installed"`
	Proxy          ProxyInfo `json:"proxy"`
}

func lgPath() string { return filepath.Join(dataDir, "local_session.json") }

func (g *LocalGate) persist() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.asg == nil || g.state == "sleeping" {
		os.Remove(lgPath())
		return
	}
	writeJSON(lgPath(), lgPersist{Endpoint: g.asg.Endpoint, Accel: g.accel, Profile: g.profile, SessionStarted: g.sessionStarted,
		Tunnel: g.tunnel, WBKey: g.wbKey, Extends: g.extendsUsed, LibInstalled: g.libInstalled, Proxy: g.asg.Proxy})
}

func (g *LocalGate) restore() {
	b, err := os.ReadFile(lgPath())
	if err != nil {
		return
	}
	var p lgPersist
	if json.Unmarshal(b, &p) != nil || p.Endpoint == "" || p.Tunnel == "" {
		os.Remove(lgPath())
		return
	}
	go func() {
		if ok, _ := googleSignedIn(); !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		a := &Assignment{Endpoint: p.Endpoint, Accelerator: p.Accel, Proxy: p.Proxy}
		if err := refreshProxy(ctx, a); err != nil {
			os.Remove(lgPath())
			return
		}
		g.mu.Lock()
		g.asg, g.rt, g.proxyAt = a, &Runtime{A: a}, time.Now()
		g.accel, g.profile, g.sessionStarted, g.tunnel, g.wbKey = p.Accel, p.Profile, p.SessionStarted, p.Tunnel, p.WBKey
		g.extendsUsed, g.libInstalled, g.lastActivity = p.Extends, p.LibInstalled, time.Now()
		g.rt.OnReconnect = g.refreshToken
		g.mu.Unlock()
		g.logLine("重新接管运行中的 Colab GPU（" + p.Accel + "）")
		g.setState("ready", "re-adopted")
	}()
}

// ---- status ----

func (g *LocalGate) maxSession() time.Duration {
	c := getConfig()
	h := c.LocalMaxHours
	if h <= 0 {
		h = 6
	}
	return time.Duration(float64(h)*float64(time.Hour)) + time.Duration(g.extendsUsed)*time.Hour
}

func (g *LocalGate) status() map[string]any {
	ok, email := googleSignedIn()
	g.mu.Lock()
	defer g.mu.Unlock()
	st := map[string]any{
		"mode": "local", "state": g.state, "reason": g.reason, "since": g.since, "profile": nil,
		"busy":     stateOrder[g.state] > 0 && g.state != "ready" || g.state == "stopping",
		"progress": g.progress, "warn": "", "can_extend": g.extendsUsed < 2, "extends_used": g.extendsUsed,
		"clients": g.wsOpen, "you": map[string]any{"email": email, "owner": true, "signed_in": ok},
		"library":     map[string]any{"installed": g.libInstalled},
		"models":      map[string]any{"fetching": g.fetching, "fetch_error": g.fetchErr, "on_demand": []string{}},
		"accelerator": g.accel,
	}
	if g.profile != "" {
		st["profile"] = g.profile
	}
	if g.sessionStarted > 0 {
		st["session_started"] = g.sessionStarted
		if g.state == "ready" {
			rem := g.maxSession().Seconds() - (nowF() - g.sessionStarted)
			st["remaining_s"] = rem
			if rem < 15*60 {
				st["warn"] = "session_ending"
			}
		}
	}
	if g.ccu != nil {
		st["cu"] = map[string]any{"balance": g.ccu.Balance, "per_hour": g.ccu.RatePerHour}
	}
	return st
}

// ---- HTTP ----

func (g *LocalGate) authed(r *http.Request) bool {
	if r.Header.Get("Authorization") == "Bearer "+g.key {
		return true
	}
	if c, err := r.Cookie("wbk"); err == nil && c.Value == g.key {
		return true
	}
	return false
}

func (g *LocalGate) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("wbk") == g.key { // browser hand-off: set cookie, drop the key from the URL
		http.SetCookie(w, &http.Cookie{Name: "wbk", Value: g.key, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		q := r.URL.Query()
		q.Del("wbk")
		r.URL.RawQuery = q.Encode()
		http.Redirect(w, r, r.URL.String(), http.StatusFound)
		return
	}
	if !g.authed(r) {
		jsonOut(w, 401, map[string]string{"error": "unauthorized", "message": "local gate key required"})
		return
	}
	p := r.URL.Path
	switch {
	case p == "/_gate/status":
		g.refreshCCU(false)
		jsonOut(w, 200, g.status())
	case p == "/_gate/events":
		g.events(w, r)
	case p == "/_gate/profiles":
		jsonOut(w, 200, localProfilesSummary())
	case p == "/_gate/custom-profiles" || strings.HasPrefix(p, "/_gate/custom-profiles/"):
		g.customProfiles(w, r)
	case p == "/_gate/wake" && r.Method == "POST":
		g.wakeHandler(w, r)
	case p == "/_gate/stop" && r.Method == "POST":
		go g.stop("已手动停止")
		jsonOut(w, 200, map[string]string{"state": "stopping"})
	case p == "/_gate/extend" && r.Method == "POST":
		g.mu.Lock()
		okExt := g.extendsUsed < 2 && g.state == "ready"
		if okExt {
			g.extendsUsed++
		}
		g.mu.Unlock()
		if !okExt {
			jsonOut(w, 409, map[string]string{"error": "extend_limit", "message": "不能再延长了"})
			return
		}
		g.persist()
		jsonOut(w, 200, g.status())
	case p == "/_gate/library" && r.Method == "GET":
		g.mu.Lock()
		out := map[string]any{"items": libLoad(), "installed": nonNil(g.libInstalled), "fetching": nonNil(g.fetching), "fetch_error": g.fetchErr}
		g.mu.Unlock()
		jsonOut(w, 200, out)
	case p == "/_gate/library" && r.Method == "POST":
		var in LibItem
		json.NewDecoder(r.Body).Decode(&in)
		it, err := libNormalize(in)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": "bad_request", "message": err.Error()})
			return
		}
		items := libLoad()
		out := []LibItem{}
		for _, x := range items {
			if x.ID != it.ID {
				out = append(out, x)
			}
		}
		libSave(append(out, it))
		jsonOut(w, 200, it)
	case strings.HasPrefix(p, "/_gate/library/") && r.Method == "DELETE":
		id := strings.TrimPrefix(p, "/_gate/library/")
		items, out := libLoad(), []LibItem{}
		for _, x := range items {
			if x.ID != id {
				out = append(out, x)
			}
		}
		libSave(out)
		jsonOut(w, 200, map[string]string{"deleted": id})
	case p == "/_gate/library/install" && r.Method == "POST":
		var in struct {
			IDs []string `json:"ids"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if why := g.installLibrary(in.IDs); why != "" {
			jsonOut(w, 409, map[string]string{"error": why, "message": "install refused: " + why})
			return
		}
		jsonOut(w, 202, map[string]any{"installing": in.IDs})
	case strings.HasPrefix(p, "/_gate/"):
		jsonOut(w, 404, map[string]string{"error": "not_found"})
	default:
		g.mu.Lock()
		ready, tunnel := g.state == "ready", g.tunnel
		// the load panel's polling and the user-data backup (userdata.go) are not "use"
		if p != "/system_stats" && p != "/queue" && r.Header.Get(udSyncHeader) == "" {
			g.lastActivity = time.Now()
		}
		g.mu.Unlock()
		if !ready || tunnel == "" {
			jsonOut(w, 503, map[string]string{"error": "asleep", "message": "GPU 未就绪"})
			return
		}
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			g.mu.Lock()
			g.wsOpen++
			g.mu.Unlock()
			defer func() { g.mu.Lock(); g.wsOpen--; g.lastActivity = time.Now(); g.mu.Unlock() }()
		}
		g.rp.ServeHTTP(w, r)
	}
}

func (g *LocalGate) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	ch := make(chan string, 256)
	g.mu.Lock()
	backlog := append([]string(nil), g.logs...)
	g.subs[ch] = struct{}{}
	g.mu.Unlock()
	defer func() { g.mu.Lock(); delete(g.subs, ch); g.mu.Unlock() }()
	if len(backlog) > 400 {
		backlog = backlog[len(backlog)-400:]
	}
	for _, l := range backlog {
		b, _ := json.Marshal(l)
		fmt.Fprintf(w, "event: log\ndata: %s\n\n", b)
	}
	b, _ := json.Marshal(g.status())
	fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
	if fl != nil {
		fl.Flush()
	}
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case m := <-ch:
			w.Write([]byte(m))
		case <-tick.C:
			w.Write([]byte(": ping\n\n"))
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// ---- CU ----

func (g *LocalGate) refreshCCU(force bool) {
	g.mu.Lock()
	every := 5 * time.Minute
	if g.state != "sleeping" {
		every = time.Minute
	}
	due := force || time.Since(g.ccuAt) > every
	if due {
		g.ccuAt = time.Now()
	}
	g.mu.Unlock()
	if !due {
		return
	}
	if ok, _ := googleSignedIn(); !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if c, err := colabCCU(ctx); err == nil {
			g.mu.Lock()
			g.ccu = &c
			g.mu.Unlock()
		}
	}()
}

// ---- wake / boot ----

func (g *LocalGate) wakeHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile     string   `json:"profile"`
		Workflows   []string `json:"workflows"`
		Extras      []string `json:"extras"`
		Accelerator string   `json:"accelerator"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	if ok, _ := googleSignedIn(); !ok {
		jsonOut(w, 403, map[string]string{"error": "not_signed_in", "message": "本地模式还没登录 Google（设置 → 本地模式）"})
		return
	}
	g.mu.Lock()
	st := g.state
	g.mu.Unlock()
	if st != "sleeping" && st != "failed" {
		jsonOut(w, 200, map[string]any{"joined": true, "state": st, "profile": g.profile})
		return
	}
	profs := localProfiles()
	if in.Profile == "" {
		in.Profile = getConfig().LocalProfile
	}
	pr, ok := profs[in.Profile]
	if !ok {
		ids := sortedProfileIDs(profs)
		if in.Profile != "" || len(ids) == 0 {
			jsonOut(w, 404, map[string]string{"error": "unknown_profile", "message": "找不到配置 " + in.Profile})
			return
		}
		pr = profs[ids[0]]
	}
	accel := strings.ToUpper(strings.TrimSpace(firstNonEmpty(in.Accelerator, firstNonEmpty(getConfig().LocalAccelerator, pr.Accelerator))))
	if accel == "" {
		accel = "L4"
	}
	// selected extras = explicitly picked + library items flagged auto_wake
	pick := map[string]bool{}
	for _, id := range in.Extras {
		pick[id] = true
	}
	extras := []LibItem{}
	for _, it := range libLoad() {
		if pick[it.ID] || it.AutoWake {
			extras = append(extras, it)
		}
	}
	k := make([]byte, 24)
	rand.Read(k)
	ctx, cancel := context.WithCancel(context.Background())
	g.mu.Lock()
	g.cancel = cancel
	g.profile, g.accel, g.sessionStarted, g.extendsUsed = pr.ID, accel, nowF(), 0
	g.progress, g.tunnel, g.wbKey, g.libInstalled, g.fetching, g.fetchErr = map[string]any{}, "", hex.EncodeToString(k), nil, nil, ""
	g.logs = nil
	g.mu.Unlock()
	g.setState("allocating", "wake")
	go g.boot(ctx, pr, in.Workflows, extras)
	jsonOut(w, 202, map[string]any{"joined": false, "state": "allocating", "profile": pr.ID})
}

func (g *LocalGate) boot(ctx context.Context, pr *LocalProfile, workflows []string, extras []LibItem) {
	fail := func(err error) {
		g.logLine("✗ " + err.Error())
		g.setState("failed", truncate(err.Error(), 300))
		g.mu.Lock()
		a := g.asg
		g.asg, g.rt = nil, nil
		g.mu.Unlock()
		if a != nil { // don't keep paying for a VM that failed to boot
			c2, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			if colabUnassign(c2, a.Endpoint) == nil {
				g.logLine("已释放 GPU，避免空跑扣费")
			}
		}
		os.Remove(lgPath())
	}
	g.logLine(fmt.Sprintf("申请 Colab %s GPU …", g.accel))
	a, err := colabAssign(ctx, g.accel)
	if err != nil {
		fail(err)
		return
	}
	rt := &Runtime{A: a}
	rt.OnReconnect = g.refreshToken
	g.mu.Lock()
	g.asg, g.rt, g.proxyAt = a, rt, time.Now()
	g.mu.Unlock()
	g.persist()
	g.logLine("✓ 已分配 " + g.accel)
	if workflows == nil {
		workflows = []string{}
	}
	tgz, err := buildKitTarball(pr, map[string]any{"profile": pr.ID, "workflows": workflows, "extras": extras,
		"settings": map[string]string{"COLAB_ACCELERATOR": g.accel}})
	if err != nil {
		fail(err)
		return
	}
	if err := rt.StartKernel(ctx); err != nil {
		fail(err)
		return
	}
	if err := rt.Upload(ctx, tgz, "/content/kit.tgz"); err != nil {
		fail(err)
		return
	}
	g.setState("installing", "setup started")
	code := "import os, subprocess, runpy\n" +
		"subprocess.run(['bash','-lc','rm -rf /content/kit && mkdir -p /content/kit && tar -xzf /content/kit.tgz -C /content/kit'], check=True)\n" +
		fmt.Sprintf("os.environ['WB_KEY']=%q\n", g.wbKey) +
		hfTokenEnv() +
		fmt.Sprintf("_wb = runpy.run_path(%q, run_name='__main__'); del _wb\n", bootstrapDriver)
	g.execMu.Lock()
	err = rt.Exec(ctx, code, g.handleLine)
	g.execMu.Unlock()
	g.mu.Lock()
	st, tunnel := g.state, g.tunnel
	g.mu.Unlock()
	if ctx.Err() != nil {
		return // stopped meanwhile
	}
	if err != nil && st != "ready" {
		fail(fmt.Errorf("开机失败：%v", err))
		return
	}
	if st != "ready" {
		fail(errors.New("开机脚本结束但没有就绪（KIT_READY）"))
		return
	}
	if tunnel == "" {
		fail(errors.New("ComfyUI 起来了，但隧道地址没拿到"))
		return
	}
	g.mu.Lock()
	g.lastActivity = time.Now()
	g.mu.Unlock()
	g.persist()
}

func (g *LocalGate) handleLine(line string) {
	if strings.HasPrefix(line, "KIT_KEEPALIVE") {
		return
	}
	g.logLine(line)
	parts := strings.SplitN(line, " ", 4)
	switch {
	case strings.HasPrefix(line, "KIT_STAGE ") && len(parts) >= 3:
		g.mu.Lock()
		g.progress["stage"] = parts[1]
		g.mu.Unlock()
		if parts[2] == "fail" {
			g.setState("failed", "stage "+parts[1]+" failed")
		} else if t, ok := stageToState[parts[1]]; ok && parts[2] == "start" {
			g.mu.Lock()
			adv := stateOrder[t] > stateOrder[g.state]
			g.mu.Unlock()
			if adv {
				g.setState(t, "stage "+parts[1])
			}
		}
	case strings.HasPrefix(line, "KIT_PROGRESS ") && len(parts) >= 3:
		var v any
		if json.Unmarshal([]byte(strings.SplitN(line, " ", 3)[2]), &v) == nil {
			g.mu.Lock()
			if parts[1] == "download" {
				// the UI expects done_gb/total_gb/files_*/gbps like the gate reports
				if m, ok := v.(map[string]any); ok {
					if bd, ok := m["bytes_done"].(float64); ok {
						m["done_gb"] = bd / 1e9
					}
					if bt, ok := m["bytes_total"].(float64); ok {
						m["total_gb"] = bt / 1e9
					}
					if bps, ok := m["bytes_per_s"].(float64); ok {
						m["gbps"] = bps * 8 / 1e9
					}
				}
			}
			g.progress[parts[1]] = v
			p := g.progress
			g.mu.Unlock()
			g.publish("progress", p)
		}
	case strings.HasPrefix(line, "KIT_TUNNEL "):
		var d struct {
			URL string `json:"url"`
		}
		json.Unmarshal([]byte(strings.TrimPrefix(line, "KIT_TUNNEL ")), &d)
		if validTunnel(d.URL) {
			g.mu.Lock()
			g.tunnel = d.URL
			g.mu.Unlock()
			g.persist()
		}
	case strings.HasPrefix(line, "KIT_READY"):
		g.setState("ready", "ComfyUI ready")
	case strings.HasPrefix(line, "KIT_FAILED"):
		g.setState("failed", strings.TrimSpace(strings.TrimPrefix(line, "KIT_FAILED")))
	case strings.HasPrefix(line, "KIT_FETCHED"):
		var d struct {
			Library []string `json:"library"`
		}
		json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "KIT_FETCHED"))), &d)
		g.mu.Lock()
		seen := map[string]bool{}
		for _, x := range append(g.libInstalled, d.Library...) {
			seen[x] = true
		}
		g.libInstalled = nil
		for x := range seen {
			g.libInstalled = append(g.libInstalled, x)
		}
		sort.Strings(g.libInstalled)
		g.mu.Unlock()
		g.persist()
	case strings.HasPrefix(line, "KIT_FETCH_FAILED"):
		var d struct {
			Error string `json:"error"`
		}
		json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "KIT_FETCH_FAILED"))), &d)
		g.mu.Lock()
		g.fetchErr = firstNonEmpty(d.Error, "安装失败")
		g.mu.Unlock()
	}
}

func (g *LocalGate) refreshToken() {
	g.mu.Lock()
	a := g.asg
	g.mu.Unlock()
	if a == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if refreshProxy(ctx, a) == nil {
		g.mu.Lock()
		g.proxyAt = time.Now()
		g.mu.Unlock()
		g.persist()
	}
}

func (g *LocalGate) installLibrary(ids []string) string {
	g.mu.Lock()
	ready, rt, busy, prof := g.state == "ready", g.rt, len(g.fetching) > 0, g.profile
	g.mu.Unlock()
	if !ready || rt == nil {
		return "not_ready"
	}
	if busy {
		return "fetch_in_progress"
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var items []LibItem
	for _, it := range libLoad() {
		if want[it.ID] {
			items = append(items, it)
		}
	}
	if len(items) == 0 {
		return "unknown_item"
	}
	g.mu.Lock()
	g.fetching, g.fetchErr = nil, ""
	for _, it := range items {
		g.fetching = append(g.fetching, "lib:"+it.ID)
	}
	g.mu.Unlock()
	go func() {
		defer func() { g.mu.Lock(); g.fetching = nil; g.mu.Unlock(); g.publish("state", g.status()) }()
		req, _ := json.Marshal(map[string]any{"profile": prof, "install": items})
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
		defer cancel()
		if err := rt.Upload(ctx, req, "/content/kit/request.json"); err != nil {
			g.mu.Lock()
			g.fetchErr = err.Error()
			g.mu.Unlock()
			return
		}
		g.execMu.Lock()
		err := rt.Exec(ctx, fmt.Sprintf("import runpy\n_wb = runpy.run_path(%q, run_name='__main__'); del _wb\n", bootstrapDriver), g.handleLine)
		g.execMu.Unlock()
		if err != nil {
			g.mu.Lock()
			g.fetchErr = err.Error()
			g.mu.Unlock()
		}
	}()
	return ""
}

// ---- stop / housekeeping ----

func (g *LocalGate) stop(reason string) {
	g.mu.Lock()
	a, cancel, st := g.asg, g.cancel, g.state
	g.asg, g.rt, g.tunnel = nil, nil, ""
	g.mu.Unlock()
	if st == "sleeping" {
		return
	}
	if cancel != nil {
		cancel()
	}
	g.setState("stopping", reason)
	if a != nil {
		ctx, c2 := context.WithTimeout(context.Background(), 60*time.Second)
		defer c2()
		if err := colabUnassign(ctx, a.Endpoint); err != nil {
			g.logLine("⚠ 释放 GPU 失败：" + err.Error() + "（请到 Colab 网页手动断开运行时）")
		} else {
			g.logLine("✓ 已释放 GPU")
		}
	}
	g.mu.Lock()
	g.stats = nil
	g.mu.Unlock()
	g.setState("sleeping", reason)
	g.refreshCCU(true)
}

// loop: heartbeat (Colab reclaims idle runtimes; the kit watchdog wants a heartbeat file),
// proxy-token refresh, tunnel URL refresh and the auto-stop rules.
func (g *LocalGate) loop() {
	var lastBeat time.Time
	for range time.Tick(5 * time.Second) {
		g.mu.Lock()
		ready, rt, started, last, ws, proxyAt, wanted := g.state == "ready", g.rt, g.sessionStarted, g.lastActivity, g.wsOpen, g.proxyAt, g.statsWanted
		var expIn int
		if g.asg != nil {
			expIn = g.asg.Proxy.ExpiresIn
		}
		g.mu.Unlock()
		if !ready || rt == nil {
			continue
		}
		if time.Since(wanted) > 20*time.Second && time.Since(lastBeat) < 30*time.Second {
			continue // nobody is watching the load panel: heartbeat every 30 s is enough
		}
		lastBeat = time.Now()
		if started > 0 && nowF()-started > g.maxSession().Seconds() {
			ud.beforeStop()
			g.stop("达到最长会话时长，自动停机")
			continue
		}
		idle := time.Duration(getConfig().LocalIdleMin) * time.Minute
		if idle <= 0 {
			idle = 30 * time.Minute
		}
		if ws == 0 && store.pending() == 0 && time.Since(last) > idle {
			ud.beforeStop()
			g.stop(fmt.Sprintf("闲置 %d 分钟，自动停机", int(idle.Minutes())))
			continue
		}
		if expIn > 0 && time.Since(proxyAt) > time.Duration(expIn-300)*time.Second {
			g.refreshToken()
		}
		if !g.execMu.TryLock() { // a fetch is running; it keeps the kernel busy anyway
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		tunnel, err := g.sampleStats(ctx, rt)
		cancel()
		g.execMu.Unlock()
		if err != nil {
			g.mu.Lock()
			g.hbFails++
			n := g.hbFails
			g.mu.Unlock()
			if n >= 6 {
				c2, cc := context.WithTimeout(context.Background(), 20*time.Second)
				list, lerr := colabList(c2)
				cc()
				gone := lerr == nil
				for _, x := range list {
					if g.asg != nil && x.Endpoint == g.asg.Endpoint {
						gone = false
					}
				}
				if gone {
					g.mu.Lock()
					g.asg, g.rt, g.tunnel = nil, nil, ""
					g.mu.Unlock()
					g.setState("sleeping", "Colab 运行时已被回收")
				}
			}
			continue
		}
		g.mu.Lock()
		g.hbFails = 0
		changed := tunnel != "" && tunnel != g.tunnel
		if changed {
			g.tunnel = tunnel
		}
		g.mu.Unlock()
		if changed {
			g.logLine("隧道地址已更新")
			g.persist()
		}
	}
}

// ---- profiles ----

type LocalProfile struct {
	ID          string `yaml:"id"`
	Title       string `yaml:"title"`
	Accelerator string `yaml:"accelerator"`
	AppDefault  string `yaml:"app_default"`
	Models      []struct {
		ID     string  `yaml:"id"`
		Dest   string  `yaml:"dest"`
		SizeGB float64 `yaml:"size_gb"`
	} `yaml:"models"`
	AI        map[string]any `yaml:"ai"`
	Workflows []struct {
		ID       string   `yaml:"id"`
		Title    string   `yaml:"title"`
		Optional bool     `yaml:"optional"`
		Prefetch bool     `yaml:"prefetch"`
		Models   []string `yaml:"models"`
	} `yaml:"workflows"`
	Smoke *struct {
		Models []string `yaml:"models"`
	} `yaml:"smoke"`
	fsys    fs.FS  // where the profile folder lives (embedded or on disk)
	dir     string // folder inside fsys
	builtin bool
}

func userProfilesDir() string { return filepath.Join(dataDir, "profiles") }

func localProfiles() map[string]*LocalProfile {
	out := map[string]*LocalProfile{}
	load := func(fsys fs.FS, dir string, builtin bool) {
		b, err := fs.ReadFile(fsys, path.Join(dir, "profile.yaml"))
		if err != nil {
			return
		}
		var p LocalProfile
		if yaml.Unmarshal(b, &p) != nil || p.ID == "" {
			return
		}
		p.fsys, p.dir, p.builtin = fsys, dir, builtin
		out[p.ID] = &p
	}
	if ents, err := fs.ReadDir(kitFS, "kit/profiles"); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				load(kitFS, "kit/profiles/"+e.Name(), true)
			}
		}
	}
	os.MkdirAll(userProfilesDir(), 0o755)
	ufs := os.DirFS(userProfilesDir())
	if ents, err := os.ReadDir(userProfilesDir()); err == nil {
		for _, e := range ents {
			if e.IsDir() {
				load(ufs, e.Name(), false) // user profiles override built-ins with the same id
			}
		}
	}
	return out
}

func sortedProfileIDs(m map[string]*LocalProfile) []string {
	ids := make([]string, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		if m[ids[i]].builtin != m[ids[j]].builtin {
			return !m[ids[i]].builtin
		}
		return ids[i] < ids[j]
	})
	return ids
}

func (p *LocalProfile) sizeOf(ids map[string]bool) float64 {
	t := 0.0
	for _, m := range p.Models {
		if ids[m.ID] {
			t += m.SizeGB
		}
	}
	return float64(int(t*100)) / 100
}

func localProfilesSummary() map[string]any {
	ps := localProfiles()
	list := []map[string]any{}
	for _, id := range sortedProfileIDs(ps) {
		p := ps[id]
		wake := map[string]bool{}
		for _, w := range p.Workflows {
			if w.ID == p.AppDefault {
				for _, m := range w.Models {
					wake[m] = true
				}
			}
		}
		if p.Smoke != nil {
			for _, m := range p.Smoke.Models {
				wake[m] = true
			}
		}
		var aiV any
		if ai := p.aiSummary(); ai != nil {
			aiV = ai
			for _, m := range ai["models"].([]string) {
				wake[m] = true
			}
		}
		wfs := []map[string]any{}
		for _, w := range p.Workflows {
			mm := map[string]bool{}
			for _, m := range w.Models {
				mm[m] = true
			}
			wfs = append(wfs, map[string]any{"id": w.ID, "title": w.Title, "optional": w.Optional, "prefetch": w.Prefetch, "size_gb": p.sizeOf(mm)})
		}
		list = append(list, map[string]any{"id": p.ID, "title": p.Title, "accelerator": p.Accelerator, "app_default": p.AppDefault,
			"size_gb_default": p.sizeOf(wake), "workflows": wfs, "builtin": p.builtin, "ai": aiV})
	}
	def := getConfig().LocalProfile
	if _, ok := ps[def]; !ok {
		if ids := sortedProfileIDs(ps); len(ids) > 0 {
			def = ids[0]
		}
	}
	return map[string]any{"default": def, "profiles": list, "errors": map[string]string{}}
}

// buildKitTarball packs the kit (setup.py, plan builder, driver, auth proxy), the chosen profile
// folder and the boot request into the tar.gz that is extracted to /content/kit on the VM.
func buildKitTarball(p *LocalProfile, request map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	add := func(name string, data []byte) error {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(data)), ModTime: time.Now().Add(-24 * time.Hour)}); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}
	err := fs.WalkDir(kitFS, "kit", func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(fp, "kit/profiles/") || strings.Contains(fp, "__pycache__") {
			return err
		}
		b, err := kitFS.ReadFile(fp)
		if err != nil {
			return err
		}
		return add(strings.TrimPrefix(fp, "kit/"), b)
	})
	if err != nil {
		return nil, err
	}
	err = fs.WalkDir(p.fsys, p.dir, func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(p.fsys, fp)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(fp, p.dir), "/")
		return add("profiles/"+p.ID+"/"+rel, b)
	})
	if err != nil {
		return nil, err
	}
	rb, _ := json.Marshal(request)
	if err := add("request.json", rb); err != nil {
		return nil, err
	}
	tw.Close()
	gz.Close()
	return buf.Bytes(), nil
}

// hfTokenEnv hands the optional Hugging Face token (settings, local mode) to setup.py, which only sends it
// to huggingface.co, strips it on redirects and removes it from the ComfyUI / Ollama environments.
// The exec code itself is never logged.
func hfTokenEnv() string {
	t := strings.TrimSpace(getConfig().HFToken)
	if t == "" || strings.ContainsAny(t, "'\"\\\n\r") {
		return ""
	}
	return fmt.Sprintf("os.environ['HF_TOKEN']=%q\n", t)
}

func validTunnel(u string) bool {
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://127.0.0.1:") // loopback: tests
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
