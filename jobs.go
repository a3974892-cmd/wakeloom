package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Output struct {
	Filename string `json:"filename"`
	Path     string `json:"path"`
	Kind     string `json:"kind"`
}

type Job struct {
	ID           string            `json:"id"`
	TemplateID   string            `json:"template_id"`
	TemplateName string            `json:"template_name"`
	Title        string            `json:"title"`
	Values       map[string]any    `json:"values"`
	Uploads      map[string]string `json:"uploads"`      // field key -> local stored path
	RandomSeeds  []string          `json:"random_seeds"` // seed fields re-rolled at submit
	Status       string            `json:"status"`       // queued waiting uploading submitted running downloading done failed canceled
	Message      string            `json:"message"`
	PromptID     string            `json:"prompt_id"`
	QueuePos     int               `json:"queue_pos"`
	Progress     float64           `json:"progress"`
	Node         string            `json:"node"`
	UsedSeeds    map[string]int64  `json:"used_seeds,omitempty"`
	Outputs      []Output          `json:"outputs"`
	Created      int64             `json:"created"`
	Draft        bool              `json:"draft,omitempty"`     // low-res trial run
	DraftSet     map[string]any    `json:"draft_set,omitempty"` // overrides applied only to the trial (width/height/steps)
	DraftRes     string            `json:"draft_res,omitempty"`
	FullRes      string            `json:"full_res,omitempty"`
	ParentID     string            `json:"parent_id,omitempty"` // hi-res job: the draft it came from
	Started      int64             `json:"started"`
	Finished     int64             `json:"finished"`
	cancel       bool
}

type JobStore struct {
	mu   sync.Mutex
	jobs []*Job
	kick chan struct{}
}

var store = &JobStore{kick: make(chan struct{}, 1)}

func jobsPath() string { return filepath.Join(dataDir, "jobs.json") }

func (s *JobStore) load() {
	b, err := os.ReadFile(jobsPath())
	if err != nil {
		return
	}
	json.Unmarshal(b, &s.jobs)
	for _, j := range s.jobs {
		// jobs interrupted by an app restart: resume ones already on ComfyUI, re-queue the rest
		switch j.Status {
		case "uploading", "waiting":
			j.Status = "queued"
		case "downloading":
			j.Status = "running"
		}
	}
}

func (s *JobStore) saveLocked() { writeJSON(jobsPath(), s.jobs) }

func (s *JobStore) update(j *Job, f func(*Job)) {
	s.mu.Lock()
	f(j)
	s.saveLocked()
	s.mu.Unlock()
}

func (s *JobStore) add(j *Job) {
	s.mu.Lock()
	s.jobs = append(s.jobs, j)
	s.saveLocked()
	s.mu.Unlock()
	s.poke()
}

func (s *JobStore) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *JobStore) snapshot() []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Job, 0, len(s.jobs))
	for i := len(s.jobs) - 1; i >= 0; i-- {
		out = append(out, *s.jobs[i])
	}
	return out
}

func (s *JobStore) get(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

func (s *JobStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, j := range s.jobs {
		if j.ID == id {
			for _, p := range j.Uploads {
				if !s.uploadSharedLocked(p, id) {
					os.Remove(p)
				}
			}
			s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
			s.saveLocked()
			return
		}
	}
}

func (s *JobStore) uploadSharedLocked(p, except string) bool {
	for _, j := range s.jobs {
		if j.ID == except {
			continue
		}
		for _, q := range j.Uploads {
			if q == p {
				return true
			}
		}
	}
	return false
}

func (s *JobStore) next() *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	// a job already on ComfyUI (after restart) goes first
	for _, j := range s.jobs {
		if (j.Status == "submitted" || j.Status == "running") && j.PromptID != "" {
			return j
		}
	}
	for _, j := range s.jobs {
		if j.Status == "queued" {
			return j
		}
	}
	return nil
}

func (s *JobStore) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, j := range s.jobs {
		switch j.Status {
		case "queued", "waiting", "uploading", "submitted", "running", "downloading":
			n++
		}
	}
	return n
}

// ---- progress over ComfyUI websocket ----

var clientID = fmt.Sprintf("ckwb-%d", time.Now().UnixNano())

type wsProgress struct {
	mu       sync.Mutex
	value    map[string]float64
	node     map[string]string
	conn     bool
	lastTry  time.Time
	cancelFn context.CancelFunc
}

