package main

// "AI" page: chat with the LLM served by Ollama on the VM (reached through ComfyUI's /wbllm route)
// and a one-click text-to-image template for checkpoint profiles. Both work in gateway and local mode.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
)

// aiSummary normalises a local profile's `ai:` block the same way the kit's profiles.py does.
func (p *LocalProfile) aiSummary() map[string]any {
	if p.AI == nil {
		return nil
	}
	kind, _ := p.AI["type"].(string)
	if kind != "llm" && kind != "image" {
		return nil
	}
	dest := map[string]string{}
	for _, m := range p.Models {
		dest[m.ID] = m.Dest
	}
	sub, _ := p.AI[kind].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
	}
	mid := fmt.Sprint(sub["model"])
	if dest[mid] == "" {
		return nil
	}
	uses := []string{mid}
	num := func(k string, d float64) float64 {
		switch v := sub[k].(type) {
		case int:
			return float64(v)
		case float64:
			return v
		}
		return d
	}
	str := func(k, d string) string {
		if v, ok := sub[k].(string); ok && v != "" {
			return v
		}
		return d
	}
	out := map[string]any{"type": kind, "title": fmt.Sprint(nonNilStr(p.AI["title"]))}
	if kind == "llm" {
		think := true
		if v, ok := sub["think"].(bool); ok {
			think = v
		}
		out["llm"] = map[string]any{"model": mid, "dest": dest[mid], "name": str("name", "wb-llm"), "ctx": int(num("ctx", 32768)),
			"think": think, "temperature": num("temperature", 0.7), "system": str("system", "")}
	} else {
		arch := str("arch", "sdxl")
		im := map[string]any{"arch": arch, "model": mid, "negative": str("negative", ""), "shift": num("shift", 1)}
		if arch == "chroma" {
			clip, vae := fmt.Sprint(sub["clip"]), fmt.Sprint(sub["vae"])
			if dest[clip] == "" || dest[vae] == "" {
				return nil
			}
			im["clip"], im["vae"] = clip, vae
			im["unet_name"], im["clip_name"], im["vae_name"] = path.Base(dest[mid]), path.Base(dest[clip]), path.Base(dest[vae])
			uses = append(uses, clip, vae)
			im["width"], im["height"], im["steps"], im["cfg"] = int(num("width", 1024)), int(num("height", 1024)), int(num("steps", 26)), num("cfg", 3.8)
			im["sampler"], im["scheduler"] = str("sampler", "euler"), str("scheduler", "beta")
		} else {
			im["checkpoint"] = path.Base(dest[mid])
			im["width"], im["height"], im["steps"], im["cfg"] = int(num("width", 1024)), int(num("height", 1024)), int(num("steps", 30)), num("cfg", 5)
			im["sampler"], im["scheduler"] = str("sampler", "dpmpp_sde"), str("scheduler", "karras")
		}
		out["image"] = im
	}
	models := []string{}
	if l, ok := p.AI["models"].([]any); ok {
		for _, m := range l {
			models = append(models, fmt.Sprint(m))
		}
	}
	for _, u := range uses {
		found := false
		for _, m := range models {
			found = found || m == u
		}
		if !found {
			models = append(models, u)
		}
	}
	out["models"] = models
	return out
}

