package main

// First-run guide: progress, the getting-started checklist, the network check and CU-per-hour estimates.
// The UI lives in ui/onboarding.js.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// checklist items, in display order
var checklistItems = []string{"network", "login", "wake", "generate", "stop"}

// cuRateGuess is a rough Colab compute-unit price per hour by GPU, used before Wakeloom has seen the real
// rate (Colab reports it while a GPU runs; learnCURate keeps the latest real value per GPU in the config).
var cuRateGuess = map[string]float64{"T4": 1.8, "L4": 4.8, "G4": 10, "A100": 11.8, "H100": 18}

func cuRate(acc string) (rate float64, measured bool) {
	acc = strings.ToUpper(strings.TrimSpace(acc))
	if r := getConfig().CURates[acc]; r > 0 {
		return r, true
	}
	return cuRateGuess[acc], false
}

var obMu sync.Mutex // serialises read-modify-write of the onboarding fields

func updateConfig(f func(c *Config)) error {
	obMu.Lock()
	defer obMu.Unlock()
	c := getConfig()
	if c.Checklist != nil { // copy: getConfig shares the map with the stored config
		m := make(map[string]int64, len(c.Checklist))
		for k, v := range c.Checklist {
			m[k] = v
		}
		c.Checklist = m
	}
	if c.CURates != nil {
		m := make(map[string]float64, len(c.CURates))
		for k, v := range c.CURates {
			m[k] = v
		}
		c.CURates = m
	}
	f(&c)
	return saveConfig(c)
}

// markChecklist ticks a getting-started item the first time it happens.
func markChecklist(item string) {
	if getConfig().Checklist[item] != 0 {
		return
	}
	updateConfig(func(c *Config) {
		if c.Checklist == nil {
			c.Checklist = map[string]int64{}
		}
		if c.Checklist[item] == 0 {
			c.Checklist[item] = time.Now().Unix()
		}
	})
}

// learnCURate remembers the real CU rate Colab reports for the running GPU.
func learnCURate(st map[string]any) {
	if fmt.Sprint(st["state"]) != "ready" {
		return
	}
	acc := strings.ToUpper(fmt.Sprint(st["accelerator"]))
	cu, _ := st["cu"].(map[string]any)
	rate, _ := cu["per_hour"].(float64)
	if acc == "" || acc == "<NIL>" || rate <= 0 || rate > 100 {
		return
	}
	if old := getConfig().CURates[acc]; math.Abs(old-rate) < 0.01 {
		return
	}
	updateConfig(func(c *Config) {
		if c.CURates == nil {
			c.CURates = map[string]float64{}
		}
		c.CURates[acc] = math.Round(rate*100) / 100
	})
}

// migrateOnboarding: users who already set up the app before the guide existed never see it.
// Called once at start, after the config and the Google token are loaded. hadConfig = config.json existed.
func migrateOnboarding(hadConfig bool) {
	c := getConfig()
	if c.Onboarded || !hadConfig {
		return
	}
	signedIn, _ := googleSignedIn()
	if strings.TrimSpace(c.GateURL) != "" || signedIn {
		updateConfig(func(c *Config) {
			c.Onboarded = true
			if c.Checklist == nil {
				c.Checklist = map[string]int64{}
			}
			for _, k := range checklistItems { // an existing user does not need the beginner checklist
				if c.Checklist[k] == 0 {
					c.Checklist[k] = -1
				}
			}
		})
	}
}

func onboardingState() map[string]any {
	c := getConfig()
	signedIn, email := googleSignedIn()
	items := []map[string]any{}
	done := 0
	for _, k := range checklistItems {
		ok := c.Checklist[k] != 0
		if k == "login" && (signedIn && c.Mode == "local") {
			ok = true
		}
		if ok {
			done++
		}
		items = append(items, map[string]any{"id": k, "done": ok})
	}
	rates := map[string]any{}
	for acc := range cuRateGuess {
		r, m := cuRate(acc)
		rates[acc] = map[string]any{"rate": r, "measured": m}
	}
	return map[string]any{"onboarded": c.Onboarded, "step": c.OnboardStep, "checklist": items, "all_done": done == len(checklistItems),
		"checklist_hidden": c.ChecklistHidden, "rates": rates, "signed_in": signedIn, "email": email, "mode": firstNonEmpty(c.Mode, "gateway"),
		"gate_url": c.GateURL, "out_dir": c.OutDir, "default_out_dir": defaultOutDir(), "local_idle_min": c.LocalIdleMin}
}

