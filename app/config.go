package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type Config struct {
	GateURL          string        `json:"gate_url"`
	Token            string        `json:"token"`
	CFClientID       string        `json:"cf_client_id"`
	CFClientSecret   string        `json:"cf_client_secret"`
	OutDir           string        `json:"out_dir"`
	AutoWake         bool          `json:"auto_wake"`
	WakeProfile      string        `json:"wake_profile"`
	AutoStopIdle     bool          `json:"auto_stop_when_queue_empty"`
	JobTimeoutMin    int           `json:"job_timeout_min"`
	DraftScale       float64       `json:"draft_scale"`
	DraftSteps       int           `json:"draft_steps"`
	Proxy            string        `json:"proxy"`
	LLMs             []LLMProvider `json:"llms"`
	DefaultLLM       string        `json:"default_llm"`
	Mode             string        `json:"mode"` // "gateway" (VPS comfy-gate) or "local" (drive Colab directly)
	LocalAccelerator string        `json:"local_accelerator"`
	LocalProfile     string        `json:"local_profile"`
	LocalIdleMin     int           `json:"local_idle_min"`
	LocalMaxHours    float64       `json:"local_max_hours"`
	UpdateURL        string        `json:"update_url"`
	HFToken          string        `json:"hf_token"`      // local mode: Hugging Face token for gated models (stays on this PC)
	UserdataSync     *bool         `json:"userdata_sync"` // back up / restore ComfyUI user data (nil = on)
	// first-run guide (onboarding.go); not part of the settings form, kept across settings saves
	Onboarded       bool               `json:"onboarded"`
	OnboardStep     string             `json:"onboard_step,omitempty"`
	Checklist       map[string]int64   `json:"checklist,omitempty"` // item -> unix time done (-1 = skipped for an existing user)
	ChecklistHidden bool               `json:"checklist_hidden,omitempty"`
	CURates         map[string]float64 `json:"cu_rates,omitempty"` // real CU/hour seen per GPU
}

var (
	cfgMu   sync.RWMutex
	cfg     Config
	dataDir string
)

func configBase() string {
	if runtime.GOOS == "linux" {
		if base := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(base) {
			return base
		}
		h, _ := os.UserHomeDir()
		return filepath.Join(h, ".config")
	}
	base := os.Getenv("APPDATA")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".config")
	}
	return base
}

func initDataDir() {
	base := configBase()
	dataDir = filepath.Join(base, "Wakeloom")
	// renamed from "ComfyColabWorkbench" in 1.17: carry the old settings, templates and queue over once
	// (the single-instance lock guarantees the previous version has exited by now)
	if _, err := os.Stat(dataDir); os.IsNotExist(err) {
		old := filepath.Join(base, "ComfyColabWorkbench")
		if st, err := os.Stat(old); err == nil && st.IsDir() {
			if os.Rename(old, dataDir) != nil {
				dataDir = old // could not move it (e.g. a file is open): keep using the old folder
			}
		}
	}
	for _, d := range []string{"", "templates", "uploads"} {
		os.MkdirAll(filepath.Join(dataDir, d), 0o700)
	}
}

func defaultOutDir() string {
	h, _ := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		return filepath.Join(h, "Desktop", "生成视频")
	}
	return filepath.Join(h, "WakeloomOutputs")
}

func loadConfig() {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfg = Config{JobTimeoutMin: 30, DraftScale: 0.5}
	if b, err := os.ReadFile(filepath.Join(dataDir, "config.json")); err == nil {
		json.Unmarshal(b, &cfg)
	}
	if cfg.OutDir == "" {
		cfg.OutDir = defaultOutDir()
	}
	if cfg.DraftScale <= 0 || cfg.DraftScale >= 1 {
		cfg.DraftScale = 0.5
	}
	if cfg.JobTimeoutMin <= 0 {
		cfg.JobTimeoutMin = 30
	}
	if cfg.UserdataSync == nil {
		on := true
		cfg.UserdataSync = &on
	}
}

func saveConfig(c Config) error {
	cfgMu.Lock()
	cfg = c
	cfgMu.Unlock()
	rememberDiagnosticConfig(c)
	return writeJSON(filepath.Join(dataDir, "config.json"), c)
}

func getConfig() Config {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
