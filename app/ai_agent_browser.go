package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Only a dedicated browser profile is controlled; never attach to daily sessions.
type agentBrowser struct {
	cmd     *exec.Cmd
	conn    *websocket.Conn
	session string
	id      int
	closed  chan struct{}
	cleanup func()
}

func startAgentBrowser(ctx context.Context, dir string, headless bool) (*agentBrowser, error) {
	exe := agentBrowserExecutable()
	if exe == "" {
		return nil, errors.New("未找到 Edge 或 Chrome，请先安装浏览器")
	}
	profile := filepath.Join(dir, "browser")
	if e := os.MkdirAll(profile, 0700); e != nil {
		return nil, e
	}
	args := []string{"--user-data-dir=" + profile, "--remote-debugging-port=0", "--remote-debugging-address=127.0.0.1", "--no-first-run", "--no-default-browser-check", "--disable-background-mode", "--new-window", "about:blank"}
	if headless {
		args = append(args, "--headless=new")
	}
	cmd := exec.Command(exe, args...)
	agentHide(cmd)
	cleanup, e := agentManagedStart(cmd)
	if e != nil {
		return nil, e
	}
	b := &agentBrowser{cmd: cmd, closed: make(chan struct{}), cleanup: cleanup}
	go func() { _ = cmd.Wait(); close(b.closed) }()
	ok := false
	defer func() {
		if !ok {
			b.close()
		}
	}()
	for i := 0; i < 100; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		raw, e := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if e == nil && len(lines) >= 2 {
			port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
			if err != nil || port < 1 || port > 65535 {
				return nil, errors.New("浏览器调试端口无效")
			}
			endpoint := strings.TrimSpace(lines[1])
			if !strings.HasPrefix(endpoint, "/devtools/browser/") {
				return nil, errors.New("浏览器调试地址无效")
			}
			conn, _, err := websocket.Dial(ctx, fmt.Sprintf("ws://127.0.0.1:%d%s", port, endpoint), nil)
			if err != nil {
				return nil, err
			}
			b.conn = conn
			conn.SetReadLimit(4 << 20)
			var target struct {
				TargetID string `json:"targetId"`
			}
			if e = b.call(ctx, "Target.createTarget", map[string]any{"url": "about:blank"}, &target); e != nil {
				return nil, e
			}
			var attached struct {
				SessionID string `json:"sessionId"`
			}
			if e = b.call(ctx, "Target.attachToTarget", map[string]any{"targetId": target.TargetID, "flatten": true}, &attached); e != nil {
				return nil, e
			}
			b.session = attached.SessionID
			ok = true
			return b, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-b.closed:
			return nil, errors.New("浏览器启动失败")
		case <-time.After(100 * time.Millisecond):
		}
	}
	return nil, errors.New("浏览器启动超时")
}
func (b *agentBrowser) close() {
	if b.conn != nil {
		_ = b.conn.CloseNow()
	}
	if b.cleanup != nil {
		b.cleanup()
		b.cleanup = nil
	}
}
func (b *agentBrowser) call(ctx context.Context, method string, params any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	b.id++
	msg := map[string]any{"id": b.id, "method": method, "params": params}
	if b.session != "" {
		msg["sessionId"] = b.session
	}
	raw, _ := json.Marshal(msg)
	if e := b.conn.Write(ctx, websocket.MessageText, raw); e != nil {
		return e
	}
	for {
		_, raw, e := b.conn.Read(ctx)
		if e != nil {
			return e
		}
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if e = json.Unmarshal(raw, &reply); e != nil {
			return e
		}
		if reply.ID != b.id {
			continue
		}
		if reply.Error != nil {
			return errors.New(reply.Error.Message)
		}
		if result != nil {
			return json.Unmarshal(reply.Result, result)
		}
		return nil
	}
}
func (b *agentBrowser) evaluate(ctx context.Context, expression string) (string, error) {
	var result struct {
		Result struct {
			Value json.RawMessage `json:"value"`
		} `json:"result"`
		Exception json.RawMessage `json:"exceptionDetails"`
	}
	if e := b.call(ctx, "Runtime.evaluate", map[string]any{"expression": expression, "returnByValue": true, "awaitPromise": true}, &result); e != nil {
		return "", e
	}
	if len(result.Exception) > 0 {
		return "", fmt.Errorf("网页操作失败：%s", agentClip(string(result.Exception), 1000))
	}
	var text string
	if json.Unmarshal(result.Result.Value, &text) == nil {
		return text, nil
	}
	return string(result.Result.Value), nil
}

