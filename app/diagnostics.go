package main

// Diagnostics are local, bounded and redacted before being written. Never store
// request bodies, chat messages, attachments or a serialized configuration here.
import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var BuildLabel string // optionally set by a test build's linker flags

type diagnosticEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Source  string `json:"source"`
	Message string `json:"message"`
}
type diagnosticStore struct {
	mu        sync.Mutex
	dir       string
	maxBytes  int64
	lastError string
}

var diagnostics atomic.Pointer[diagnosticStore]
var diagnosticSecrets struct {
	sync.RWMutex
	values []string
}

var diagURL = regexp.MustCompile(`https?://[^\s<>"']+`)
var diagAuthHeader = regexp.MustCompile(`(?im)\b(authorization|cookie|set-cookie)["']?\s*:\s*[^\r\n]+`)
var diagCredential = regexp.MustCompile(`(?i)((?:authorization|bearer|api[_-]?key|x-key|access[_-]?token|refresh[_-]?token|token|client[_-]?secret|cf-access-client-(?:id|secret)|password|secret|cookie|set-cookie|gVerifier|authorization_code)["']?\s*[:=]?\s*["']?)([^\s"',;}]+)`)
var diagToken = regexp.MustCompile(`\b(?:hf_[A-Za-z0-9]{8,}|sk-[A-Za-z0-9_-]{8,}|ya29\.[A-Za-z0-9_.-]+|eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+)\b`)
var diagEmail = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
var diagHome = regexp.MustCompile(`(?i)(?:[A-Z]:[\\/](?:Users|Documents and Settings)[\\/][^\\/\s"']+|/(?:home|Users)/[^/\s"']+)`)
var diagHTTPFailure = regexp.MustCompile(`(?i)\bHTTP(?:/\d(?:\.\d)?)?\s+(?:Error\s+)?[45][0-9]{2}\b`)

func rememberDiagnosticSecrets(values ...string) {
	diagnosticSecrets.Lock()
	defer diagnosticSecrets.Unlock()
	for _, v := range values {
		if len(v) < 4 || v == keyMask || v == "********" {
			continue
		}
		found := false
		for _, old := range diagnosticSecrets.values {
			if old == v {
				found = true
				break
			}
		}
		if !found {
			diagnosticSecrets.values = append(diagnosticSecrets.values, v)
		}
	}
	// Longer matches first, so a secret cannot be partially unmasked by another.
	sort.Slice(diagnosticSecrets.values, func(i, j int) bool { return len(diagnosticSecrets.values[i]) > len(diagnosticSecrets.values[j]) })
}
func rememberDiagnosticConfig(c Config) {
	values := []string{apiKey, c.Token, c.CFClientID, c.CFClientSecret, c.HFToken}
	for _, p := range c.LLMs {
		values = append(values, p.APIKey)
	}
	rememberDiagnosticSecrets(values...)
}
func redactDiagnostic(s string) string {
	diagnosticSecrets.RLock()
	for _, secret := range diagnosticSecrets.values {
		s = strings.ReplaceAll(s, secret, "[已隐藏]")
		s = strings.ReplaceAll(s, url.QueryEscape(secret), "[已隐藏]")
	}
	diagnosticSecrets.RUnlock()
	s = diagURL.ReplaceAllStringFunc(s, func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[URL 已隐藏]"
		}
		u.User = nil
		u.RawQuery = ""
		u.Fragment = ""
		u.ForceQuery = false
		return u.String()
	})
	s = diagAuthHeader.ReplaceAllString(s, "${1}: [已隐藏]")
	s = diagCredential.ReplaceAllString(s, "${1}[已隐藏]")
	s = diagToken.ReplaceAllString(s, "[令牌已隐藏]")
	s = diagEmail.ReplaceAllString(s, "[邮箱已隐藏]")
	return diagHome.ReplaceAllString(s, "[用户目录]")
}
func diagnosticLevel(s string) string {
	if diagHTTPFailure.MatchString(s) {
		return "error"
	}
	lower := strings.ToLower(s)
	for _, word := range []string{"failed", "failure", "traceback", "error", "exception", "失败", "错误", "异常", "timeout", "timed out", "超时"} {
		if strings.Contains(lower, word) {
			return "error"
		}
	}
	if strings.Contains(lower, "warn") || strings.Contains(lower, "retry") || strings.Contains(lower, "重试") {
		return "warn"
	}
	return "info"
}
func diagEvent(level, source, message string) {
	if d := diagnostics.Load(); d != nil {
		d.add(level, source, message)
	}
}
func (d *diagnosticStore) add(level, source, message string) {
	message = redactDiagnostic(strings.TrimSpace(message))
	if len(message) > 24000 {
		message = string([]rune(message)[:min(6000, len([]rune(message)))]) + "\n[长日志已截短]"
	}
	if message == "" {
		return
	}
	entry := diagnosticEntry{time.Now().Format(time.RFC3339Nano), level, source, message}
	b, _ := json.Marshal(entry)
	b = append(b, '\n')
	d.mu.Lock()
	defer d.mu.Unlock()
	path := filepath.Join(d.dir, "diagnostics.jsonl")
	if err := os.MkdirAll(d.dir, 0o700); err != nil {
		d.lastError = "无法创建日志目录"
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size()+int64(len(b)) > d.maxBytes {
		if err := os.Remove(path + ".2"); err != nil && !os.IsNotExist(err) {
			d.lastError = "无法轮换日志"
			return
		}
		if err := os.Rename(path+".1", path+".2"); err != nil && !os.IsNotExist(err) {
			d.lastError = "无法轮换日志"
			return
		}
		if err := os.Rename(path, path+".1"); err != nil {
			d.lastError = "无法轮换日志"
			return
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		d.lastError = "无法写入日志"
		return
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		d.lastError = "日志写入失败"
	} else {
		d.lastError = ""
	}
}
func (d *diagnosticStore) snapshot() ([]diagnosticEntry, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := make([]diagnosticEntry, 0)
	for _, suffix := range []string{".2", ".1", ""} {
		f, err := os.Open(filepath.Join(d.dir, "diagnostics.jsonl") + suffix)
		if err != nil {
			continue
		}
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 4096), 256*1024)
		for scan.Scan() {
			var e diagnosticEntry
			if json.Unmarshal(scan.Bytes(), &e) == nil {
				e.Message = redactDiagnostic(e.Message)
				entries = append(entries, e)
			}
		}
		if scan.Err() != nil {
			d.lastError = "部分日志未能读取"
		}
		f.Close()
	}
	if len(entries) > 3000 {
		entries = entries[len(entries)-3000:]
	}
	return entries, d.lastError
}

