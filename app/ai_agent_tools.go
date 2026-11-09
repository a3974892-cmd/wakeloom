package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Structured actions are interpreted here, never extracted from prose/code fences.
type agentAction struct {
	Tool    string `json:"tool"`
	Path    string `json:"path,omitempty"`
	Content string `json:"content,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Offset  int64  `json:"offset,omitempty"`
	Command string `json:"command,omitempty"`
	URL     string `json:"url,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Text    string `json:"text,omitempty"`
	Message string `json:"message,omitempty"`
}
type agentTools struct {
	root, logDir             string
	terminal, browserAllowed bool
	browser                  *agentBrowser
}

func agentClip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n[truncated; read a smaller range if needed]"
}
func agentRoot(p string) (string, error) {
	if strings.TrimSpace(p) == "" || !filepath.IsAbs(p) {
		return "", errors.New("请选择完整的工作目录路径")
	}
	p, e := filepath.EvalSymlinks(filepath.Clean(p))
	if e != nil {
		return "", e
	}
	st, e := os.Stat(p)
	if e != nil {
		return "", e
	}
	if !st.IsDir() {
		return "", errors.New("工作目录不是文件夹")
	}
	return p, nil
}

// This bounds file tools, not the optional shell (which runs as the current user).
func (t *agentTools) path(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.root, p)
	}
	p = filepath.Clean(p)
	within := func(v string) bool {
		r, e := filepath.Rel(t.root, v)
		return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(os.PathSeparator)) && !strings.Contains(r, ":") && !filepath.IsAbs(r)
	}
	if !within(p) {
		return "", errors.New("文件操作超出所选工作目录")
	}
	ancestor := p
	for {
		_, e := os.Lstat(ancestor)
		if e == nil {
			break
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", e
		}
		ancestor = parent
	}
	resolved, e := filepath.EvalSymlinks(ancestor)
	if e != nil {
		return "", e
	}
	if !within(resolved) {
		return "", errors.New("链接指向工作目录之外")
	}
	return p, nil
}
func agentHash(b []byte) string { v := sha256.Sum256(b); return hex.EncodeToString(v[:]) }
func (t *agentTools) execute(ctx context.Context, a agentAction) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	switch a.Tool {
	case "list_files":
		p, e := t.path(a.Path)
		if e != nil {
			return "", e
		}
		entries, e := os.ReadDir(p)
		if e != nil {
			return "", e
		}
		var b strings.Builder
		for i, entry := range entries {
			if i >= 200 {
				b.WriteString("[first 200 entries only]\n")
				break
			}
			kind := "file"
			if entry.IsDir() {
				kind = "dir"
			}
			fmt.Fprintf(&b, "%s\t%s\n", kind, entry.Name())
		}
		return b.String(), nil
	case "read_file":
		p, e := t.path(a.Path)
		if e != nil {
			return "", e
		}
		f, e := os.Open(p)
		if e != nil {
			return "", e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil {
			return "", e
		}
		if !st.Mode().IsRegular() {
			return "", errors.New("仅支持普通文件")
		}
		if st.Size() > 16<<20 {
			return "", errors.New("文件超过 16 MB，请通过附件读取或用终端提取所需部分")
		}
		if a.Offset < 0 || a.Offset > st.Size() {
			return "", errors.New("offset 超出文件范围")
		}
		raw, e := io.ReadAll(io.LimitReader(f, (16<<20)+1))
		if e != nil {
			return "", e
		}
		if len(raw) > 16<<20 || a.Offset > int64(len(raw)) {
			return "", errors.New("读取时文件发生变化，请重新读取")
		}
		if !utf8.Valid(raw) || strings.ContainsRune(string(raw), 0) {
			return "", errors.New("不是 UTF-8 文本，请使用程序附件静态分析")
		}
		if a.Offset < int64(len(raw)) && !utf8.RuneStart(raw[a.Offset]) {
			return "", errors.New("offset 必须位于 UTF-8 字符开头")
		}
		end := min(len(raw), int(a.Offset)+6000)
		for end < len(raw) && !utf8.RuneStart(raw[end]) {
			end++
		}
		return fmt.Sprintf("path=%s\nsha256=%s\nbytes=%d range=%d..%d\n%s", a.Path, agentHash(raw), len(raw), a.Offset, end, raw[a.Offset:int64(end)]), nil
	case "write_file":
		if len(a.Content) > 256<<10 {
			return "", errors.New("单次写入超过 256 KB")
		}
		p, e := t.path(a.Path)
		if e != nil {
			return "", e
		}
		if st, err := os.Stat(p); err == nil && (!st.Mode().IsRegular() || st.Size() > 16<<20) {
			return "", errors.New("只可覆盖不超过 16 MB 的普通文件")
		}
		old, e := os.ReadFile(p)
		exists := e == nil
		if e != nil && !os.IsNotExist(e) {
			return "", e
		}
		if exists {
			if a.SHA256 == "" || a.SHA256 != agentHash(old) {
				return "", errors.New("文件内容已变化或没有 sha256，请先 read_file 再修改")
			}
		} else if a.SHA256 != "new" {
			return "", errors.New("新建文件需 sha256=new")
		}
		if exists {
			if len(old) > 16<<20 {
				return "", errors.New("原文件过大，不能自动备份覆盖")
			}
			backup := filepath.Join(t.logDir, "backups", newID()+".bin")
			if e = os.MkdirAll(filepath.Dir(backup), 0700); e != nil {
				return "", e
			}
			if e = os.WriteFile(backup, old, 0600); e != nil {
				return "", e
			}
			index, _ := json.Marshal(map[string]string{"path": p, "backup": backup})
			if e = os.WriteFile(backup+".json", index, 0600); e != nil {
				return "", e
			}
		}
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return "", e
		}
		mode := os.FileMode(0600)
		if st, e := os.Stat(p); e == nil {
			mode = st.Mode().Perm()
		}
		if e = agentAtomicWrite(p, []byte(a.Content), mode); e != nil {
			return "", e
		}
		return "written sha256=" + agentHash([]byte(a.Content)), nil
	case "run_command":
		if !t.terminal {
			return "", errors.New("本次任务未启用终端")
		}
		if strings.TrimSpace(a.Command) == "" || len(a.Command) > 32768 {
			return "", errors.New("命令为空或过长")
		}
		runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return agentRunCommand(runCtx, t.root, a.Command)
	case "browser_open", "browser_read", "browser_click", "browser_type", "browser_enter":
		if !t.browserAllowed {
			return "", errors.New("本次任务未启用浏览器")
		}
		if t.browser == nil {
			b, e := startAgentBrowser(ctx, t.logDir, false)
			if e != nil {
				return "", e
			}
			t.browser = b
		}
		return t.browser.action(ctx, a)
	default:
		return "", errors.New("未知工具：" + a.Tool)
	}
}

func agentAtomicWrite(path string, raw []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".wakeloom-write-*")
	if e != nil {
		return e
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(raw)
	}
	if e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, path)
}

type agentOutput struct {
	mu        sync.Mutex
	b         []byte
	truncated bool
}

func (o *agentOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := len(p)
	left := 16000 - len(o.b)
	if len(p) > left {
		p = p[:max(0, left)]
		o.truncated = true
	}
	o.b = append(o.b, p...)
	return n, nil
}
func (o *agentOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	s := strings.ToValidUTF8(string(o.b), "�")
	if o.truncated {
		s += "\n[output truncated at 16 KB]"
	}
	return s
}