func nonNilStr(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// aiRequest sends a request to the VM's /wbllm/... route (via the gateway or the local gate).
func aiRequest(r *http.Request, method, p string, body []byte) (*http.Response, error) {
	b := baseURL()
	if b == "" {
		return nil, errors.New("还没配置网关 / 本地模式")
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, b+"/wbllm/"+p, rd)
	if err != nil {
		return nil, err
	}
	addAuth(req)
	req.Header.Set("Content-Type", "application/json")
	return (&http.Client{Transport: transport}).Do(req)
}

func aiInfoHandler(w http.ResponseWriter, r *http.Request) {
	resp, err := aiRequest(r, "GET", "info", nil)
	if err != nil {
		jsonOut(w, 200, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if resp.StatusCode >= 400 || json.Unmarshal(raw, &out) != nil {
		msg := fmt.Sprintf("GPU 上没有 AI 服务（HTTP %d）。确认是用 AI 启动配置唤醒的，且 GPU 已就绪", resp.StatusCode)
		if resp.StatusCode == 404 {
			msg = "GPU 上的环境还不支持 AI 页（网关需要先打 gate-ai.py 补丁，并用 AI 启动配置重新唤醒）"
		}
		jsonOut(w, 200, map[string]any{"error": msg, "status": resp.StatusCode})
		return
	}
	jsonOut(w, 200, out)
}

// aiChatHandler streams Ollama's NDJSON chat response straight through to the page.
func aiChatHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model       string           `json:"model"`
		Messages    []map[string]any `json:"messages"`
		Think       *bool            `json:"think"`
		Temperature *float64         `json:"temperature"`
		NumCtx      int              `json:"num_ctx"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&in); err != nil || in.Model == "" || len(in.Messages) == 0 {
		errOut(w, 400, errString("请求格式不对"))
		return
	}
	body := map[string]any{"model": in.Model, "messages": in.Messages, "stream": true, "keep_alive": -1}
	if in.Think != nil {
		body["think"] = *in.Think
	}
	opts := map[string]any{}
	if in.Temperature != nil {
		opts["temperature"] = *in.Temperature
	}
	if in.NumCtx > 0 {
		opts["num_ctx"] = in.NumCtx
	}
	if len(opts) > 0 {
		body["options"] = opts
	}
	b, _ := json.Marshal(body)
	resp, err := aiRequest(r, "POST", "api/chat", b)
	if err != nil {
		errOut(w, 502, fmt.Errorf("连不上 GPU 上的大模型：%w", err))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		msg := e.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 200 || strings.Contains(msg, "<html") {
				msg = ""
			}
		}
		switch {
		case resp.StatusCode == 404 && msg == "not allowed":
			msg = "接口被拒绝"
		case resp.StatusCode == 404 && msg == "":
			msg = "GPU 上没有 AI 服务（网关要先打 gate-ai.py 补丁，并用 AI 启动配置唤醒）"
		case resp.StatusCode == 503:
			msg = "大模型服务还没启动好，等启动日志出现「llm」完成后再试"
		}
		errOut(w, resp.StatusCode, fmt.Errorf("大模型返回 %d：%s", resp.StatusCode, msg))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

var aiTplID = regexp.MustCompile(`[^a-z0-9-]+`)

// aiImageTemplate creates (or refreshes) the built-in text-to-image template for a checkpoint profile.
func aiImageTemplateHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Profile    string  `json:"profile"`
		Title      string  `json:"title"`
		Arch       string  `json:"arch"`
		Checkpoint string  `json:"checkpoint"`
		UnetName   string  `json:"unet_name"`
		ClipName   string  `json:"clip_name"`
		VaeName    string  `json:"vae_name"`
		Shift      float64 `json:"shift"`
		Width      int     `json:"width"`
		Height     int     `json:"height"`
		Steps      int     `json:"steps"`
		CFG        float64 `json:"cfg"`
		Sampler    string  `json:"sampler"`
		Scheduler  string  `json:"scheduler"`
		Negative   string  `json:"negative"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil || in.Profile == "" ||
		(in.Arch == "chroma" && (in.UnetName == "" || in.ClipName == "" || in.VaeName == "")) || (in.Arch != "chroma" && in.Checkpoint == "") {
		errOut(w, 400, errString("请求格式不对"))
		return
	}
	id := "ai-" + strings.Trim(aiTplID.ReplaceAllString(strings.ToLower(in.Profile), "-"), "-")
	var g map[string]map[string]any
	var keys map[string]string
	if in.Arch == "chroma" {
		g, keys = chromaGraph(in.UnetName, in.ClipName, in.VaeName, in.Negative, in.Width, in.Height, in.Steps, in.CFG, in.Shift, in.Sampler)
	} else {
		g, keys = sdxlGraph(in.Checkpoint, in.Negative, in.Width, in.Height, in.Steps, in.CFG, in.Sampler, in.Scheduler), sdxlKeys
	}
	t := &Template{ID: id, Name: "AI 生图 · " + firstNonEmpty(in.Title, in.Profile), Created: time.Now().Unix(), Graph: g}
	if old, err := loadTemplate(id); err == nil {
		t.Created, t.Hidden = old.Created, old.Hidden
	}
	if err := saveTemplate(t); err != nil {
		errOut(w, 500, err)
		return
	}
	jsonOut(w, 200, map[string]any{"id": t.ID, "name": t.Name, "keys": keys, "fields": detectFields(t)})
}

// where the AI page's form values go in each graph
var sdxlKeys = map[string]string{"prompt": "2.text", "negative": "3.text", "width": "4.width", "height": "4.height",
	"steps": "5.steps", "cfg": "5.cfg", "seed": "5.seed"}

// chromaGraph follows the official Chroma1-HD ComfyUI template: UNET + T5 (type chroma, tokenizer padding
// options) + FLUX VAE, AuraFlow shift 1.0, CFGGuider, euler with the beta schedule (alpha = beta = 0.45).
func chromaGraph(unet, clip, vae, negative string, wd, ht, steps int, cfg, shift float64, sampler string) (map[string]map[string]any, map[string]string) {
	if wd <= 0 {
		wd = 1024
	}
	if ht <= 0 {
		ht = 1024
	}
	if steps <= 0 {
		steps = 26
	}
	if cfg <= 0 {
		cfg = 3.8
	}
	if shift <= 0 {
		shift = 1
	}
	node := func(cls, title string, in map[string]any) map[string]any {
		return map[string]any{"class_type": cls, "inputs": in, "_meta": map[string]any{"title": title}}
	}
	l := func(id string, i int) []any { return []any{id, float64(i)} }
	g := map[string]map[string]any{
		"1":  node("UNETLoader", "模型", map[string]any{"unet_name": unet, "weight_dtype": "default"}),
		"2":  node("CLIPTextEncode", "正向提示词", map[string]any{"text": "", "clip": l("11", 0)}),
		"3":  node("CLIPTextEncode", "反向提示词 negative", map[string]any{"text": negative, "clip": l("11", 0)}),
		"4":  node("EmptySD3LatentImage", "画布", map[string]any{"width": float64(wd), "height": float64(ht), "batch_size": float64(1)}),
		"5":  node("RandomNoise", "随机种子", map[string]any{"noise_seed": float64(0)}),
		"6":  node("VAEDecode", "解码", map[string]any{"samples": l("15", 0), "vae": l("12", 0)}),
		"7":  node("SaveImage", "保存图片", map[string]any{"filename_prefix": "ai", "images": l("6", 0)}),
		"8":  node("ModelSamplingAuraFlow", "Flow Shift", map[string]any{"shift": shift, "model": l("1", 0)}),
		"9":  node("CFGGuider", "引导", map[string]any{"cfg": cfg, "model": l("8", 0), "positive": l("2", 0), "negative": l("3", 0)}),
		"10": node("CLIPLoader", "文本编码器", map[string]any{"clip_name": clip, "type": "chroma", "device": "default"}),
		"11": node("T5TokenizerOptions", "T5 分词选项", map[string]any{"min_padding": float64(1), "min_length": float64(0), "clip": l("10", 0)}),
		"12": node("VAELoader", "VAE", map[string]any{"vae_name": vae}),
		"13": node("KSamplerSelect", "采样器", map[string]any{"sampler_name": firstNonEmpty(sampler, "euler")}),
		"14": node("BetaSamplingScheduler", "调度器", map[string]any{"steps": float64(steps), "alpha": 0.45, "beta": 0.45, "model": l("8", 0)}),
		"15": node("SamplerCustomAdvanced", "采样", map[string]any{"noise": l("5", 0), "guider": l("9", 0), "sampler": l("13", 0),
			"sigmas": l("14", 0), "latent_image": l("4", 0)}),
	}
	return g, map[string]string{"prompt": "2.text", "negative": "3.text", "width": "4.width", "height": "4.height",
		"steps": "14.steps", "cfg": "9.cfg", "seed": "5.noise_seed"}
}

// sdxlGraph is a plain checkpoint text-to-image graph in ComfyUI API format.
func sdxlGraph(ckpt, negative string, wd, ht, steps int, cfg float64, sampler, scheduler string) map[string]map[string]any {
	if wd <= 0 {
		wd = 1024
	}
	if ht <= 0 {
		ht = 1024
	}
	if steps <= 0 {
		steps = 30
	}
	if cfg <= 0 {
		cfg = 5
	}
	sampler = firstNonEmpty(sampler, "dpmpp_sde")
	scheduler = firstNonEmpty(scheduler, "karras")
	node := func(cls, title string, in map[string]any) map[string]any {
		return map[string]any{"class_type": cls, "inputs": in, "_meta": map[string]any{"title": title}}
	}
	l := func(id string, i int) []any { return []any{id, float64(i)} }
	return map[string]map[string]any{
		"1": node("CheckpointLoaderSimple", "模型", map[string]any{"ckpt_name": ckpt}),
		"2": node("CLIPTextEncode", "正向提示词", map[string]any{"text": "", "clip": l("1", 1)}),
		"3": node("CLIPTextEncode", "反向提示词 negative", map[string]any{"text": negative, "clip": l("1", 1)}),
		"4": node("EmptyLatentImage", "画布", map[string]any{"width": float64(wd), "height": float64(ht), "batch_size": float64(1)}),
		"5": node("KSampler", "采样", map[string]any{"seed": float64(0), "steps": float64(steps), "cfg": cfg,
			"sampler_name": sampler, "scheduler": scheduler, "denoise": 1.0,
			"model": l("1", 0), "positive": l("2", 0), "negative": l("3", 0), "latent_image": l("4", 0)}),
		"6": node("VAEDecode", "解码", map[string]any{"samples": l("5", 0), "vae": l("1", 2)}),
		"7": node("SaveImage", "保存图片", map[string]any{"filename_prefix": "ai", "images": l("6", 0)}),
	}
}

func aiRoutes(api *http.ServeMux) {
	api.HandleFunc("GET /api/ai/info", aiInfoHandler)
	api.HandleFunc("POST /api/ai/chat", aiChatHandler)
	api.HandleFunc("POST /api/ai/image-template", aiImageTemplateHandler)
}
