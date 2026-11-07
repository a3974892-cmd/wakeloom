package main

// Minimal Colab client for local mode, mirroring what google-colab-cli does (Apache-2.0):
// /tun/m/* on colab.research.google.com to assign/list/release runtimes and read the CU
// balance, then the runtime's Jupyter server (via the runtime proxy URL + token) to start a
// kernel, stream code execution over the kernel WebSocket and upload files.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
)

var colabDomain = firstNonEmpty(strings.TrimRight(os.Getenv("CKWB_COLAB_BASE"), "/"), "https://colab.research.google.com") // CKWB_COLAB_BASE: UI tests

type ColabError struct {
	Status int
	Body   string
}

func (e *ColabError) Error() string {
	switch e.Status {
	case 401, 403:
		return fmt.Sprintf("Colab 拒绝访问（%d），请重新登录 Google", e.Status)
	case 412:
		return "Colab 拒绝分配（可能已有太多运行中的会话，或这种 GPU 暂时没有额度/库存）"
	case 400:
		return "Colab 拒绝了这种 GPU（账号可能没有这个型号的权限或额度）：" + truncate(e.Body, 200)
	}
	return fmt.Sprintf("Colab 返回 %d：%s", e.Status, truncate(e.Body, 200))
}

type ProxyInfo struct {
	Token     string `json:"token"`
	ExpiresIn int    `json:"tokenExpiresInSeconds"`
	URL       string `json:"url"`
}

type Assignment struct {
	Accelerator string    `json:"accelerator"`
	Endpoint    string    `json:"endpoint"`
	Proxy       ProxyInfo `json:"runtimeProxyInfo"`
}

type CCUInfo struct {
	Balance     float64 `json:"currentBalance"`
	RatePerHour float64 `json:"consumptionRateHourly"`
	Assignments int     `json:"assignmentsCount"`
}

var colabHTTP = &http.Client{Transport: transport, Timeout: 5 * time.Minute}

func colabDo(ctx context.Context, method, rawURL string, headers map[string]string, out any) error {
	tok, err := googleAccess(ctx)
	if err != nil {
		return err
	}
	u, _ := url.Parse(rawURL)
	q := u.Query()
	q.Set("authuser", "0")
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, method, u.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Colab-Client-Agent", "colab-cli")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := colabHTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连不上 Colab：%w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return &ColabError{Status: resp.StatusCode, Body: string(raw)}
	}
	body := strings.TrimPrefix(string(raw), ")]}'\n")
	if out != nil && strings.TrimSpace(body) != "" {
		if err := json.Unmarshal([]byte(body), out); err != nil {
			return fmt.Errorf("Colab 返回了无法识别的数据：%s", truncate(body, 200))
		}
	}
	return nil
}

func colabCCU(ctx context.Context) (CCUInfo, error) {
	var c CCUInfo
	err := colabDo(ctx, "GET", colabDomain+"/tun/m/ccu-info", nil, &c)
	return c, err
}

func colabList(ctx context.Context) ([]Assignment, error) {
	var r struct {
		Assignments []Assignment `json:"assignments"`
	}
	err := colabDo(ctx, "GET", colabDomain+"/tun/m/assignments", nil, &r)
	return r.Assignments, err
}

func nbHash() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b)
	s := h[0:8] + "_" + h[8:12] + "_" + h[12:16] + "_" + h[16:20] + "_" + h[20:32]
	return s + strings.Repeat(".", 44-len(s))
}

// colabAssign allocates a runtime with the given accelerator ("L4", "G4", "A100", "T4", …).
func colabAssign(ctx context.Context, accelerator string) (*Assignment, error) {
	q := url.Values{"nbh": {nbHash()}, "variant": {"GPU"}, "accelerator": {accelerator}}
	if accelerator == "" || accelerator == "NONE" {
		q = url.Values{"nbh": {q.Get("nbh")}}
	}
	u := colabDomain + "/tun/m/assign?" + q.Encode()
	var probe struct {
		Token    string    `json:"token"`
		Endpoint string    `json:"endpoint"`
		Proxy    ProxyInfo `json:"runtimeProxyInfo"`
	}
	if err := colabDo(ctx, "GET", u, nil, &probe); err != nil {
		return nil, err
	}
	if probe.Endpoint != "" && probe.Proxy.URL != "" { // already assigned for this notebook
		return &Assignment{Accelerator: accelerator, Endpoint: probe.Endpoint, Proxy: probe.Proxy}, nil
	}
	var a Assignment
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = colabDo(ctx, "POST", u, map[string]string{"X-Goog-Colab-Token": probe.Token}, &a)
		var ce *ColabError
		if err == nil || !(errors.As(err, &ce) && (ce.Status == 502 || ce.Status == 503 || ce.Status == 504)) {
			break
		}
		time.Sleep(time.Duration(10<<attempt) * time.Second)
	}
	if err != nil {
		return nil, err
	}
	if a.Proxy.URL == "" {
		return nil, errors.New("Colab 分配成功但没有返回运行时地址")
	}
	return &a, nil
}

