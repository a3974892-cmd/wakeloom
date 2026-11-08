package main

// Local-mode library of on-demand custom nodes / models. Same item shape as the gate's
// library.py so the VM-side driver can merge them into the boot plan or a fetch plan.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type LibItem struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"` // node | model
	Title    string  `json:"title"`
	URL      string  `json:"url"`
	Dest     string  `json:"dest,omitempty"`
	Ref      string  `json:"ref,omitempty"`
	Subdir   string  `json:"subdir,omitempty"`
	SizeGB   float64 `json:"size_gb,omitempty"`
	AutoWake bool    `json:"auto_wake"`
	License  string  `json:"license,omitempty"`
	Note     string  `json:"note,omitempty"`
}

var (
	libMu    sync.Mutex
	slugRe   = regexp.MustCompile(`[^a-z0-9_-]+`)
	destRe   = regexp.MustCompile(`^[A-Za-z0-9_.\- ]+(/[A-Za-z0-9_.\- ]+)+$`)
	secretRe = regexp.MustCompile(`(?i)(hf_[A-Za-z0-9]{20,}|ghp_[A-Za-z0-9]{20,}|token=|access_token|ya29\.)`)
)

func libPath() string { return filepath.Join(dataDir, "library.json") }

func libLoad() []LibItem {
	libMu.Lock()
	defer libMu.Unlock()
	var d struct {
		Items []LibItem `json:"items"`
	}
	if b, err := os.ReadFile(libPath()); err == nil {
		json.Unmarshal(b, &d)
	}
	if d.Items == nil {
		d.Items = []LibItem{}
	}
	return d.Items
}

func libSave(items []LibItem) error {
	libMu.Lock()
	defer libMu.Unlock()
	return writeJSON(libPath(), map[string]any{"items": items})
}

func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	if len(s) > 48 {
		s = s[:48]
	}
	if s == "" {
		s = "item"
	}
	return s
}

func hfToHTTPS(u string) string {
	if !strings.HasPrefix(u, "hf://") {
		return u
	}
	parts := strings.SplitN(u[5:], "/", 3)
	if len(parts) < 3 {
		return u
	}
	repo, rev, _ := strings.Cut(parts[1], "@")
	if rev == "" {
		rev = "main"
	}
	return "https://huggingface.co/" + parts[0] + "/" + repo + "/resolve/" + rev + "/" + parts[2]
}

func libNormalize(in LibItem) (LibItem, error) {
	it := LibItem{Kind: strings.TrimSpace(in.Kind), URL: strings.TrimSpace(in.URL), AutoWake: in.AutoWake}
	if it.Kind != "node" && it.Kind != "model" {
		return it, errors.New("类型必须是插件或模型")
	}
	if !strings.HasPrefix(it.URL, "https://") && !strings.HasPrefix(it.URL, "hf://") {
		return it, errors.New("地址必须以 https:// 或 hf:// 开头")
	}
	if secretRe.MatchString(it.URL) {
		return it, errors.New("地址里像是带了密钥/令牌，请用公开链接")
	}
	title := strings.TrimSpace(in.Title)
	if it.Kind == "node" {
		base := strings.TrimSuffix(it.URL[strings.LastIndex(strings.TrimRight(it.URL, "/"), "/")+1:], ".git")
		base = strings.TrimRight(base, "/")
		if title == "" {
			title = base
		}
		it.ID = slugify(firstNonEmpty(in.ID, base))
		it.Ref, it.Subdir = strings.TrimSpace(in.Ref), strings.Trim(strings.TrimSpace(in.Subdir), "/")
		if strings.Contains(it.Ref+it.Subdir, "..") {
			return it, errors.New("ref/subdir 不合法")
		}
	} else {
		it.Dest = strings.Trim(strings.TrimSpace(in.Dest), "/")
		if !destRe.MatchString(it.Dest) || strings.Contains(it.Dest, "..") {
			return it, errors.New("保存位置要像「文件夹/文件名.safetensors」（相对 ComfyUI/models）")
		}
		if title == "" {
			title = it.Dest[strings.LastIndex(it.Dest, "/")+1:]
		}
		it.ID = slugify(firstNonEmpty(in.ID, strings.ReplaceAll(it.Dest, "/", "-")))
		it.SizeGB = in.SizeGB
		if it.SizeGB <= 0 {
			it.SizeGB = probeSizeGB(it.URL)
		}
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80])
	}
	it.Title, it.License, it.Note = title, strings.TrimSpace(in.License), strings.TrimSpace(in.Note)
	return it, nil
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func probeSizeGB(u string) float64 {
	req, _ := http.NewRequest("HEAD", hfToHTTPS(u), nil)
	req.Header.Set("User-Agent", "Wakeloom")
	resp, err := (&http.Client{Transport: transport, Timeout: 12 * time.Second}).Do(req)
	if err != nil {
		return 0
	}
	resp.Body.Close()
	if resp.ContentLength > 0 {
		return float64(int(float64(resp.ContentLength)/1e6)) / 1000
	}
	return 0
}