type diagnosticWriter struct{}

func (diagnosticWriter) Write(p []byte) (int, error) {
	diagEvent(diagnosticLevel(string(p)), "app", string(p))
	return len(p), nil
}
func initDiagnostics() {
	rememberDiagnosticConfig(getConfig())
	gMu.Lock()
	if gTok != nil {
		rememberDiagnosticSecrets(gTok.AccessToken, gTok.RefreshToken)
	}
	gMu.Unlock()
	diagnostics.Store(&diagnosticStore{dir: filepath.Join(dataDir, "logs"), maxBytes: 2 * 1024 * 1024})
	log.SetOutput(diagnosticWriter{})
	label := BuildLabel
	if label == "" {
		label = AppVersion
	}
	diagEvent("info", "app", fmt.Sprintf("启动 Wakeloom %s · %s/%s · %s · mode=%s", label, runtime.GOOS, runtime.GOARCH, runtime.Version(), getConfig().Mode))
}

type diagnosticResponse struct {
	http.ResponseWriter
	status int
}

func (w *diagnosticResponse) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		w.ResponseWriter.WriteHeader(code)
	}
}
func (w *diagnosticResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(p)
}
func (w *diagnosticResponse) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *diagnosticResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func diagnosticHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		dw := &diagnosticResponse{ResponseWriter: w}
		defer func() {
			if p := recover(); p != nil {
				diagEvent("error", "http", fmt.Sprintf("%s %s panic: %v\n%s", r.Method, r.URL.Path, p, debug.Stack()))
				panic(p)
			}
			if strings.HasPrefix(r.URL.Path, "/api/diagnostics") {
				return
			}
			if dw.status >= 400 || r.Method != "GET" {
				level := "info"
				if dw.status >= 400 {
					level = "error"
				}
				endpoint := r.Pattern
				if endpoint == "" {
					endpoint = r.Method + " " + r.URL.Path
				}
				diagEvent(level, "http", fmt.Sprintf("%s → HTTP %d · %d ms", endpoint, dw.status, time.Since(start).Milliseconds()))
			}
		}()
		next.ServeHTTP(dw, r)
	})
}
func diagnosticGatewayEvent(event []byte) {
	var kind, data string
	for _, line := range strings.Split(string(event), "\n") {
		if strings.HasPrefix(line, "event:") {
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
		if strings.HasPrefix(line, "data:") {
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	if kind == "log" {
		var line string
		if json.Unmarshal([]byte(data), &line) == nil {
			diagEvent(diagnosticLevel(line), "gateway", line)
		}
	}
	if kind == "state" {
		var s struct {
			State   string `json:"state"`
			Reason  string `json:"reason"`
			Profile string `json:"profile"`
		}
		if json.Unmarshal([]byte(data), &s) == nil {
			diagEvent(diagnosticLevel(s.State+" "+s.Reason), "gateway", fmt.Sprintf("state=%s profile=%s %s", s.State, s.Profile, s.Reason))
		}
	}
}
func registerDiagnosticRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/diagnostics", func(w http.ResponseWriter, r *http.Request) {
		entries := []diagnosticEntry{}
		warning := ""
		if d := diagnostics.Load(); d != nil {
			entries, warning = d.snapshot()
		}
		label := BuildLabel
		if label == "" {
			label = AppVersion
		}
		var report strings.Builder
		fmt.Fprintf(&report, "Wakeloom 诊断日志\n版本：%s\n系统：%s/%s · %s\n连接方式：%s\n导出时间：%s\n已过滤常见令牌、邮箱和用户目录；分享前请检查预览。\n只包含软件运行期间收集的最近日志，未附带配置、聊天或附件文件。\n\n", label, runtime.GOOS, runtime.GOARCH, runtime.Version(), getConfig().Mode, time.Now().Format(time.RFC3339))
		for _, e := range entries {
			fmt.Fprintf(&report, "%s [%s] [%s] %s\n", e.Time, strings.ToUpper(e.Level), e.Source, e.Message)
		}
		if warning != "" {
			fmt.Fprintf(&report, "\n日志状态：%s\n", warning)
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonOut(w, 200, map[string]any{"entries": entries, "report": report.String(), "warning": warning})
	})
	api.HandleFunc("POST /api/diagnostics/client", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		var e struct {
			Message string `json:"message"`
			Stack   string `json:"stack"`
		}
		if json.NewDecoder(r.Body).Decode(&e) != nil {
			http.Error(w, "invalid diagnostic event", 400)
			return
		}
		diagEvent("error", "ui", e.Message+"\n"+e.Stack)
		jsonOut(w, 200, map[string]bool{"ok": true})
	})
}