var prog = &wsProgress{value: map[string]float64{}, node: map[string]string{}}

func (p *wsProgress) ensure() {
	p.mu.Lock()
	if p.conn || time.Since(p.lastTry) < 15*time.Second {
		p.mu.Unlock()
		return
	}
	p.lastTry = time.Now()
	p.conn = true
	p.mu.Unlock()
	go p.run()
}

func (p *wsProgress) run() {
	defer func() { p.mu.Lock(); p.conn = false; p.mu.Unlock() }()
	b := baseURL()
	if b == "" {
		return
	}
	u := strings.Replace(strings.Replace(b, "https://", "wss://", 1), "http://", "ws://", 1) + "/ws?clientId=" + clientID
	h := http.Header{}
	req, _ := http.NewRequest("GET", b, nil)
	addAuth(req)
	for k, v := range req.Header {
		h[k] = v
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, err := websocket.Dial(ctx, u, &websocket.DialOptions{HTTPHeader: h, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return
	}
	c.SetReadLimit(64 << 20)
	defer c.CloseNow()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageText {
			continue // binary preview frames
		}
		var m struct {
			Type string `json:"type"`
			Data struct {
				PromptID string  `json:"prompt_id"`
				Value    float64 `json:"value"`
				Max      float64 `json:"max"`
				Node     any     `json:"node"`
			} `json:"data"`
		}
		if json.Unmarshal(data, &m) != nil || m.Data.PromptID == "" {
			continue
		}
		p.mu.Lock()
		switch m.Type {
		case "progress":
			if m.Data.Max > 0 {
				p.value[m.Data.PromptID] = m.Data.Value / m.Data.Max
			}
		case "executing":
			if s, ok := m.Data.Node.(string); ok {
				p.node[m.Data.PromptID] = s
				p.value[m.Data.PromptID] = 0
			}
		}
		p.mu.Unlock()
	}
}

func (p *wsProgress) get(pid string) (float64, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.value[pid], p.node[pid]
}

// ---- worker ----

var lastActivity = time.Now()

func worker() {
	for {
		j := store.next()
		if j == nil {
			maybeAutoStop()
			select {
			case <-store.kick:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		runJob(j)
		lastActivity = time.Now()
	}
}

var autoStopped bool

func maybeAutoStop() {
	if localMode() && aiVisionInstalling() {
		return
	}
	c := getConfig()
	if !c.AutoStopIdle || autoStopped || time.Since(lastActivity) < 3*time.Minute || store.pending() > 0 {
		return
	}
	if st, err := gateState(); err == nil && st == "ready" {
		ud.beforeStop()
		var out map[string]any
		if gateJSON("POST", "/_gate/stop", nil, &out) == nil {
			autoStopped = true
		}
	}
}

func fail(j *Job, msg string) {
	store.update(j, func(j *Job) {
		j.Status, j.Message, j.Finished = "failed", msg, time.Now().Unix()
	})
}

func canceled(j *Job) bool {
	store.mu.Lock()
	defer store.mu.Unlock()
	return j.cancel || j.Status == "canceled"
}

// waitReady blocks until the gate is ready; optionally wakes it.
func waitReady(j *Job) bool {
	woke := false
	for {
		if canceled(j) {
			return false
		}
		st, err := gateState()
		if err != nil {
			store.update(j, func(j *Job) { j.Status, j.Message = "waiting", "网关不可用："+err.Error() })
			time.Sleep(10 * time.Second)
			continue
		}
		switch st {
		case "ready":
			return true
		case "sleeping", "failed":
			c := getConfig()
			if c.AutoWake && !woke {
				body := map[string]any{}
				if c.WakeProfile != "" {
					body["profile"] = c.WakeProfile
				}
				if t, err := loadTemplate(j.TemplateID); err == nil && t.Profile != "" {
					body["profile"] = t.Profile // a custom model linked to this template
				}
				if p, _ := body["profile"].(string); customWakeProblem(p) != "" {
					store.update(j, func(j *Job) { j.Status, j.Message = "waiting", customWakeProblem(p) })
					time.Sleep(30 * time.Second)
					continue
				}
				var out map[string]any
				if err := gateJSON("POST", "/_gate/wake", body, &out); err != nil {
					store.update(j, func(j *Job) { j.Status, j.Message = "waiting", "自动唤醒失败："+err.Error() })
					time.Sleep(15 * time.Second)
					continue
				}
				woke = true
				autoStopped = false
				store.update(j, func(j *Job) { j.Status, j.Message = "waiting", "已自动唤醒 GPU，等待就绪…" })
			} else if !woke {
				store.update(j, func(j *Job) {
					j.Status, j.Message = "waiting", "GPU 在休眠，点「唤醒」后自动开始（或在设置里开启自动唤醒）"
				})
			} else if st == "failed" {
				store.update(j, func(j *Job) {
					j.Status, j.Message = "waiting", "GPU 启动失败，请查看控制台日志后重新唤醒"
				})
				woke = false
			}
		default:
			store.update(j, func(j *Job) { j.Status, j.Message = "waiting", "GPU 启动中（"+stateCN(st)+"）…" })
		}
		select {
		case <-store.kick:
		case <-time.After(5 * time.Second):
		}
	}
}

func stateCN(s string) string {
	m := map[string]string{"sleeping": "休眠", "allocating": "分配机器", "installing": "安装依赖", "downloading": "下载模型",
		"booting": "启动 ComfyUI", "smoke_testing": "自检", "ready": "就绪", "switching": "切换配置", "stopping": "停止中", "failed": "失败"}
	if v, ok := m[s]; ok {
		return v
	}
	return s
}

func runJob(j *Job) {
	if j.PromptID == "" {
		if !waitReady(j) {
			return
		}
		if !submit(j) {
			return
		}
	}
	track(j)
}

func submit(j *Job) bool {
	t, err := loadTemplate(j.TemplateID)
	if err != nil {
		fail(j, "模板已被删除")
		return false
	}
	required := t.Profile
	if required == "" && strings.HasPrefix(t.ID, "ai-") {
		required = strings.TrimPrefix(t.ID, "ai-")
	}
	if required != "" {
		st, err := gateStatus()
		if err != nil {
			fail(j, "无法核对当前模型配置："+err.Error())
			return false
		}
		if current, _ := st["profile"].(string); current != "" && current != required {
			fail(j, fmt.Sprintf("当前 GPU 运行的是 %s，此任务需要 %s。请使用当前模型的工作流，或用对应配置重新唤醒", current, required))
			return false
		}
	}
	store.update(j, func(j *Job) { j.Status, j.Message, j.Started = "uploading", "上传素材…", time.Now().Unix() })
	g := cloneGraph(t.Graph)
	for k, v := range j.Values {
		if err := setInput(g, k, v); err != nil {
			fail(j, err.Error())
			return false
		}
	}
	if strings.HasPrefix(t.ID, "ai-") {
		if err := checkImageModels(g); err != nil {
			fail(j, err.Error())
			return false
		}
	}
	keys := make([]string, 0, len(j.Uploads))
	for k := range j.Uploads {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		name, err := uploadFile(j.Uploads[k])
		if err != nil {
			fail(j, err.Error())
			return false
		}
		setInput(g, k, name)
	}
	if j.Draft {
		for k, v := range j.DraftSet {
			setInput(g, k, v)
		}
	}
	used := map[string]int64{}
	for _, k := range j.RandomSeeds {
		s := rand.Int63n(1 << 48)
		setInput(g, k, s)
		used[k] = s
	}
	var resp struct {
		PromptID   string         `json:"prompt_id"`
		Number     int            `json:"number"`
		NodeErrors map[string]any `json:"node_errors"`
	}
	prog.ensure()
	err = gateJSON("POST", "/prompt", map[string]any{"prompt": g, "client_id": clientID}, &resp)
	if err != nil {
		var ge *GateError
		msg := err.Error()
		if errors.As(err, &ge) && ge.Status == 400 {
			msg = "ComfyUI 拒绝了工作流：" + ge.Msg + nodeErrors(ge.Raw)
		}
		fail(j, msg)
		return false
	}
	if resp.PromptID == "" {
		fail(j, "ComfyUI 没有返回 prompt_id")
		return false
	}
	store.update(j, func(j *Job) {
		j.PromptID, j.Status, j.Message, j.UsedSeeds = resp.PromptID, "submitted", "已提交，排队中", used
	})
	return true
}

func track(j *Job) {
	timeout := time.Duration(getConfig().JobTimeoutMin) * time.Minute
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	start := time.Now()
	misses := 0
	for {
		if canceled(j) {
			gateJSON("POST", "/queue", map[string]any{"delete": []string{j.PromptID}}, nil)
			var q map[string]any
			if gateJSON("GET", "/queue", nil, &q) == nil && inQueue(q["queue_running"], j.PromptID) {
				gateJSON("POST", "/interrupt", map[string]any{"prompt_id": j.PromptID}, nil)
			}
			store.update(j, func(j *Job) { j.Status, j.Message, j.Finished = "canceled", "已取消", time.Now().Unix() })
			return
		}
		if time.Since(start) > timeout {
			fail(j, fmt.Sprintf("超过 %d 分钟没有完成（可在设置里调大超时）", int(timeout.Minutes())))
			return
		}
		var h map[string]struct {
			Status struct {
				StatusStr string  `json:"status_str"`
				Completed bool    `json:"completed"`
				Messages  [][]any `json:"messages"`
			} `json:"status"`
			Outputs map[string]map[string]any `json:"outputs"`
		}
		err := gateJSON("GET", "/history/"+j.PromptID, nil, &h)
		if err != nil {
			misses++
			st, _ := gateState()
			if st == "sleeping" || st == "stopping" || st == "failed" {
				fail(j, "GPU 已停止（"+stateCN(st)+"），任务中断。可点「重试」")
				return
			}
			if misses > 60 {
				fail(j, "长时间连不上 ComfyUI："+err.Error())
				return
			}
			time.Sleep(5 * time.Second)
			continue
		}
		misses = 0
		if item, ok := h[j.PromptID]; ok {
			if item.Status.StatusStr == "error" {
				fail(j, "生成出错："+execError(item.Status.Messages))
				return
			}
			if item.Status.Completed || item.Status.StatusStr == "success" || len(item.Outputs) > 0 {
				download(j, item.Outputs)
				return
			}
		}
		var q map[string]any
		if gateJSON("GET", "/queue", nil, &q) == nil {
			if inQueue(q["queue_running"], j.PromptID) {
				p, node := prog.get(j.PromptID)
				store.update(j, func(j *Job) {
					j.Status, j.QueuePos, j.Progress, j.Node, j.Message = "running", 0, p, node, "生成中"
				})
			} else if pos := queuePos(q["queue_pending"], j.PromptID); pos > 0 {
				store.update(j, func(j *Job) { j.Status, j.QueuePos, j.Message = "submitted", pos, fmt.Sprintf("排队第 %d 位", pos) })
			}
		}
		prog.ensure()
		time.Sleep(2 * time.Second)
	}
}

func inQueue(v any, pid string) bool { return queuePos(v, pid) > 0 }

func queuePos(v any, pid string) int {
	arr, _ := v.([]any)
	type e struct {
		n   float64
		pid string
	}
	var es []e
	for _, it := range arr {
		a, _ := it.([]any)
		if len(a) >= 2 {
			n, _ := a[0].(float64)
			p, _ := a[1].(string)
			es = append(es, e{n, p})
		}
	}
	sort.Slice(es, func(i, k int) bool { return es[i].n < es[k].n })
	for i, x := range es {
		if x.pid == pid {
			return i + 1
		}
	}
	return 0
}

func execError(msgs [][]any) string {
	for _, m := range msgs {
		if len(m) == 2 {
			if t, _ := m[0].(string); t == "execution_error" {
				d, _ := m[1].(map[string]any)
				return fmt.Sprintf("%v（节点 %v %v）", d["exception_message"], d["node_id"], d["node_type"])
			}
		}
	}
	return "未知错误"
}

var unsafeName = regexp.MustCompile(`[\\/:*?"<>|\s]+`)

func download(j *Job, outputs map[string]map[string]any) {
	store.update(j, func(j *Job) { j.Status, j.Message, j.Progress = "downloading", "下载结果…", 1 })
	type item struct{ fn, sub, typ string }
	var items []item
	var temps []item
	for _, nid := range sortedKeys(outputs) {
		for _, val := range outputs[nid] {
			arr, ok := val.([]any)
			if !ok {
				continue
			}
			for _, x := range arr {
				m, ok := x.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := m["filename"].(string)
				if fn == "" {
					continue
				}
				sub, _ := m["subfolder"].(string)
				typ, _ := m["type"].(string)
				if typ == "" {
					typ = "output"
				}
				it := item{fn, sub, typ}
				if typ == "temp" {
					temps = append(temps, it)
				} else {
					items = append(items, it)
				}
			}
		}
	}
	if len(items) == 0 {
		items = temps
	}
	if len(items) == 0 {
		store.update(j, func(j *Job) {
			j.Status, j.Message, j.Finished = "done", "完成，但工作流没有输出文件（检查是否有 Save 节点）", time.Now().Unix()
		})
		return
	}
	now := time.Now()
	dir := filepath.Join(getConfig().OutDir, now.Format("2006-01-02"))
	title := unsafeName.ReplaceAllString(j.Title, "_")
	if r := []rune(title); len(r) > 24 {
		title = string(r[:24])
	}
	var outs []Output
	seen := map[string]bool{}
	for _, it := range items {
		key := it.sub + "/" + it.fn + "/" + it.typ
		if seen[key] {
			continue
		}
		seen[key] = true
		name := now.Format("150405") + "_" + title + "_" + it.fn
		dest := filepath.Join(dir, name)
		if err := downloadView(it.fn, it.sub, it.typ, dest); err != nil {
			fail(j, err.Error())
			return
		}
		outs = append(outs, Output{Filename: name, Path: dest, Kind: fileKind(name)})
	}
	store.update(j, func(j *Job) {
		j.Outputs, j.Status, j.Finished = outs, "done", time.Now().Unix()
		go markChecklist("generate")
		j.Message = fmt.Sprintf("完成，已保存 %d 个文件", len(outs))
	})
}

func sortedKeys(m map[string]map[string]any) []string {
	g := map[string]map[string]any{}
	for k := range m {
		g[k] = nil
	}
	return sortedNodeIDs(g)
}

func fileKind(name string) string {
	l := strings.ToLower(name)
	switch {
	case hasExt(l, []string{".mp4", ".webm", ".mov", ".mkv"}):
		return "video"
	case hasExt(l, []string{".wav", ".mp3", ".flac", ".ogg", ".m4a", ".aac"}):
		return "audio"
	case hasExt(l, []string{".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp"}):
		return "image"
	}
	return "file"
}

func nodeErrors(raw []byte) string {
	var r struct {
		NodeErrors map[string]struct {
			ClassType string `json:"class_type"`
			Errors    []struct {
				Message string `json:"message"`
				Details string `json:"details"`
			} `json:"errors"`
		} `json:"node_errors"`
	}
	if json.Unmarshal(raw, &r) != nil || len(r.NodeErrors) == 0 {
		return ""
	}
	var parts []string
	for id, n := range r.NodeErrors {
		for _, e := range n.Errors {
			parts = append(parts, strings.TrimSpace(fmt.Sprintf("节点 #%s %s：%s %s", id, n.ClassType, e.Message, e.Details)))
		}
	}
	sort.Strings(parts)
	return "\n" + strings.Join(parts, "\n")
}

// draftPlan works out the trial-run overrides: numeric width/height scaled down (multiples of 32,
// at least 128), "megapixels" (e.g. ResolutionSelector in the official H3 workflow) scaled by
// scale², and optionally fewer steps. Seeds and everything else stay the same so the hi-res run
// reproduces the same shot as closely as the model allows.
func draftPlan(t *Template, values map[string]any, scale float64, steps int) (map[string]any, string, string) {
	if scale <= 0 || scale >= 1 {
		scale = 0.5
	}
	set := map[string]any{}
	var w, h, fw, fh int
	var mp, fmp float64
	for _, f := range detectFields(t) {
		if f.Kind != "number" {
			continue
		}
		in := strings.ToLower(f.Input)
		cur, ok := values[f.Key].(float64)
		if !ok {
			cur, _ = f.Default.(float64)
		}
		switch in {
		case "width", "height":
			v := int(cur*scale/32+0.5) * 32
			if v < 128 {
				v = 128
			}
			set[f.Key] = v
			if in == "width" && w == 0 {
				w, fw = v, int(cur)
			} else if in == "height" && h == 0 {
				h, fh = v, int(cur)
			}
		case "megapixels":
			v := float64(int(cur*scale*scale*100+0.5)) / 100
			if v < 0.05 {
				v = 0.05
			}
			set[f.Key] = v
			if mp == 0 {
				mp, fmp = v, cur
			}
		case "steps":
			if steps > 0 && float64(steps) < cur {
				set[f.Key] = steps
			}
		}
	}
	switch {
	case w > 0 && h > 0:
		return set, fmt.Sprintf("%d×%d", w, h), fmt.Sprintf("%d×%d", fw, fh)
	case mp > 0:
		return set, fmt.Sprintf("%.2gMP", mp), fmt.Sprintf("%.2gMP", fmp)
	}
	return set, "", ""
}