func colabUnassign(ctx context.Context, endpoint string) error {
	u := colabDomain + "/tun/m/unassign/" + url.PathEscape(endpoint)
	var t struct {
		Token string `json:"token"`
	}
	if err := colabDo(ctx, "GET", u, nil, &t); err != nil {
		return err
	}
	return colabDo(ctx, "POST", u, map[string]string{"X-Goog-Colab-Token": t.Token}, nil)
}

// refreshProxy re-reads the runtime proxy token (it expires) from the assignment list.
func refreshProxy(ctx context.Context, a *Assignment) error {
	list, err := colabList(ctx)
	if err != nil {
		return err
	}
	for _, x := range list {
		if x.Endpoint == a.Endpoint {
			a.Proxy = x.Proxy
			return nil
		}
	}
	return errors.New("这台 Colab 运行时已经不存在了（可能被回收）")
}

// ---- Jupyter on the runtime ----

type Runtime struct {
	A           *Assignment
	KernelID    string
	Session     string
	OnReconnect func() // e.g. refresh the runtime proxy token before redialing
}

func (rt *Runtime) rtReq(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	u := strings.TrimRight(rt.A.Proxy.URL, "/") + path
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	u += sep + "authuser=0&colab-runtime-proxy-token=" + url.QueryEscape(rt.A.Proxy.Token)
	req, _ := http.NewRequestWithContext(ctx, method, u, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Colab-Runtime-Proxy-Token", rt.A.Proxy.Token)
	req.Header.Set("X-Colab-Client-Agent", "colab-cli")
	resp, err := colabHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, &ColabError{Status: resp.StatusCode, Body: string(raw)}
	}
	return resp, nil
}

// StartKernel creates (or reuses) the runtime's Python kernel.
func (rt *Runtime) StartKernel(ctx context.Context) error {
	if rt.KernelID != "" {
		return nil
	}
	var lastErr error
	for i := 0; i < 6; i++ {
		resp, err := rt.rtReq(ctx, "GET", "/api/kernels", nil)
		if err == nil {
			var ks []struct {
				ID string `json:"id"`
			}
			json.NewDecoder(resp.Body).Decode(&ks)
			resp.Body.Close()
			if len(ks) > 0 {
				rt.KernelID = ks[0].ID
				return nil
			}
			resp, err = rt.rtReq(ctx, "POST", "/api/kernels", map[string]string{"name": "python3"})
			if err == nil {
				var k struct {
					ID string `json:"id"`
				}
				json.NewDecoder(resp.Body).Decode(&k)
				resp.Body.Close()
				if k.ID != "" {
					rt.KernelID = k.ID
					return nil
				}
			}
		}
		lastErr = err
		time.Sleep(time.Duration(3+i*2) * time.Second)
	}
	return fmt.Errorf("启动 Colab 内核失败：%v", lastErr)
}

type jmsg struct {
	Header       map[string]any `json:"header"`
	ParentHeader map[string]any `json:"parent_header"`
	Metadata     map[string]any `json:"metadata"`
	Content      map[string]any `json:"content"`
	Channel      string         `json:"channel"`
	Buffers      []any          `json:"buffers"`
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (rt *Runtime) dialKernel(ctx context.Context) (*websocket.Conn, error) {
	if rt.Session == "" {
		rt.Session = randHex(16)
	}
	base := strings.TrimRight(rt.A.Proxy.URL, "/")
	wsURL := strings.Replace(strings.Replace(base, "https://", "wss://", 1), "http://", "ws://", 1) +
		"/api/kernels/" + rt.KernelID + "/channels?session_id=" + rt.Session +
		"&authuser=0&colab-runtime-proxy-token=" + url.QueryEscape(rt.A.Proxy.Token)
	h := http.Header{}
	h.Set("X-Colab-Runtime-Proxy-Token", rt.A.Proxy.Token)
	h.Set("X-Colab-Client-Agent", "colab-cli")
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: h, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		return nil, fmt.Errorf("连接 Colab 内核失败：%w", err)
	}
	c.SetReadLimit(64 << 20)
	return c, nil
}