// ---------------- network check ----------------

type netTarget struct{ Name, URL, Why string }

// netTargets can be replaced in tests.
var netTargets = []netTarget{
	{"accounts.google.com", "https://accounts.google.com/", "登录 Google（本地模式）"},
	{"colab.research.google.com", "https://colab.research.google.com/", "开关 Colab GPU（本地模式）"},
	{"huggingface.co", "https://huggingface.co/", "「添加模型」识别模型"},
	{"github.com", "https://github.com/", "软件自动更新"},
}

type netResult struct {
	Name  string `json:"name"`
	Why   string `json:"why"`
	OK    bool   `json:"ok"`
	MS    int64  `json:"ms"`
	Error string `json:"error,omitempty"`
}

func netCheck(ctx context.Context) map[string]any {
	cl := &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res := make([]netResult, len(netTargets))
	var wg sync.WaitGroup
	for i, t := range netTargets {
		wg.Add(1)
		go func(i int, t netTarget) {
			defer wg.Done()
			r := netResult{Name: t.Name, Why: t.Why}
			t0 := time.Now()
			req, _ := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
			req.Header.Set("User-Agent", userAgent)
			resp, err := cl.Do(req)
			r.MS = time.Since(t0).Milliseconds()
			if err != nil {
				r.Error = netErrCN(err)
			} else {
				resp.Body.Close()
				r.OK = resp.StatusCode < 500 // any answer means the site is reachable
				if !r.OK {
					r.Error = "服务器返回 " + resp.Status
				}
			}
			res[i] = r
		}(i, t)
	}
	wg.Wait()
	allOK := true
	for _, r := range res {
		allOK = allOK && r.OK
	}
	advice := ""
	if !allOK {
		advice = "有网站连不上：请打开代理（软件会自动走系统代理），或在「设置 → 网关连接 → 网络代理」填写代理地址，例如 http://127.0.0.1:7890（按你代理软件的端口）。检测失败不影响继续，但对应功能可能用不了。"
	} else {
		markChecklist("network")
	}
	label := "直连"
	if req, _ := http.NewRequest("GET", "https://accounts.google.com/", nil); req != nil {
		if u, _ := proxyFor(req); u != nil {
			label = "经代理 " + u.Host
		}
	}
	return map[string]any{"ok": allOK, "proxy": label, "items": res, "advice": advice}
}

func netErrCN(err error) string {
	s := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(s, "Timeout") || strings.Contains(s, "timeout"):
		return "超时"
	case strings.Contains(s, "no such host"):
		return "域名解析失败"
	case strings.Contains(s, "refused"):
		return "连接被拒绝（代理没开？）"
	case strings.Contains(s, "Forbidden") || strings.Contains(s, "403"):
		return "被代理拒绝（403）：代理不允许访问这个网站"
	case strings.Contains(s, "proxyconnect"):
		return "连不上代理：" + truncate(s, 120)
	case strings.Contains(s, "reset") || strings.Contains(s, "EOF"):
		return "连接被重置"
	}
	return truncate(s, 160)
}

// ---------------- API ----------------

func registerOnboardingRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/onboarding", func(w http.ResponseWriter, r *http.Request) {
		jsonOut(w, 200, onboardingState())
	})
	api.HandleFunc("POST /api/onboarding", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Step      *string `json:"step"`
			Onboarded *bool   `json:"onboarded"`
			Hidden    *bool   `json:"checklist_hidden"`
			Mark      string  `json:"mark"`
		}
		json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		if in.Mark != "" {
			for _, k := range checklistItems {
				if k == in.Mark {
					markChecklist(k)
				}
			}
		}
		updateConfig(func(c *Config) {
			if in.Step != nil {
				c.OnboardStep = truncate(*in.Step, 40)
			}
			if in.Onboarded != nil {
				c.Onboarded = *in.Onboarded
			}
			if in.Hidden != nil {
				c.ChecklistHidden = *in.Hidden
			}
		})
		jsonOut(w, 200, onboardingState())
	})
	api.HandleFunc("GET /api/netcheck", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		jsonOut(w, 200, netCheck(ctx))
	})
}

// configExisted is set at start: whether config.json was there before this run.
func configExisted() bool {
	_, err := os.Stat(filepath.Join(dataDir, "config.json"))
	return err == nil
}