const agentDOMSnapshot = `(()=>{const visible=e=>{const r=e.getBoundingClientRect();return r.width>0&&r.height>0&&getComputedStyle(e).visibility!=='hidden'};
const nodes=Array.from(document.querySelectorAll('a,button,input,textarea,select,[role="button"],[contenteditable="true"]')).filter(visible).slice(0,150);
const nonce=Math.random().toString(36).slice(2);window.__wbAgent={nonce,nodes};
return JSON.stringify({url:location.href,title:document.title,text:(document.body?.innerText||'').slice(0,6500),elements:nodes.map((e,i)=>({ref:nonce+':'+i,tag:e.tagName,type:e.type||'',name:(e.getAttribute('aria-label')||e.innerText||e.placeholder||e.name||'').slice(0,120),disabled:!!e.disabled}))})})()`

func (b *agentBrowser) action(ctx context.Context, a agentAction) (string, error) {
	switch a.Tool {
	case "browser_open":
		u, e := url.Parse(a.URL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
			return "", errors.New("浏览器仅接受不含账号密码的 HTTP/HTTPS 地址")
		}
		var result map[string]any
		if e = b.call(ctx, "Page.navigate", map[string]any{"url": a.URL}, &result); e != nil {
			return "", e
		}
		if v, ok := result["errorText"].(string); ok && v != "" {
			return "", errors.New(v)
		}
		for i := 0; i < 30; i++ {
			state, e := b.evaluate(ctx, "document.readyState")
			if e == nil && (state == "interactive" || state == "complete") {
				break
			}
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	case "browser_read":
	case "browser_click", "browser_type", "browser_enter":
		ref, _ := json.Marshal(a.Ref)
		value, _ := json.Marshal(a.Text)
		check := `const ref=` + string(ref) + `;const [nonce,index]=ref.split(':');const state=window.__wbAgent;if(!state||state.nonce!==nonce||!/^\d+$/.test(index))throw Error('Stale ref; use browser_read');const e=state.nodes[Number(index)];if(!e||!e.isConnected||e.disabled)throw Error('Element unavailable; use browser_read');e.scrollIntoView({block:'center'});e.focus();`
		action := `e.click();`
		if a.Tool == "browser_type" {
			action = `if(e.type==='password')throw Error('Password input requires the user');const value=` + string(value) + `;if(e instanceof HTMLInputElement||e instanceof HTMLTextAreaElement||e instanceof HTMLSelectElement){const proto=e instanceof HTMLTextAreaElement?HTMLTextAreaElement.prototype:e instanceof HTMLSelectElement?HTMLSelectElement.prototype:HTMLInputElement.prototype;Object.getOwnPropertyDescriptor(proto,'value').set.call(e,value)}else if(e.isContentEditable){e.textContent=value}else throw Error('Not an editable field');e.dispatchEvent(new Event('input',{bubbles:true}));e.dispatchEvent(new Event('change',{bubbles:true}));`
		}
		if a.Tool == "browser_enter" {
			action = ""
		}
		if _, e := b.evaluate(ctx, "(()=>{"+check+action+"return 'ok'})()"); e != nil {
			return "", e
		}
		if a.Tool == "browser_enter" {
			for _, kind := range []string{"keyDown", "keyUp"} {
				if e := b.call(ctx, "Input.dispatchKeyEvent", map[string]any{"type": kind, "key": "Enter", "code": "Enter", "windowsVirtualKeyCode": 13}, nil); e != nil {
					return "", e
				}
			}
		}
	default:
		return "", errors.New("未知浏览器操作")
	}
	return b.evaluate(ctx, agentDOMSnapshot)
}
