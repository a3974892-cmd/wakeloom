package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Wakeloom/1.0"

var httpc = &http.Client{Timeout: 60 * time.Second}

type GateError struct {
	Status int
	Code   string
	Msg    string
	Raw    []byte
}

func (e *GateError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("%s (%d %s)", e.Msg, e.Status, e.Code)
	}
	return fmt.Sprintf("HTTP %d %s", e.Status, e.Code)
}

func baseURL() string {
	if localMode() {
		return lg.baseURL()
	}
	u := strings.TrimRight(strings.TrimSpace(getConfig().GateURL), "/")
	if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	return u
}

func addAuth(req *http.Request) {
	c := getConfig()
	req.Header.Set("User-Agent", userAgent)
	if localMode() {
		req.Header.Set("Authorization", "Bearer "+lg.key)
		return
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.CFClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.CFClientID)
		req.Header.Set("CF-Access-Client-Secret", c.CFClientSecret)
	}
}

func gateReq(method, path string, body io.Reader, ctype string) (*http.Response, error) {
	b := baseURL()
	if b == "" {
		return nil, &GateError{Status: 0, Code: "no_url", Msg: "还没有设置网关地址"}
	}
	req, err := http.NewRequest(method, b+path, body)
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	addAuth(req)
	return httpc.Do(req)
}

// gateJSON performs a request and decodes JSON; HTML responses mean Cloudflare Access intercepted us.
func gateJSON(method, path string, in any, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
		ct = "application/json"
	}
	var bodyBytes []byte
	if in != nil {
		bodyBytes, _ = json.Marshal(in)
	}
	resp, err := gateReq(method, path, body, ct)
	if err != nil && isTransient(err) {
		time.Sleep(700 * time.Millisecond)
		var b2 io.Reader
		if bodyBytes != nil {
			b2 = bytes.NewReader(bodyBytes)
		}
		resp, err = gateReq(method, path, b2, ct)
	}
	if err != nil {
		var ge *GateError
		if errors.As(err, &ge) {
			return err
		}
		return &GateError{Code: "network_error", Msg: "连不上网关（" + proxyLabel() + "）：" + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return &GateError{Status: resp.StatusCode, Code: "cf_access", Msg: "被 Cloudflare Access 拦截（返回了登录页）。请检查令牌，或在设置里填 Access 服务令牌"}
	}
	if resp.StatusCode >= 400 {
		var e struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &e) != nil {
			var e2 struct {
				Error struct{ Type, Message string } `json:"error"`
			}
			json.Unmarshal(raw, &e2)
			e.Error, e.Message = e2.Error.Type, e2.Error.Message
		}
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(raw))
			if len(e.Message) > 300 {
				e.Message = e.Message[:300]
			}
		}
		return &GateError{Status: resp.StatusCode, Code: e.Error, Msg: e.Message, Raw: raw}
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return &GateError{Status: resp.StatusCode, Code: "bad_json", Msg: "网关返回的不是 JSON"}
		}
	}
	return nil
}

func gateStatus() (map[string]any, error) {
	var st map[string]any
	err := gateJSON("GET", "/_gate/status", nil, &st)
	return st, err
}

func gateState() (string, error) {
	st, err := gateStatus()
	if err != nil {
		return "", err
	}
	s, _ := st["state"].(string)
	return s, nil
}

// uploadFile sends a local file to ComfyUI's input folder and returns the value LoadImage-style nodes expect.
func uploadFile(local string) (string, error) {
	f, err := os.Open(local)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := filepath.Base(local)
	// strip our "<jobid>_" storage prefix
	if i := strings.Index(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	fw, _ := mw.CreateFormFile("image", name)
	io.Copy(fw, f)
	mw.WriteField("type", "input")
	mw.WriteField("overwrite", "true")
	mw.Close()
	resp, err := gateReq("POST", "/upload/image", &buf, mw.FormDataContentType())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("上传 %s 失败：HTTP %d %s", name, resp.StatusCode, truncate(string(raw), 200))
	}
	var r struct {
		Name      string `json:"name"`
		Subfolder string `json:"subfolder"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", fmt.Errorf("上传返回异常：%s", truncate(string(raw), 200))
	}
	if r.Subfolder != "" {
		return r.Subfolder + "/" + r.Name, nil
	}
	return r.Name, nil
}

func downloadView(filename, subfolder, typ, dest string) error {
	q := url.Values{"filename": {filename}, "subfolder": {subfolder}, "type": {typ}}
	b := baseURL()
	req, _ := http.NewRequest("GET", b+"/view?"+q.Encode(), nil)
	addAuth(req)
	cl := &http.Client{Timeout: 30 * time.Minute, Transport: transport}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("下载 %s 失败：HTTP %d", filename, resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return fmt.Errorf("下载 %s 被 Cloudflare Access 拦截", filename)
	}
	os.MkdirAll(filepath.Dir(dest), 0o755)
	tmp := dest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	out.Close()
	return os.Rename(tmp, dest)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func isTransient(err error) bool {
	s := err.Error()
	return strings.Contains(s, "EOF") || strings.Contains(s, "connection reset") || strings.Contains(s, "forcibly closed")
}

// testGateway checks the VPS gateway settings (whatever the current mode) with a short timeout.
func testGateway(timeout time.Duration) (map[string]any, error) {
	c := getConfig()
	b := strings.TrimRight(strings.TrimSpace(c.GateURL), "/")
	if b == "" {
		return nil, &GateError{Code: "no_url", Msg: "还没有填网关地址"}
	}
	if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
		b = "https://" + b
	}
	req, _ := http.NewRequest("GET", b+"/_gate/status", nil)
	req.Header.Set("User-Agent", userAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.CFClientID != "" {
		req.Header.Set("CF-Access-Client-Id", c.CFClientID)
		req.Header.Set("CF-Access-Client-Secret", c.CFClientSecret)
	}
	resp, err := (&http.Client{Transport: transport, Timeout: timeout}).Do(req)
	if err != nil {
		return nil, &GateError{Code: "network_error", Msg: "连不上网关（" + proxyLabel() + "）：" + err.Error()}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return nil, &GateError{Status: resp.StatusCode, Code: "cf_access", Msg: "被 Cloudflare Access 拦截（返回了登录页），检查 Access 服务令牌"}
	}
	if resp.StatusCode >= 400 {
		return nil, &GateError{Status: resp.StatusCode, Code: "http", Msg: truncate(string(raw), 200)}
	}
	var st map[string]any
	if json.Unmarshal(raw, &st) != nil {
		return nil, &GateError{Status: resp.StatusCode, Code: "bad_json", Msg: "网关返回的不是 JSON"}
	}
	return st, nil
}
