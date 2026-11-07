package main

import (
	"bufio"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed ui
var uiFS embed.FS

var apiKey = func() string { b := make([]byte, 16); rand.Read(b); return hex.EncodeToString(b) }()

func jsonOut(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, code int, err error) {
	jsonOut(w, code, map[string]string{"error": err.Error()})
}

func newServer() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(uiFS, "ui")
	mux.Handle("/", http.FileServer(http.FS(sub)))

	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", func(w http.ResponseWriter, r *http.Request) {
		c := getConfig()
		if c.CFClientSecret != "" {
			c.CFClientSecret = "********"
		}
		if c.HFToken != "" {
			c.HFToken = "********"
		}
		llms := make([]LLMProvider, len(c.LLMs))
		for i, p := range c.LLMs {
			if p.APIKey != "" {
				p.APIKey = keyMask
			}
			llms[i] = p
		}
		c.LLMs = llms
		jsonOut(w, 200, map[string]any{"config": c, "data_dir": dataDir, "version": AppVersion, "default_update_url": defaultUpdateURL})
	})
	api.HandleFunc("POST /api/config", func(w http.ResponseWriter, r *http.Request) {
		var c Config
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			errOut(w, 400, err)
			return
		}
		old := getConfig()
		if c.CFClientSecret == "********" {
			c.CFClientSecret = old.CFClientSecret
		}
		if c.HFToken == "********" {
			c.HFToken = old.HFToken
		}
		c.HFToken = strings.TrimSpace(c.HFToken)
		for i, p := range c.LLMs {
			if p.ID == "" {
				c.LLMs[i].ID = newID()
			}
			if p.APIKey == keyMask {
				c.LLMs[i].APIKey = ""
				for _, q := range old.LLMs {
					if q.ID == p.ID {
						c.LLMs[i].APIKey = q.APIKey
					}
				}
			}
		}
		if c.OutDir == "" {
			c.OutDir = defaultOutDir()
		}
		if err := saveConfig(c); err != nil {
			errOut(w, 500, err)
			return
		}
		store.poke()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})

	// ---- gate passthrough ----
	gatePass := func(method, path string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var in any
			if method == "POST" {
				b, _ := io.ReadAll(r.Body)
				if len(b) > 0 {
					json.Unmarshal(b, &in)
				}
			}
			var out any
			if err := gateJSON(method, path, in, &out); err != nil {
				code := 502
				if ge, ok := err.(*GateError); ok && ge.Status >= 400 {
					code = ge.Status
				}
				jsonOut(w, code, map[string]any{"error": err.Error(), "status": code})
				return
			}
			if method == "POST" {
				autoStopped = false
				store.poke()
			}
			jsonOut(w, 200, out)
		}
	}
	api.HandleFunc("GET /api/gate/status", gatePass("GET", "/_gate/status"))
	api.HandleFunc("POST /api/gate/test", func(w http.ResponseWriter, r *http.Request) {
		st, err := testGateway(15 * time.Second)
		if err != nil {
			errOut(w, 502, err)
			return
		}
		jsonOut(w, 200, st)
	})
	api.HandleFunc("GET /api/gate/profiles", gatePass("GET", "/_gate/profiles"))
	api.HandleFunc("POST /api/gate/wake", gatePass("POST", "/_gate/wake"))
	api.HandleFunc("POST /api/gate/stop", func(w http.ResponseWriter, r *http.Request) {
		warn := ud.beforeStop() // back up ComfyUI user data first (at most 20 s)
		var out map[string]any
		if err := gateJSON("POST", "/_gate/stop", map[string]any{}, &out); err != nil {
			code := 502
			if ge, ok := err.(*GateError); ok && ge.Status >= 400 {
				code = ge.Status
			}
			jsonOut(w, code, map[string]any{"error": err.Error(), "status": code, "backup_warning": warn})
			return
		}
		if out == nil {
			out = map[string]any{}
		}
		out["backup_warning"] = warn
		store.poke()
		jsonOut(w, 200, out)
	})
	api.HandleFunc("POST /api/gate/extend", gatePass("POST", "/_gate/extend"))
	api.HandleFunc("POST /api/gate/switch", gatePass("POST", "/_gate/switch"))
	api.HandleFunc("GET /api/gate/events", sseProxy)
	api.HandleFunc("GET /api/gate/library", gatePass("GET", "/_gate/library"))
	api.HandleFunc("POST /api/gate/library", gatePass("POST", "/_gate/library"))
	api.HandleFunc("POST /api/gate/library/install", gatePass("POST", "/_gate/library/install"))
	api.HandleFunc("DELETE /api/gate/library/{id}", func(w http.ResponseWriter, r *http.Request) {
		var out any
		if err := gateJSON("DELETE", "/_gate/library/"+url.PathEscape(r.PathValue("id")), nil, &out); err != nil {
			code := 502
			if ge, ok := err.(*GateError); ok && ge.Status >= 400 {
				code = ge.Status
			}
			jsonOut(w, code, map[string]any{"error": err.Error(), "status": code})
			return
		}
		jsonOut(w, 200, out)
	})

	api.HandleFunc("POST /api/pair/start", pairStart)
	localRoutes(api)
	aiRoutes(api)
	api.HandleFunc("GET /api/gpu/stats", gpuStatsHandler)
	api.HandleFunc("GET /api/update/check", updateCheckHandler)
	api.HandleFunc("POST /api/update/apply", updateApplyHandler)
	api.HandleFunc("GET /api/update/status", updateStatusHandler)
	api.HandleFunc("POST /api/llm/write", llmWriteHandler)
	api.HandleFunc("POST /api/llm/test", llmTestHandler)
	api.HandleFunc("POST /api/llm/models", llmModelsHandler)
	api.HandleFunc("GET /api/pair/state", func(w http.ResponseWriter, r *http.Request) {
		pair.mu.Lock()
		defer pair.mu.Unlock()
		jsonOut(w, 200, map[string]any{"state": pair.state, "code": pair.code, "url": pair.url, "message": pair.msg})
	})

	// ---- templates ----
	api.HandleFunc("GET /api/templates", func(w http.ResponseWriter, r *http.Request) {
		out := []map[string]any{}
		for _, t := range listTemplates() {
			out = append(out, map[string]any{"id": t.ID, "name": t.Name, "created": t.Created, "nodes": len(t.Graph), "hidden": t.Hidden, "profile": t.Profile, "fields": detectFields(t)})
		}
		jsonOut(w, 200, out)
	})
	api.HandleFunc("POST /api/templates", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name string          `json:"name"`
			JSON json.RawMessage `json:"json"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&in); err != nil {
			errOut(w, 400, err)
			return
		}
		var raw []byte = in.JSON
		var s string
		if json.Unmarshal(in.JSON, &s) == nil { // sent as a string
			raw = []byte(s)
		}
		g, err := parseWorkflow(raw)
		if err != nil {
			errOut(w, 400, err)
			return
		}
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = "未命名工作流"
		}
		t := &Template{Name: name, Graph: g}
		if err := saveTemplate(t); err != nil {
			errOut(w, 500, err)
			return
		}
		jsonOut(w, 200, map[string]any{"id": t.ID, "fields": detectFields(t)})
	})
	api.HandleFunc("POST /api/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		t, err := loadTemplate(r.PathValue("id"))
		if err != nil {
			errOut(w, 404, err)
			return
		}
		var in struct {
			Name   *string  `json:"name"`
			Hidden []string `json:"hidden"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
			t.Name = strings.TrimSpace(*in.Name)
		}
		if in.Hidden != nil {
			t.Hidden = in.Hidden
		}
		saveTemplate(t)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("DELETE /api/templates/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, err := loadTemplate(id); err != nil {
			errOut(w, 404, err)
			return
		}
		os.Remove(tplPath(id))
		jsonOut(w, 200, map[string]bool{"ok": true})
	})

	// ---- jobs ----
	api.HandleFunc("GET /api/jobs", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, store.snapshot()) })
	api.HandleFunc("POST /api/jobs", createJob)
	api.HandleFunc("POST /api/jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		j := store.get(r.PathValue("id"))
		if j == nil {
			errOut(w, 404, os.ErrNotExist)
			return
		}
		store.update(j, func(j *Job) {
			if j.Status == "queued" || j.Status == "waiting" {
				j.Status, j.Message, j.Finished = "canceled", "已取消", time.Now().Unix()
			} else {
				j.cancel = true
			}
		})
		store.poke()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/jobs/{id}/retry", func(w http.ResponseWriter, r *http.Request) {
		j := store.get(r.PathValue("id"))
		if j == nil {
			errOut(w, 404, os.ErrNotExist)
			return
		}
		store.update(j, func(j *Job) {
			if j.Status == "failed" || j.Status == "canceled" || j.Status == "done" {
				j.Status, j.Message, j.PromptID, j.Progress, j.Outputs, j.cancel = "queued", "重新排队", "", 0, nil, false
				j.Started, j.Finished, j.QueuePos = 0, 0, 0
			}
		})
		store.poke()
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/jobs/{id}/hires", func(w http.ResponseWriter, r *http.Request) {
		src := store.get(r.PathValue("id"))
		if src == nil {
			errOut(w, 404, errString("任务不存在"))
			return
		}
		store.mu.Lock()
		s := *src
		store.mu.Unlock()
		if s.Status != "done" {
			errOut(w, 409, errString("试生成还没完成"))
			return
		}
		if _, err := loadTemplate(s.TemplateID); err != nil {
			errOut(w, 404, errString("模板已被删除"))
			return
		}
		vals := map[string]any{}
		for k, v := range s.Values {
			vals[k] = v
		}
		for k, v := range s.UsedSeeds { // lock the seeds the trial actually used
			vals[k] = v
		}
		up := map[string]string{}
		for k, v := range s.Uploads {
			up[k] = v
		}
		title := strings.TrimSuffix(s.Title, " · 草稿")
		j := &Job{ID: newID() + "-h", TemplateID: s.TemplateID, TemplateName: s.TemplateName, Title: title + " · 高清",
			Values: vals, Uploads: up, Status: "queued", Message: "排队中（高清版）", Created: time.Now().Unix(),
			ParentID: s.ID, FullRes: s.FullRes}
		store.add(j)
		jsonOut(w, 200, map[string]any{"id": j.ID})
	})
	api.HandleFunc("DELETE /api/jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j := store.get(r.PathValue("id"))
		if j != nil && (j.Status == "queued" || j.Status == "waiting" || j.Status == "failed" || j.Status == "done" || j.Status == "canceled") {
			store.remove(j.ID)
			jsonOut(w, 200, map[string]bool{"ok": true})
			return
		}
		errOut(w, 409, errString("运行中的任务请先取消"))
	})
	api.HandleFunc("POST /api/jobs/clear", func(w http.ResponseWriter, r *http.Request) {
		for _, j := range store.snapshot() {
			if j.Status == "done" || j.Status == "canceled" || j.Status == "failed" {
				store.remove(j.ID)
			}
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	})

	// ---- prompt library ----
	api.HandleFunc("GET /api/prompts", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, loadPrompts()) })
	api.HandleFunc("POST /api/prompts", func(w http.ResponseWriter, r *http.Request) {
		var in []map[string]any
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			errOut(w, 400, err)
			return
		}
		writeJSON(filepath.Join(dataDir, "prompts.json"), in)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})

	// ---- local results ----
	api.HandleFunc("GET /api/files", listFiles)
	api.HandleFunc("GET /api/file", serveFile)
	api.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		p := in.Path
		if p == "" {
			p = getConfig().OutDir
			os.MkdirAll(p, 0o755)
		}
		if !insideOut(p) && filepath.Clean(p) != filepath.Clean(getConfig().OutDir) {
			errOut(w, 403, errString("只能打开输出文件夹里的文件"))
			return
		}
		openPath(p)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/open-url", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if in.URL == "comfy" {
			in.URL = baseURL() + "/"
			if localMode() {
				in.URL = lg.baseURL() + "/?wbk=" + lg.key
			}
		}
		if strings.HasPrefix(in.URL, "https://") || strings.HasPrefix(in.URL, "http://") {
			openBrowser(in.URL)
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
	api.HandleFunc("POST /api/delete-file", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Path string `json:"path"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		if !insideOut(in.Path) {
			errOut(w, 403, errString("只能删除输出文件夹里的文件"))
			return
		}
		if err := os.Remove(in.Path); err != nil {
			errOut(w, 500, err)
			return
		}
		jsonOut(w, 200, map[string]bool{"ok": true})
	})

	registerCustomModelRoutes(api)
	registerUserdataRoutes(api)

	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		k := r.Header.Get("X-Key")
		if k == "" {
			k = r.URL.Query().Get("k")
		}
		if k != apiKey {
			http.Error(w, "forbidden", 403)
			return
		}
		api.ServeHTTP(w, r)
	})
	return mux
}

type errString string

func (e errString) Error() string { return string(e) }

func createJob(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		errOut(w, 400, err)
		return
	}
	var spec struct {
		TemplateID  string         `json:"template_id"`
		Values      map[string]any `json:"values"`
		RandomSeeds []string       `json:"random_seeds"`
		Title       string         `json:"title"`
		Count       int            `json:"count"`
		Draft       bool           `json:"draft"`
	}
	if err := json.Unmarshal([]byte(r.FormValue("spec")), &spec); err != nil {
		errOut(w, 400, err)
		return
	}
	t, err := loadTemplate(spec.TemplateID)
	if err != nil {
		errOut(w, 404, errString("模板不存在"))
		return
	}
	if spec.Count < 1 {
		spec.Count = 1
	}
	if spec.Count > 50 {
		spec.Count = 50
	}
	var draftSet map[string]any
	var draftRes, fullRes string
	if spec.Draft {
		c := getConfig()
		draftSet, draftRes, fullRes = draftPlan(t, spec.Values, c.DraftScale, c.DraftSteps)
		if draftRes == "" {
			errOut(w, 400, errString("这个模板里没找到数字型的 width / height 或 megapixels 参数，没法自动降分辨率"))
			return
		}
	}
	base := newID()
	uploads := map[string]string{}
	if r.MultipartForm != nil {
		for key, fhs := range r.MultipartForm.File {
			if len(fhs) == 0 {
				continue
			}
			fh := fhs[0]
			src, err := fh.Open()
			if err != nil {
				continue
			}
			name := unsafeName.ReplaceAllString(filepath.Base(fh.Filename), "_")
			dst := filepath.Join(dataDir, "uploads", base+"_"+sanitizeKey(key)+"__"+name)
			f, err := os.Create(dst)
			if err == nil {
				io.Copy(f, src)
				f.Close()
				uploads[key] = dst
			}
			src.Close()
		}
	}
	// reuse an earlier job's upload (for "use again")
	for k, v := range spec.Values {
		if s, ok := v.(string); ok && strings.HasPrefix(s, "@upload:") {
			p := strings.TrimPrefix(s, "@upload:")
			if filepath.Dir(p) == filepath.Join(dataDir, "uploads") {
				if _, err := os.Stat(p); err == nil {
					uploads[k] = p
				}
			}
			delete(spec.Values, k)
		}
	}
	ids := []string{}
	for i := 0; i < spec.Count; i++ {
		j := &Job{ID: base + "-" + itoa(i), TemplateID: t.ID, TemplateName: t.Name, Title: spec.Title, Values: spec.Values,
			Uploads: uploads, RandomSeeds: spec.RandomSeeds, Status: "queued", Message: "排队中", Created: time.Now().Unix(),
			Draft: spec.Draft, DraftSet: draftSet, DraftRes: draftRes, FullRes: fullRes}
		if j.Title == "" {
			j.Title = t.Name
		}
		store.add(j)
		ids = append(ids, j.ID)
	}
	jsonOut(w, 200, map[string]any{"ids": ids})
}

func itoa(i int) string { return strconv.Itoa(i) }

func sanitizeKey(k string) string {
	return unsafeName.ReplaceAllString(strings.ReplaceAll(k, ".", "-"), "_")
}

func loadPrompts() []map[string]any {
	out := []map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dataDir, "prompts.json")); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

// ---- SSE passthrough of /_gate/events (boot logs & progress) ----
func sseProxy(w http.ResponseWriter, r *http.Request) {
	b := baseURL()
	if b == "" {
		http.Error(w, "no url", 400)
		return
	}
	req, _ := http.NewRequestWithContext(r.Context(), "GET", b+"/_gate/events", nil)
	addAuth(req)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 || !strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		http.Error(w, "upstream "+resp.Status, 502)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	// whole upstream events (up to the blank line), so the app's own log lines (userdata.go) can be
	// slotted in between events without breaking one
	events := make(chan []byte, 64)
	go func() {
		defer close(events)
		rd := bufio.NewReader(resp.Body)
		var ev []byte
		for {
			line, err := rd.ReadBytes('\n')
			ev = append(ev, line...)
			if len(line) > 0 && (line[0] == '\n' || string(line) == "\r\n") || err != nil {
				if len(ev) > 0 {
					select {
					case events <- ev:
					case <-r.Context().Done():
						return
					}
				}
				ev = nil
			}
			if err != nil {
				return
			}
		}
	}()
	appCh, backlog, unsub := appLogSubscribe()
	defer unsub()
	// the gateway replays its backlog first; add ours after it has settled
	replay := time.After(800 * time.Millisecond)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			w.Write(ev)
			flush()
		case <-replay:
			for _, l := range backlog {
				w.Write(sseLogEvent(l))
			}
			flush()
		case l := <-appCh:
			w.Write(sseLogEvent(l))
			flush()
		case <-r.Context().Done():
			return
		}
	}
}

// ---- device-code pairing (RFC 8628, same flow as `ckit auth`) ----
var pair struct {
	mu                    sync.Mutex
	state, code, url, msg string
}

func pairStart(w http.ResponseWriter, r *http.Request) {
	var res struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
		VerifyURL  string `json:"verification_uri_complete"`
		Interval   int    `json:"interval"`
		ExpiresIn  int    `json:"expires_in"`
	}
	if err := gateJSON("POST", "/api/auth/device/code", map[string]string{"client_name": "Wakeloom"}, &res); err != nil {
		errOut(w, 502, err)
		return
	}
	if res.VerifyURL == "" {
		res.VerifyURL = baseURL() + "/auth/device?code=" + res.UserCode
	}
	pair.mu.Lock()
	pair.state, pair.code, pair.url, pair.msg = "pending", res.UserCode, res.VerifyURL, "请在打开的浏览器页面里确认"
	pair.mu.Unlock()
	openBrowser(res.VerifyURL)
	go func() {
		iv := res.Interval
		if iv < 2 {
			iv = 3
		}
		exp := res.ExpiresIn
		if exp <= 0 {
			exp = 600
		}
		deadline := time.Now().Add(time.Duration(exp) * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(time.Duration(iv) * time.Second)
			var tok struct {
				AccessToken string `json:"access_token"`
				Email       string `json:"email"`
			}
			err := gateJSON("POST", "/api/auth/device/token", map[string]string{"device_code": res.DeviceCode}, &tok)
			if err == nil && tok.AccessToken != "" {
				c := getConfig()
				c.Token = tok.AccessToken
				saveConfig(c)
				pair.mu.Lock()
				pair.state, pair.msg = "ok", "配对成功："+tok.Email
				pair.mu.Unlock()
				store.poke()
				return
			}
			if ge, ok := err.(*GateError); ok {
				switch ge.Code {
				case "authorization_pending", "":
					continue
				case "slow_down":
					iv += 2
					continue
				default:
					pair.mu.Lock()
					pair.state, pair.msg = "error", "配对失败："+ge.Error()
					pair.mu.Unlock()
					return
				}
			}
		}
		pair.mu.Lock()
		pair.state, pair.msg = "error", "配对超时，请重试"
		pair.mu.Unlock()
	}()
	jsonOut(w, 200, map[string]any{"code": res.UserCode, "url": res.VerifyURL})
}

// ---- results folder ----
func insideOut(p string) bool {
	out, err1 := filepath.Abs(getConfig().OutDir)
	ap, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(out, ap)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

func listFiles(w http.ResponseWriter, r *http.Request) {
	out := getConfig().OutDir
	type F struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Dir   string `json:"dir"`
		Kind  string `json:"kind"`
		Size  int64  `json:"size"`
		Mtime int64  `json:"mtime"`
	}
	var fsList []F
	filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		k := fileKind(d.Name())
		if k == "file" || strings.HasSuffix(d.Name(), ".part") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(out, filepath.Dir(p))
		fsList = append(fsList, F{d.Name(), p, rel, k, info.Size(), info.ModTime().Unix()})
		return nil
	})
	sort.Slice(fsList, func(i, j int) bool { return fsList[i].Mtime > fsList[j].Mtime })
	if len(fsList) > 500 {
		fsList = fsList[:500]
	}
	if fsList == nil {
		fsList = []F{}
	}
	jsonOut(w, 200, map[string]any{"dir": out, "files": fsList})
}

func serveFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !insideOut(p) {
		http.Error(w, "forbidden", 403)
		return
	}
	http.ServeFile(w, r, p)
}

func openPath(p string) {
	switch runtime.GOOS {
	case "windows":
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			exec.Command("explorer", "/select,", p).Start()
		} else {
			exec.Command("explorer", p).Start()
		}
	case "darwin":
		exec.Command("open", "-R", p).Start()
	default:
		exec.Command("xdg-open", filepath.Dir(p)).Start()
	}
}

func openBrowser(u string) {
	switch runtime.GOOS {
	case "windows":
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	case "darwin":
		exec.Command("open", u).Start()
	default:
		exec.Command("xdg-open", u).Start()
	}
}