// Exec runs code in the kernel and streams stdout/stderr lines to onLine. It returns when the
// cell finishes; a Python exception is returned as an error. If the WebSocket drops while the
// cell is still running (long boots), it reconnects and keeps following the same execution:
// the kernel broadcasts iopub output to every connected client.
func (rt *Runtime) Exec(ctx context.Context, code string, onLine func(string)) error {
	if err := rt.StartKernel(ctx); err != nil {
		return err
	}
	c, err := rt.dialKernel(ctx)
	if err != nil {
		return err
	}
	defer func() { c.CloseNow() }()

	msgID := randHex(16)
	req := jmsg{
		Header: map[string]any{"msg_id": msgID, "username": "workbench", "session": rt.Session, "msg_type": "execute_request",
			"version": "5.3", "date": time.Now().UTC().Format(time.RFC3339)},
		ParentHeader: map[string]any{}, Metadata: map[string]any{}, Channel: "shell", Buffers: []any{},
		Content: map[string]any{"code": code, "silent": false, "store_history": false, "user_expressions": map[string]any{},
			"allow_stdin": false, "stop_on_error": true},
	}
	b, _ := json.Marshal(req)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		return err
	}
	var partial string
	emit := func(s string) {
		s = partial + s
		lines := strings.Split(s, "\n")
		partial = lines[len(lines)-1]
		for _, l := range lines[:len(lines)-1] {
			onLine(strings.TrimRight(l, "\r"))
		}
	}
	gotReply, gotIdle := false, false
	var execErr error
	redials := 0
	for !(gotReply && gotIdle) {
		_, data, err := c.Read(ctx)
		if err != nil {
			if ctx.Err() != nil || redials >= 8 {
				if partial != "" {
					onLine(partial)
				}
				return fmt.Errorf("和 Colab 内核的连接断开了：%w", err)
			}
			redials++
			time.Sleep(3 * time.Second)
			if rt.OnReconnect != nil {
				rt.OnReconnect()
			}
			c.CloseNow()
			nc, derr := rt.dialKernel(ctx)
			if derr != nil {
				continue
			}
			c = nc
			continue
		}
		var m jmsg
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if pid, _ := m.ParentHeader["msg_id"].(string); pid != msgID {
			continue
		}
		switch m.Header["msg_type"] {
		case "stream":
			if t, ok := m.Content["text"].(string); ok {
				emit(t)
			}
		case "error":
			en, _ := m.Content["ename"].(string)
			ev, _ := m.Content["evalue"].(string)
			execErr = fmt.Errorf("%s: %s", en, ev)
		case "execute_reply":
			gotReply = true
			if st, _ := m.Content["status"].(string); st == "error" && execErr == nil {
				en, _ := m.Content["ename"].(string)
				ev, _ := m.Content["evalue"].(string)
				execErr = fmt.Errorf("%s: %s", en, ev)
			}
		case "status":
			if st, _ := m.Content["execution_state"].(string); st == "idle" {
				gotIdle = true
			}
		}
	}
	if partial != "" {
		onLine(partial)
	}
	c.Close(websocket.StatusNormalClosure, "")
	return execErr
}

// Upload writes a file to the runtime through the Jupyter contents API (path relative to /content).
func (rt *Runtime) Upload(ctx context.Context, data []byte, remotePath string) error {
	remotePath = strings.TrimPrefix(remotePath, "/") // Colab's Jupyter root is "/"
	name := remotePath[strings.LastIndex(remotePath, "/")+1:]
	body := map[string]any{"name": name, "path": remotePath, "type": "file", "format": "base64",
		"content": base64.StdEncoding.EncodeToString(data), "chunk": 1}
	resp, err := rt.rtReq(ctx, "PUT", "/api/contents/"+(&url.URL{Path: remotePath}).EscapedPath(), body)
	if err != nil {
		return fmt.Errorf("上传到 Colab 失败：%w", err)
	}
	resp.Body.Close()
	return nil
}
