package main

// "Add model": paste a Hugging Face link, detect what it is (GGUF chat model, SDXL checkpoint,
// Chroma-style image model, or any ComfyUI model files), and save a start profile for it.
// Local mode stores the profile under <dataDir>/profiles/<id>/, gateway mode posts it to the
// gate's /_gate/custom-profiles (added by gateway/gate-ai.py). Both validate it with the same
// rules as the kit's gate/profiles.py (validateProfileYAML below; the tests cross-check with Python).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	customPrefix   = "custom-"
	customMaxBytes = 64 * 1024
	hfBase         = "https://huggingface.co"
)

// ---------------- link parsing ----------------

type hfRef struct {
	Repo string `json:"repo"` // owner/name
	Rev  string `json:"rev"`  // "" = main
	Path string `json:"path"` // file inside the repo, may be ""
}

var hfPart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// parseHFLink accepts https://huggingface.co/<owner>/<repo>[/tree|blob|resolve/<rev>[/<path>]],
// hf://owner/repo[@rev][/path] and a bare owner/repo.
func parseHFLink(s string) (hfRef, error) {
	s = strings.TrimSpace(s)
	bad := errors.New("看不懂这个链接。请粘贴 Hugging Face 模型页或文件链接，例如 https://huggingface.co/作者/仓库")
	if s == "" {
		return hfRef{}, errors.New("请先粘贴 Hugging Face 链接")
	}
	var parts []string
	rev := ""
	switch {
	case strings.HasPrefix(s, "hf://"):
		rest := strings.SplitN(strings.TrimPrefix(s, "hf://"), "?", 2)[0]
		parts = strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) >= 2 {
			if name, r, ok := strings.Cut(parts[1], "@"); ok {
				parts[1], rev = name, r
			}
		}
	case strings.Contains(s, "huggingface.co/") || strings.Contains(s, "hf.co/"):
		if !strings.Contains(s, "://") {
			s = "https://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			return hfRef{}, bad
		}
		h := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
		if h != "huggingface.co" && h != "hf.co" {
			return hfRef{}, bad
		}
		parts = strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) > 0 && (parts[0] == "models" || parts[0] == "api") { // /api/models/o/r
			parts = parts[1:]
			if len(parts) > 0 && parts[0] == "models" {
				parts = parts[1:]
			}
		}
		if len(parts) > 0 && (parts[0] == "datasets" || parts[0] == "spaces") {
			return hfRef{}, errors.New("这是数据集或 Space 的链接，不是模型仓库")
		}
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob" || parts[2] == "resolve") {
			rev = parts[3]
			parts = append(parts[:2], parts[4:]...)
		} else if len(parts) > 2 {
			return hfRef{}, bad
		}
	default:
		parts = strings.Split(strings.Trim(s, "/"), "/")
		if len(parts) != 2 {
			return hfRef{}, bad
		}
	}
	if len(parts) < 2 || !hfPart.MatchString(parts[0]) || !hfPart.MatchString(parts[1]) {
		return hfRef{}, bad
	}
	if rev == "main" {
		rev = ""
	}
	if rev != "" && !hfPart.MatchString(rev) {
		return hfRef{}, errors.New("链接里的版本（分支）名不支持：" + rev)
	}
	p := strings.Join(parts[2:], "/")
	if strings.Contains(p, "..") {
		return hfRef{}, bad
	}
	return hfRef{Repo: parts[0] + "/" + parts[1], Rev: rev, Path: p}, nil
}

// hfURL is the profile form: hf://owner/repo[@rev]/path
func (r hfRef) hfURL(p string) string {
	at := ""
	if r.Rev != "" {
		at = "@" + r.Rev
	}
	o, n, _ := strings.Cut(r.Repo, "/")
	return "hf://" + o + "/" + n + at + "/" + p
}

func (r hfRef) revOrMain() string {
	if r.Rev == "" {
		return "main"
	}
	return r.Rev
}

// ---------------- Hugging Face API ----------------

type hfModelInfo struct {
	ID          string         `json:"id"`
	Private     bool           `json:"private"`
	Gated       any            `json:"gated"` // false | "auto" | "manual"
	Tags        []string       `json:"tags"`
	PipelineTag string         `json:"pipeline_tag"`
	LibraryName string         `json:"library_name"`
	CardData    map[string]any `json:"cardData"`
	GGUF        *struct {
		Total         int64  `json:"total"`
		Architecture  string `json:"architecture"`
		ContextLength int64  `json:"context_length"`
		ChatTemplate  string `json:"chat_template"`
	} `json:"gguf"`
}

type hfTreeEntry struct {
	Type string `json:"type"`
	Path string `json:"path"`
	Size int64  `json:"size"`
	LFS  *struct {
		Size int64 `json:"size"`
	} `json:"lfs"`
}

var hfClient = &http.Client{Transport: transport, Timeout: 45 * time.Second}

// hfAPIBase can be pointed at a test server (CKWB_HF_BASE, like CKWB_PORT, is for UI tests).
var hfAPIBase = firstNonEmpty(strings.TrimRight(os.Getenv("CKWB_HF_BASE"), "/"), hfBase)

func hfProxyLabel() string {
	req, _ := http.NewRequest("GET", hfAPIBase+"/", nil)
	if req == nil {
		return ""
	}
	if u, _ := proxyFor(req); u != nil {
		return "经代理 " + u.Host
	}
	return "直连"
}

// hfGet fetches a Hugging Face URL; returns the body and the next-page link (tree listings are paginated).
func hfGet(ctx context.Context, u string, max int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Wakeloom/"+AppVersion)
	req.Header.Set("Accept", "application/json")
	if t := strings.TrimSpace(getConfig().HFToken); t != "" {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := hfClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("连不上 Hugging Face（%s）：%v。国内网络请在「设置 → 网络代理」填代理", hfProxyLabel(), err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, max))
	switch {
	case resp.StatusCode == 404:
		return nil, "", errors.New("找不到这个模型或文件（404）。请检查链接；私有仓库也会显示为找不到")
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, "", errors.New("没有权限读取（" + resp.Status + "）：这是私有或需要申请的仓库。请先在 Hugging Face 网站上申请访问，并在设置里填 HF 令牌")
	case resp.StatusCode == 429:
		return nil, "", errors.New("Hugging Face 请求太频繁（429），请稍后再试")
	case resp.StatusCode >= 400:
		return nil, "", fmt.Errorf("Hugging Face 返回错误 %s", resp.Status)
	}
	next := ""
	for _, l := range strings.Split(resp.Header.Get("Link"), ",") {
		if strings.Contains(l, `rel="next"`) {
			if i, j := strings.Index(l, "<"), strings.Index(l, ">"); i >= 0 && j > i {
				next = l[i+1 : j]
			}
		}
	}
	return b, next, nil
}

func hfModel(ctx context.Context, r hfRef) (*hfModelInfo, error) {
	u := hfAPIBase + "/api/models/" + r.Repo
	if r.Rev != "" {
		u += "/revision/" + url.PathEscape(r.Rev)
	}
	b, _, err := hfGet(ctx, u, 8<<20)
	if err != nil {
		return nil, err
	}
	var m hfModelInfo
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, errors.New("Hugging Face 返回的数据看不懂（不是 JSON）")
	}
	if m.Private {
		return nil, errors.New("这是私有仓库，Colab 上下载不了，请换一个公开的模型")
	}
	return &m, nil
}

func hfTree(ctx context.Context, r hfRef) ([]hfTreeEntry, error) {
	u := hfAPIBase + "/api/models/" + r.Repo + "/tree/" + url.PathEscape(r.revOrMain()) + "?recursive=1"
	var all []hfTreeEntry
	for page := 0; u != "" && page < 20; page++ {
		b, next, err := hfGet(ctx, u, 16<<20)
		if err != nil {
			return nil, err
		}
		var es []hfTreeEntry
		if err := json.Unmarshal(b, &es); err != nil {
			return nil, errors.New("Hugging Face 返回的文件列表看不懂")
		}
		all = append(all, es...)
		u = next
	}
	return all, nil
}

// ---------------- detection ----------------

type hfFile struct {
	Path   string  `json:"path"`
	Size   int64   `json:"size"`
	SizeGB float64 `json:"size_gb"`
	Quant  string  `json:"quant,omitempty"`
	Dir    string  `json:"dir"` // suggested ComfyUI models/ folder
}

type InspectResult struct {
	Repo        string         `json:"repo"`
	Rev         string         `json:"rev"`
	Link        string         `json:"link"`
	Kind        string         `json:"kind"` // llm | sdxl | chroma | comfy
	KindReason  string         `json:"kind_reason"`
	License     string         `json:"license"`
	Gated       bool           `json:"gated"`
	GatedNote   string         `json:"gated_note,omitempty"`
	Files       []hfFile       `json:"files"`     // weight files for the detected kind, best first
	AllFiles    []hfFile       `json:"all_files"` // every weight file (the UI re-filters when the kind is changed)
	Selected    []string       `json:"selected"`  // default choice
	Arch        string         `json:"arch,omitempty"`
	ContextLen  int64          `json:"context_length,omitempty"`
	GPU         string         `json:"gpu"`
	GPUNote     string         `json:"gpu_note,omitempty"`
	NeedGB      float64        `json:"need_gb"`
	Params      map[string]any `json:"params"`
	ID          string         `json:"id"`
	Title       string         `json:"title"`
	Extras      []specFile     `json:"extras"` // chroma: default T5 + VAE (reused from the built-in Chroma1-HD profile)
	ModelDirs   []string       `json:"model_dirs"`
	PipelineTag string         `json:"pipeline_tag,omitempty"`
	Notice      string         `json:"notice"`
}

var (
	quantRE   = regexp.MustCompile(`(?i)(?:^|[-_.])(I?Q[1-8](?:_[A-Z0-9]+)*|BF16|F16|F32|FP16|MXFP4)(?:[-_.]|$)`)
	shardRE   = regexp.MustCompile(`-\d{5}-of-\d{5}\.gguf$`)
	weightExt = []string{".safetensors", ".gguf", ".ckpt", ".pt", ".pth", ".bin", ".sft"}
	// GGUF architectures that are image/video diffusion models (ComfyUI-GGUF), not chat models
	imageGGUFArch = map[string]bool{"flux": true, "sd1": true, "sdxl": true, "sd3": true, "aura": true, "ltxv": true,
		"hyvid": true, "wan": true, "hidream": true, "chroma": true, "lumina2": true, "cosmos": true, "qwen_image": true}
)

func ggufQuant(p string) string {
	base := strings.TrimSuffix(path.Base(p), path.Ext(p))
	m := quantRE.FindAllStringSubmatch(base, -1)
	if len(m) == 0 {
		return ""
	}
	return strings.ToUpper(m[len(m)-1][1])
}

func gb(n int64) float64 { return math.Round(float64(n)/1e9*100) / 100 }

func hasTag(tags []string, want ...string) bool {
	for _, t := range tags {
		lt := strings.ToLower(t)
		for _, w := range want {
			if lt == w {
				return true
			}
		}
	}
	return false
}

func tagContains(tags []string, sub string) bool {
	for _, t := range tags {
		if strings.Contains(strings.ToLower(t), sub) {
			return true
		}
	}
	return false
}

// guessDir suggests a ComfyUI models/ subfolder for a file (type 4 and extras).
func guessDir(p string, tags []string) string {
	l := strings.ToLower(p)
	switch {
	case strings.HasSuffix(l, ".gguf") && !strings.Contains(l, "mmproj"):
		return "unet" // ComfyUI-GGUF loads diffusion GGUFs from unet/
	case strings.Contains(l, "lora") || hasTag(tags, "lora"):
		return "loras"
	case strings.Contains(l, "vae") || strings.HasPrefix(path.Base(l), "ae."):
		return "vae"
	case strings.Contains(l, "esrgan") || strings.Contains(l, "upscale") || regexp.MustCompile(`(^|[^a-z])x[248]([^0-9]|$)`).MatchString(path.Base(l)):
		return "upscale_models"
	case strings.Contains(l, "controlnet") || strings.Contains(l, "control_"):
		return "controlnet"
	case strings.Contains(l, "t5") || strings.Contains(l, "clip_") || strings.Contains(l, "text_encoder") || strings.Contains(l, "umt5"):
		return "text_encoders"
	case strings.Contains(l, "clip_vision") || strings.Contains(l, "siglip"):
		return "clip_vision"
	case strings.Contains(l, "ipadapter") || strings.Contains(l, "ip-adapter"):
		return "ipadapter"
	case strings.Contains(l, "transformer") || strings.Contains(l, "diffusion") || strings.Contains(l, "unet") ||
		strings.Contains(l, "flux") || strings.Contains(l, "wan2") || strings.Contains(l, "hunyuan") || strings.Contains(l, "ltx"):
		return "diffusion_models"
	}
	return "checkpoints"
}

// recommendGPU: need = size × 1.2 + 2 GB headroom; L4 = 24 GB, G4 = 96 GB, H100 = 80 GB (but faster / last resort).
func recommendGPU(totalGB float64) (gpu string, need float64, note string) {
	need = math.Round((totalGB*1.2+2)*10) / 10
	switch {
	case need <= 20:
		return "L4", need, ""
	case need <= 80:
		return "G4", need, ""
	}
	return "H100", need, "可能放不下，建议选更小的量化"
}

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// customID makes a profile id: custom- prefix, lowercase, a-z0-9-, at most 63 chars.
func customID(s string) string {
	s = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), customPrefix)
	s = strings.Trim(slugRE.ReplaceAllString(s, "-"), "-")
	if s == "" {
		s = "model"
	}
	id := customPrefix + s
	if len(id) > 63 {
		id = strings.TrimRight(id[:63], "-")
	}
	return id
}

func licenseOf(m *hfModelInfo) string {
	if m.CardData != nil {
		switch v := m.CardData["license"].(type) {
		case string:
			if v == "other" {
				if n, ok := m.CardData["license_name"].(string); ok && n != "" {
					return n
				}
			}
			return v
		case []any:
			if len(v) > 0 {
				return fmt.Sprint(v[0])
			}
		}
	}
	for _, t := range m.Tags {
		if strings.HasPrefix(t, "license:") {
			return strings.TrimPrefix(t, "license:")
		}
	}
	return ""
}

func isWeight(p string) bool {
	l := strings.ToLower(p)
	for _, e := range weightExt {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

// classify decides the kind from the repo metadata, the file list and (optional) model_index.json.
func classify(m *hfModelInfo, files []hfFile, modelIndex string) (kind, reason string) {
	nGGUF := 0
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Path), ".gguf") {
			nGGUF++
		}
	}
	arch := ""
	if m.GGUF != nil {
		arch = strings.ToLower(m.GGUF.Architecture)
	}
	lora := hasTag(m.Tags, "lora") || strings.EqualFold(m.LibraryName, "peft")
	switch {
	case nGGUF > 0 && !imageGGUFArch[arch] && m.PipelineTag != "text-to-image" && m.PipelineTag != "text-to-video":
		r := "仓库里有 GGUF 文件"
		if arch != "" {
			r += "（架构 " + arch + "）"
		}
		return "llm", r + "，按对话大模型处理（Ollama）"
	case lora:
		return "comfy", "这是 LoRA，按「任意 ComfyUI 模型」处理（放到 loras/）"
	case strings.Contains(modelIndex, "ChromaPipeline") || hasTag(m.Tags, "chroma") || strings.Contains(strings.ToLower(m.ID), "chroma") && tagContains(m.Tags, "flux.1-schnell"):
		return "chroma", "Chroma 结构（FLUX.1-schnell 衍生）"
	case strings.Contains(modelIndex, "StableDiffusionXL"):
		return "sdxl", "model_index.json 是 StableDiffusionXL"
	case hasTag(m.Tags, "stable-diffusion-xl", "sdxl") || tagContains(m.Tags, "base_model:stabilityai/stable-diffusion-xl-base"):
		return "sdxl", "标签含 stable-diffusion-xl"
	}
	if strings.Contains(modelIndex, "FluxPipeline") || tagContains(m.Tags, "flux") {
		return "comfy", "FLUX 原版结构和 Chroma 不同，按「任意 ComfyUI 模型」处理，需要配工作流"
	}
	return "comfy", "没认出固定类型，按「任意 ComfyUI 模型」处理：选好文件和存放目录，唤醒后在「生成」页用工作流"
}

// candidates returns the weight files relevant for a kind, best first, plus the default selection.
func candidates(kind string, files []hfFile, focus string) ([]hfFile, []string) {
	var out []hfFile
	for _, f := range files {
		l := strings.ToLower(f.Path)
		switch kind {
		case "llm":
			if !strings.HasSuffix(l, ".gguf") || strings.Contains(l, "mmproj") || shardRE.MatchString(l) {
				continue
			}
		case "sdxl", "chroma":
			// single-file checkpoints at the top of the repo (diffusers sub-folders are not loadable as one file)
			if strings.Contains(f.Path, "/") || !(strings.HasSuffix(l, ".safetensors") || strings.HasSuffix(l, ".ckpt") || strings.HasSuffix(l, ".sft")) || f.Size < 1e9 {
				continue
			}
		}
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	if len(out) == 0 {
		return out, nil
	}
	pick := ""
	for _, f := range out {
		if focus != "" && f.Path == focus {
			pick = f.Path
		}
	}
	if pick == "" {
		switch kind {
		case "llm":
			var q8, q4km, q4 string
			for _, f := range out {
				switch {
				case f.Quant == "Q8_0":
					if need := f.SizeGB*1.2 + 2; need <= 80 { // "显存够": fits a single big GPU
						q8 = f.Path
					}
				case f.Quant == "Q4_K_M":
					q4km = f.Path
				case strings.HasPrefix(f.Quant, "Q4") && q4 == "":
					q4 = f.Path
				}
			}
			pick = firstNonEmpty(q4km, firstNonEmpty(q4, out[0].Path))
			// Q8_0 when it still fits the GPU the Q4 file would get anyway (no extra cost, better quality)
			if q8 != "" {
				g8, _, _ := recommendGPU(sizeOfPath(out, q8))
				g4, _, _ := recommendGPU(sizeOfPath(out, pick))
				if g8 == g4 {
					pick = q8
				}
			}
		case "sdxl", "chroma":
			for _, f := range out {
				if strings.Contains(strings.ToLower(f.Path), "fp16") {
					pick = f.Path
					break
				}
			}
			if pick == "" && kind == "sdxl" {
				pick = out[0].Path // smallest full checkpoint (fp16-sized)
			}
			if pick == "" {
				for _, f := range out { // chroma: the full-precision main file, not an fp8 variant
					if !strings.Contains(strings.ToLower(f.Path), "fp8") {
						pick = f.Path
					}
				}
				if pick == "" {
					pick = out[len(out)-1].Path
				}
			}
		default:
			if focus == "" && len(out) == 1 {
				pick = out[0].Path
			}
		}
	}
	// best first: the pick, then the rest by size
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path == pick && out[j].Path != pick })
	if pick == "" {
		return out, nil
	}
	return out, []string{pick}
}

func sizeOfPath(fs []hfFile, p string) float64 {
	for _, f := range fs {
		if f.Path == p {
			return f.SizeGB
		}
	}
	return 0
}

// inspectHF is POST /api/custom-models/inspect.
func inspectHF(ctx context.Context, link string) (*InspectResult, error) {
	ref, err := parseHFLink(link)
	if err != nil {
		return nil, err
	}
	m, err := hfModel(ctx, ref)
	if err != nil {
		return nil, err
	}
	tree, err := hfTree(ctx, ref)
	if err != nil {
		return nil, err
	}
	modelIndex := ""
	var files []hfFile
	for _, e := range tree {
		if e.Type != "file" {
			continue
		}
		if e.Path == "model_index.json" {
			if b, _, err := hfGet(ctx, hfAPIBase+"/"+ref.Repo+"/resolve/"+url.PathEscape(ref.revOrMain())+"/model_index.json", 1<<20); err == nil {
				modelIndex = string(b)
			}
		}
		if !isWeight(e.Path) {
			continue
		}
		size := e.Size
		if e.LFS != nil && e.LFS.Size > 0 {
			size = e.LFS.Size
		}
		f := hfFile{Path: e.Path, Size: size, SizeGB: gb(size), Dir: guessDir(e.Path, m.Tags)}
		if strings.HasSuffix(strings.ToLower(e.Path), ".gguf") {
			f.Quant = ggufQuant(e.Path)
		}
		files = append(files, f)
	}
	if len(files) == 0 {
		return nil, errors.New("这个仓库里没有模型文件（.safetensors / .gguf / .ckpt 等）")
	}
	return buildInspect(ref, m, files, modelIndex)
}

// buildInspect is the offline part of inspectHF (unit-tested with recorded API JSON).
func buildInspect(ref hfRef, m *hfModelInfo, files []hfFile, modelIndex string) (*InspectResult, error) {
	kind, reason := classify(m, files, modelIndex)
	if ref.Path != "" && !isWeight(ref.Path) {
		ref.Path = ""
	}
	cands, sel := candidates(kind, files, ref.Path)
	if len(cands) == 0 && kind != "comfy" {
		if kind == "llm" {
			return nil, errors.New("只找到分片的 GGUF（-00001-of-0000N），Ollama 暂不支持，请换一个单文件的量化版本")
		}
		kind, reason = "comfy", "没找到单文件检查点，按「任意 ComfyUI 模型」处理"
		cands, sel = candidates(kind, files, ref.Path)
	}
	name := path.Base(ref.Repo)
	res := &InspectResult{Repo: ref.Repo, Rev: ref.Rev, Link: hfBase + "/" + ref.Repo, Kind: kind, KindReason: reason,
		License: licenseOf(m), Files: cands, AllFiles: files, Selected: nonNil(sel), PipelineTag: m.PipelineTag,
		Title: name, ID: customID(name), ModelDirs: modelDirList(), Extras: []specFile{},
		Notice: "模型来自第三方，请遵守其许可证和当地法律"}
	if g, ok := m.Gated.(string); ok && g != "" {
		res.Gated = true
		res.GatedNote = "这是需要申请访问的（gated）仓库：先在 Hugging Face 网站同意条款。网关模式由网关的 hf_token 下载；本地模式请在「设置 → 本地模式」填 HF 令牌。"
	} else if b, ok := m.Gated.(bool); ok && b {
		res.Gated = true
	}
	total := 0.0
	for _, p := range sel {
		total += sizeOfPath(cands, p)
	}
	switch kind {
	case "llm":
		ctxLen := int64(32768)
		if m.GGUF != nil {
			res.Arch = m.GGUF.Architecture
			res.ContextLen = m.GGUF.ContextLength
			if m.GGUF.ContextLength > 0 && m.GGUF.ContextLength < ctxLen {
				ctxLen = m.GGUF.ContextLength
			}
		}
		if ctxLen < 2048 {
			ctxLen = 2048
		}
		think := m.GGUF != nil && strings.Contains(m.GGUF.ChatTemplate, "<think>")
		res.Params = map[string]any{"ctx": ctxLen, "think": think, "temperature": 0.7}
		name = strings.TrimSuffix(strings.TrimSuffix(name, "-GGUF"), "-gguf")
		res.Title, res.ID = name, customID(name)
	case "sdxl":
		res.Params = map[string]any{"width": 1024, "height": 1024, "steps": 30, "cfg": 5.0, "sampler": "dpmpp_sde", "scheduler": "karras", "negative": ""}
	case "chroma":
		res.Params = map[string]any{"width": 1024, "height": 1024, "steps": 26, "cfg": 3.8, "sampler": "euler", "scheduler": "beta", "shift": 1.0, "negative": ""}
		if bp := builtinProfileYAML("chroma1-hd"); bp != nil {
			if bp.AI != nil && bp.AI.Image != nil {
				res.Params["negative"] = bp.AI.Image.Negative
			}
			for _, bm := range bp.Models {
				role := map[string]string{"text_encoders": "clip", "vae": "vae"}[strings.SplitN(bm.Dest, "/", 2)[0]]
				if role != "" {
					res.Extras = append(res.Extras, specFile{Role: role, URL: bm.URL, Dir: strings.SplitN(bm.Dest, "/", 2)[0],
						SizeGB: bm.SizeGB, License: bm.License, Name: path.Base(bm.Dest)})
					total += bm.SizeGB
				}
			}
		}
	default:
		res.Params = map[string]any{}
	}
	res.GPU, res.NeedGB, res.GPUNote = recommendGPU(total)
	return res, nil
}

// ---------------- profile YAML ----------------

// The YAML shape the kit understands (field order = output order).
type pyModel struct {
	ID      string  `yaml:"id"`
	Dest    string  `yaml:"dest"`
	URL     string  `yaml:"url"`
	License string  `yaml:"license"`
	SizeGB  float64 `yaml:"size_gb"`
}
type pyLLM struct {
	Model       string  `yaml:"model" json:"model"`
	Name        string  `yaml:"name" json:"name"`
	Ctx         int     `yaml:"ctx" json:"ctx"`
	Think       bool    `yaml:"think" json:"think"`
	Temperature float64 `yaml:"temperature" json:"temperature"`
	System      string  `yaml:"system,omitempty" json:"system,omitempty"`
}
type pyImage struct {
	Arch      string  `yaml:"arch" json:"arch"`
	Model     string  `yaml:"model" json:"model"`
	Clip      string  `yaml:"clip,omitempty" json:"clip,omitempty"`
	VAE       string  `yaml:"vae,omitempty" json:"vae,omitempty"`
	Width     int     `yaml:"width" json:"width"`
	Height    int     `yaml:"height" json:"height"`
	Steps     int     `yaml:"steps" json:"steps"`
	CFG       float64 `yaml:"cfg" json:"cfg"`
	Sampler   string  `yaml:"sampler" json:"sampler"`
	Scheduler string  `yaml:"scheduler" json:"scheduler"`
	Shift     float64 `yaml:"shift,omitempty" json:"shift,omitempty"`
	Negative  string  `yaml:"negative,omitempty" json:"negative,omitempty"`
}
type pyAI struct {
	Type  string   `yaml:"type"`
	Title string   `yaml:"title"`
	LLM   *pyLLM   `yaml:"llm,omitempty"`
	Image *pyImage `yaml:"image,omitempty"`
}
type pyComfy struct {
	Repo   string   `yaml:"repo"`
	Commit string   `yaml:"commit"`
	Args   []string `yaml:"args,flow,omitempty"`
}
type pyProfile struct {
	ID          string  `yaml:"id"`
	Title       string  `yaml:"title"`
	Accelerator string  `yaml:"accelerator"`
	ComfyUI     pyComfy `yaml:"comfyui"`
	Frontend    *struct {
		Package string `yaml:"package"`
	} `yaml:"frontend,omitempty"`
	Models []pyModel `yaml:"models"`
	AI     *pyAI     `yaml:"ai,omitempty"`
}

// builtinProfileYAML reads an embedded kit profile (the ComfyUI commit / frontend and Chroma's T5 + VAE come from there).
func builtinProfileYAML(id string) *pyProfile {
	b, err := fs.ReadFile(kitFS, "kit/profiles/"+id+"/profile.yaml")
	if err != nil {
		return nil
	}
	var p pyProfile
	if yaml.Unmarshal(b, &p) != nil {
		return nil
	}
	return &p
}

// specFile is one model file of a custom profile.
type specFile struct {
	Role    string  `json:"role"` // model | clip | vae | extra
	URL     string  `json:"url"`  // hf://… or https://…
	Dir     string  `json:"dir"`
	Name    string  `json:"name,omitempty"` // file name under dir (default: the URL's)
	SizeGB  float64 `json:"size_gb"`
	License string  `json:"license"`
}

// CustomSpec is what the UI sends to save (and gets back to edit).
type CustomSpec struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Kind     string     `json:"kind"`
	GPU      string     `json:"gpu"`
	Source   string     `json:"source,omitempty"` // https://huggingface.co/owner/repo (comment only)
	Files    []specFile `json:"files"`
	LLM      *pyLLM     `json:"llm,omitempty"`
	Image    *pyImage   `json:"image,omitempty"`
	Template string     `json:"template,omitempty"` // linked "生成" template (type 4)
}

var gpuChoices = map[string]bool{"T4": true, "L4": true, "G4": true, "A100": true, "H100": true}

func fileName(u string) string {
	u = strings.SplitN(strings.SplitN(u, "?", 2)[0], "#", 2)[0]
	return path.Base(u)
}

var fileNameRE = regexp.MustCompile(`^[A-Za-z0-9_.+ ()\[\]-]+$`)

// normalizeURL turns any accepted Hugging Face link into hf:// form; other https URLs pass through.
func normalizeURL(u string) (string, error) {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "hf://") || strings.Contains(u, "huggingface.co/") || strings.Contains(u, "hf.co/") {
		r, err := parseHFLink(u)
		if err != nil {
			return "", err
		}
		if r.Path == "" {
			return "", errors.New("链接要指向具体文件（…/blob/main/文件名）：" + u)
		}
		return r.hfURL(r.Path), nil
	}
	if strings.HasPrefix(u, "https://") && !strings.ContainsAny(u, "?# ") {
		return u, nil
	}
	return "", errors.New("文件链接要用 https:// 或 hf://，且不能带 ? 参数：" + u)
}

// buildProfile turns a spec into profile.yaml text.
func buildProfile(s CustomSpec) ([]byte, string, error) {
	id := customID(firstNonEmpty(s.ID, s.Title))
	title := strings.TrimSpace(s.Title)
	if title == "" {
		title = id
	}
	if len([]rune(title)) > 80 {
		title = string([]rune(title)[:80])
	}
	gpu := strings.ToUpper(strings.TrimSpace(s.GPU))
	if !gpuChoices[gpu] {
		gpu = "L4"
	}
	base := builtinProfileYAML("realvisxl-v5")
	if base == nil {
		return nil, "", errors.New("内置配置读取失败")
	}
	p := pyProfile{ID: id, Title: title, Accelerator: gpu, ComfyUI: base.ComfyUI, Frontend: base.Frontend}
	ids := map[string]string{} // role -> model id
	nExtra := 0
	for _, f := range s.Files {
		u, err := normalizeURL(f.URL)
		if err != nil {
			return nil, "", err
		}
		name := strings.TrimSpace(firstNonEmpty(f.Name, fileName(u)))
		if !fileNameRE.MatchString(name) || strings.Contains(name, "..") {
			return nil, "", errors.New("文件名不支持：" + name)
		}
		mid := map[string]string{"model": "main", "clip": "text-encoder", "vae": "vae"}[f.Role]
		if mid == "" || ids[f.Role] != "" {
			nExtra++
			mid = fmt.Sprintf("file-%d", nExtra)
		}
		if f.Role != "extra" && ids[f.Role] == "" {
			ids[f.Role] = mid
		}
		p.Models = append(p.Models, pyModel{ID: mid, Dest: strings.Trim(f.Dir, "/") + "/" + name, URL: u,
			License: strings.TrimSpace(f.License), SizeGB: math.Round(f.SizeGB*100) / 100})
	}
	if len(p.Models) == 0 {
		return nil, "", errors.New("至少要有一个模型文件")
	}
	switch s.Kind {
	case "llm":
		if ids["model"] == "" {
			return nil, "", errors.New("没有选 GGUF 文件")
		}
		l := pyLLM{Ctx: 32768, Temperature: 0.7}
		if s.LLM != nil {
			l = *s.LLM
		}
		l.Model, l.Name = ids["model"], id
		l.Ctx = max(2048, min(262144, l.Ctx))
		p.AI = &pyAI{Type: "llm", Title: title, LLM: &l}
	case "sdxl", "chroma":
		if ids["model"] == "" {
			return nil, "", errors.New("没有选主模型文件")
		}
		im := pyImage{}
		if s.Image != nil {
			im = *s.Image
		}
		im.Arch, im.Model = s.Kind, ids["model"]
		if s.Kind == "chroma" {
			if ids["clip"] == "" || ids["vae"] == "" {
				return nil, "", errors.New("Chroma 需要 T5 文本编码器和 VAE 两个文件")
			}
			im.Clip, im.VAE = ids["clip"], ids["vae"]
		} else {
			im.Clip, im.VAE, im.Shift = "", "", 0
		}
		p.AI = &pyAI{Type: "image", Title: title, Image: &im}
	case "comfy":
	default:
		return nil, "", errors.New("未知类型：" + s.Kind)
	}
	out, err := yaml.Marshal(&p)
	if err != nil {
		return nil, "", err
	}
	head := "# Added in Wakeloom (自定义模型)"
	if src := strings.TrimSpace(s.Source); strings.HasPrefix(src, "https://huggingface.co/") && !strings.ContainsAny(src, "?#\n") {
		head += " from " + src
	}
	head += "\n# Third-party model: follow its license and local law.\n"
	text := append([]byte(head), out...)
	if _, err := validateProfileYAML(text); err != nil {
		return nil, "", err
	}
	return text, id, nil
}

// specFromYAML rebuilds the edit form from a saved profile.
func specFromYAML(text []byte) (CustomSpec, error) {
	var p pyProfile
	if err := yaml.Unmarshal(text, &p); err != nil {
		return CustomSpec{}, err
	}
	s := CustomSpec{ID: p.ID, Title: p.Title, GPU: p.Accelerator, Kind: "comfy"}
	if i := strings.Index(string(text), " from https://huggingface.co/"); i >= 0 {
		s.Source = strings.Fields(string(text)[i+6:])[0]
	}
	role := map[string]string{}
	if p.AI != nil {
		if p.AI.LLM != nil {
			s.Kind, s.LLM = "llm", p.AI.LLM
			role[p.AI.LLM.Model] = "model"
		} else if p.AI.Image != nil {
			s.Kind, s.Image = p.AI.Image.Arch, p.AI.Image
			if s.Kind == "" {
				s.Kind = "sdxl"
			}
			role[p.AI.Image.Model], role[p.AI.Image.Clip], role[p.AI.Image.VAE] = "model", "clip", "vae"
		}
	}
	for _, m := range p.Models {
		r := role[m.ID]
		if r == "" {
			r = "extra"
		}
		dir, name, _ := strings.Cut(m.Dest, "/")
		s.Files = append(s.Files, specFile{Role: r, URL: m.URL, Dir: dir, Name: name, SizeGB: m.SizeGB, License: m.License})
	}
	return s, nil
}

// ---------------- validation (mirror of kit/gate/profiles.py parse_profile) ----------------

// modelDirs mirrors MODEL_DIRS in kit/gate/profiles.py (a test keeps them identical).
var modelDirs = map[string]bool{"checkpoints": true, "diffusion_models": true, "text_encoders": true, "clip": true,
	"vae": true, "loras": true, "upscale_models": true, "controlnet": true, "embeddings": true, "clip_vision": true,
	"unet": true, "rapid_moss": true, "model_patches": true, "llm": true, "style_models": true, "gligen": true,
	"hypernetworks": true, "photomaker": true, "vae_approx": true, "audio_encoders": true, "latent_upscale_models": true,
	"ipadapter": true, "sams": true, "ultralytics": true, "insightface": true, "instantid": true}

func modelDirList() []string {
	out := make([]string, 0, len(modelDirs))
	for d := range modelDirs {
		if d != "llm" && d != "rapid_moss" {
			out = append(out, d)
		}
	}
	sort.Strings(out)
	return out
}

var (
	forbiddenKey  = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[_-]?key|authorization|cookie|credential)`)
	secretValue   = regexp.MustCompile(`(hf_[A-Za-z0-9]{20,}|ya29\.|ghp_[A-Za-z0-9]{20,}|eyJ[A-Za-z0-9_-]{20,}\.)`)
	profileIDRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	profileURLRE  = regexp.MustCompile(`^(https://[^\s?#]+|hf://[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+(@[A-Za-z0-9_.-]+)?/[^\s?#]+)$`)
	ollamaNameRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9._:-]{0,62}$`)
	sha256RE      = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	envNameRE     = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	reservedArgs  = map[string]bool{"--listen": true, "--port": true, "--output-directory": true, "--disable-auto-launch": true, "--extra-model-paths-config": true}
	reservedEnvVs = map[string]bool{"CF_TUNNEL_TOKEN": true, "HF_TOKEN": true, "KIT_GATE_URL": true, "KIT_PROFILE_JSON": true}
)

func scanSecrets(v any, where string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, x := range t {
			if forbiddenKey.MatchString(k) {
				return fmt.Errorf("不能包含密钥类字段 '%s%s'（配置是公开的）", where, k)
			}
			if err := scanSecrets(x, where+k+"."); err != nil {
				return err
			}
		}
	case []any:
		for i, x := range t {
			if err := scanSecrets(x, fmt.Sprintf("%s%d.", where, i)); err != nil {
				return err
			}
		}
	case string:
		if secretValue.MatchString(t) {
			return fmt.Errorf("'%s' 的值看起来像令牌，不能写进配置", strings.TrimSuffix(where, "."))
		}
	}
	return nil
}

func reqStr(m map[string]any, k, ctx string) (string, error) {
	v, ok := m[k]
	if !ok || v == nil || v == "" {
		return "", fmt.Errorf("%s: 缺少 '%s'", ctx, k)
	}
	return fmt.Sprint(v), nil
}

func asInt(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		return int(t), true // Python int() truncates floats too
	case string:
		var n int
		_, err := fmt.Sscan(strings.TrimSpace(t), &n)
		return n, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

func asFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int:
		return float64(t), true
	case float64:
		return t, true
	case string:
		var f float64
		_, err := fmt.Sscan(strings.TrimSpace(t), &f)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// validateProfileYAML applies parse_profile's rules (the subset a custom profile can use) and returns the id.
func validateProfileYAML(text []byte) (string, error) {
	if len(text) > customMaxBytes {
		return "", fmt.Errorf("配置太大（超过 %d KB）", customMaxBytes/1024)
	}
	var data map[string]any
	if err := yaml.Unmarshal(text, &data); err != nil {
		return "", fmt.Errorf("YAML 格式错误：%v", err)
	}
	if data == nil {
		return "", errors.New("配置必须是一个映射（mapping）")
	}
	if err := scanSecrets(data, ""); err != nil {
		return "", err
	}
	pid, err := reqStr(data, "id", "profile")
	if err != nil {
		return "", err
	}
	if !profileIDRE.MatchString(pid) {
		return "", fmt.Errorf("配置 id 不合法：%q（只能用小写字母、数字和 -，最长 63）", pid)
	}
	models := map[string]string{} // id -> dest
	rawModels, _ := data["models"].([]any)
	if data["models"] != nil && rawModels == nil {
		return "", errors.New("models 必须是列表")
	}
	for i, x := range rawModels {
		ctx := fmt.Sprintf("models[%d]", i)
		m, ok := x.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%s 必须是映射", ctx)
		}
		mid, err := reqStr(m, "id", ctx)
		if err != nil {
			return "", err
		}
		dest, err := reqStr(m, "dest", ctx)
		if err != nil {
			return "", err
		}
		if _, dup := models[mid]; dup {
			return "", fmt.Errorf("%s: 模型 id 重复 %s", ctx, mid)
		}
		top := strings.SplitN(dest, "/", 2)[0]
		if !modelDirs[top] || strings.Contains(dest, "..") || strings.HasPrefix(dest, "/") {
			return "", fmt.Errorf("%s: 存放位置必须是 <models 子目录>/<文件名>，%q 不行", ctx, dest)
		}
		u, err := reqStr(m, "url", ctx)
		if err != nil {
			return "", err
		}
		if !profileURLRE.MatchString(u) {
			return "", fmt.Errorf("%s: 链接必须是 https://… 或 hf://作者/仓库/文件，且不能带 ? 参数：%q", ctx, u)
		}
		if s, ok := m["sha256"]; ok && s != nil && !sha256RE.MatchString(fmt.Sprint(s)) {
			return "", fmt.Errorf("%s: sha256 必须是 64 位十六进制", ctx)
		}
		models[mid] = dest
	}
	for i, x := range anyList(data["custom_nodes"]) {
		n, _ := x.(map[string]any)
		ctx := fmt.Sprintf("custom_nodes[%d]", i)
		if n == nil {
			return "", fmt.Errorf("%s 必须是映射", ctx)
		}
		if _, err := reqStr(n, "id", ctx); err != nil {
			return "", err
		}
		u, err := reqStr(n, "url", ctx)
		if err != nil {
			return "", err
		}
		if !profileURLRE.MatchString(u) {
			return "", fmt.Errorf("%s: 链接不合法 %q", ctx, u)
		}
	}
	if len(anyList(data["workflows"])) > 0 || data["smoke"] != nil {
		return "", errors.New("自定义模型配置不支持 workflows / smoke（工作流请在「生成」页导入）")
	}
	comfy, _ := data["comfyui"].(map[string]any)
	if comfy == nil {
		comfy = map[string]any{}
	}
	if _, err := reqStr(comfy, "commit", "comfyui"); err != nil {
		return "", err
	}
	if a, ok := comfy["args"]; ok && a != nil {
		args, ok := a.([]any)
		if !ok {
			return "", errors.New("comfyui.args 必须是字符串列表")
		}
		for i, x := range args {
			s, ok := x.(string)
			if !ok {
				return "", errors.New("comfyui.args 必须是字符串列表")
			}
			if i == 0 && !strings.HasPrefix(s, "--") {
				return "", errors.New("comfyui.args 必须以 --参数 开头")
			}
			if strings.HasPrefix(s, "--") && reservedArgs[strings.SplitN(s, "=", 2)[0]] {
				return "", fmt.Errorf("comfyui.args 不能改 %s（由启动套件管理）", s)
			}
		}
	}
	if e, ok := comfy["env"]; ok && e != nil {
		env, ok := e.(map[string]any)
		if !ok {
			return "", errors.New("comfyui.env 必须是映射")
		}
		for k := range env {
			if !envNameRE.MatchString(k) || reservedEnvVs[k] {
				return "", fmt.Errorf("comfyui.env 不能设置 %s", k)
			}
		}
	}
	if raw, ok := data["ai"]; ok && raw != nil {
		if err := validateAI(raw, models); err != nil {
			return "", err
		}
	}
	return pid, nil
}

func anyList(v any) []any { l, _ := v.([]any); return l }

func validateAI(raw any, models map[string]string) error {
	ai, ok := raw.(map[string]any)
	if !ok {
		return errors.New("ai 必须是映射")
	}
	kind := fmt.Sprint(ai["type"])
	sub := func(k string) map[string]any {
		m, _ := ai[k].(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	}
	uses := []string{}
	switch kind {
	case "llm":
		l := sub("llm")
		mid, err := reqStr(l, "model", "ai.llm")
		if err != nil {
			return err
		}
		if d, ok := models[mid]; !ok || !strings.HasPrefix(d, "llm/") || !strings.HasSuffix(d, ".gguf") {
			return errors.New("ai.llm.model 必须指向存到 llm/<文件>.gguf 的模型")
		}
		name := "wb-llm"
		if v, ok := l["name"]; ok && v != nil && v != "" {
			name = fmt.Sprint(v)
		}
		if !ollamaNameRE.MatchString(name) {
			return errors.New("ai.llm.name 只能用小写字母、数字和 . _ : -")
		}
		if v, ok := l["ctx"]; ok {
			if _, ok := asInt(v); !ok {
				return errors.New("ai.llm.ctx 必须是整数")
			}
		}
		if v, ok := l["temperature"]; ok {
			if _, ok := asFloat(v); !ok {
				return errors.New("ai.llm.temperature 必须是数字")
			}
		}
		uses = append(uses, mid)
	case "image":
		im := sub("image")
		arch := "sdxl"
		if v, ok := im["arch"]; ok && v != nil && v != "" {
			arch = fmt.Sprint(v)
		}
		need := func(k string, dirs ...string) error {
			mid, err := reqStr(im, k, "ai.image")
			if err != nil {
				return err
			}
			d, ok := models[mid]
			top := strings.SplitN(d, "/", 2)[0]
			for _, x := range dirs {
				if ok && top == x {
					uses = append(uses, mid)
					return nil
				}
			}
			return fmt.Errorf("ai.image.%s 必须指向 %s/ 下的模型", k, strings.Join(dirs, " 或 "))
		}
		switch arch {
		case "sdxl":
			if err := need("model", "checkpoints"); err != nil {
				return err
			}
		case "chroma":
			for _, e := range []error{need("model", "diffusion_models", "unet"), need("clip", "text_encoders", "clip"), need("vae", "vae")} {
				if e != nil {
					return e
				}
			}
		default:
			return errors.New("ai.image.arch 只能是 sdxl 或 chroma")
		}
		for _, k := range []string{"width", "height", "steps"} {
			if v, ok := im[k]; ok {
				if _, ok := asInt(v); !ok {
					return fmt.Errorf("ai.image.%s 类型不对", k)
				}
			}
		}
		for _, k := range []string{"cfg", "shift"} {
			if v, ok := im[k]; ok {
				if _, ok := asFloat(v); !ok {
					return fmt.Errorf("ai.image.%s 类型不对", k)
				}
			}
		}
	default:
		return errors.New("ai.type 只能是 llm 或 image")
	}
	for _, x := range anyList(ai["models"]) {
		if _, ok := models[fmt.Sprint(x)]; !ok {
			return fmt.Errorf("ai.models: 未知模型 %v", x)
		}
	}
	_ = uses
	return nil
}

// ---------------- local-mode store (the local gate's /_gate/custom-profiles) ----------------

func builtinIDs() map[string]bool {
	out := map[string]bool{}
	if ents, err := fs.ReadDir(kitFS, "kit/profiles"); err == nil {
		for _, e := range ents {
			if p := builtinProfileYAML(e.Name()); p != nil {
				out[p.ID] = true
			}
		}
	}
	return out
}

func localCustomList() []map[string]any {
	items := []map[string]any{}
	ents, _ := os.ReadDir(userProfilesDir())
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), customPrefix) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(userProfilesDir(), e.Name(), "profile.yaml"))
		if err != nil {
			continue
		}
		it := map[string]any{"id": e.Name(), "title": e.Name(), "yaml": string(b), "loaded": true, "error": ""}
		if _, err := validateProfileYAML(b); err != nil {
			it["loaded"], it["error"] = false, err.Error()
		} else if s, err := specFromYAML(b); err == nil {
			it["title"] = s.Title
		}
		items = append(items, it)
	}
	return items
}

func localCustomSave(text string) (string, error) {
	pid, err := validateProfileYAML([]byte(text))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(pid, customPrefix) {
		return "", fmt.Errorf("自定义配置的 id 必须以 %s 开头", customPrefix)
	}
	if builtinIDs()[pid] {
		return "", fmt.Errorf("%s 是内置配置，不能覆盖", pid)
	}
	d := filepath.Join(userProfilesDir(), pid)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(d, "profile.yaml.tmp")
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", err
	}
	return pid, os.Rename(tmp, filepath.Join(d, "profile.yaml"))
}

func (g *LocalGate) customProfiles(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/_gate/custom-profiles"), "/")
	switch {
	case r.Method == "GET" && id == "":
		jsonOut(w, 200, map[string]any{"items": localCustomList(), "max_bytes": customMaxBytes})
	case r.Method == "POST" && id == "":
		var in struct {
			YAML string `json:"yaml"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 2*customMaxBytes)).Decode(&in); err != nil {
			jsonOut(w, 400, map[string]string{"error": "bad_profile", "message": "请求格式错误"})
			return
		}
		pid, err := localCustomSave(in.YAML)
		if err != nil {
			jsonOut(w, 400, map[string]string{"error": "bad_profile", "message": err.Error()})
			return
		}
		jsonOut(w, 200, map[string]any{"id": pid})
	case r.Method == "DELETE" && id != "":
		if !strings.HasPrefix(id, customPrefix) || !profileIDRE.MatchString(id) {
			jsonOut(w, 404, map[string]string{"error": "unknown_profile", "message": "内置配置不能删除"})
			return
		}
		d := filepath.Join(userProfilesDir(), id)
		if _, err := os.Stat(filepath.Join(d, "profile.yaml")); err != nil {
			jsonOut(w, 404, map[string]string{"error": "unknown_profile", "message": "找不到这个自定义配置"})
			return
		}
		g.mu.Lock()
		inUse := g.profile == id && g.state != "sleeping" && g.state != "failed"
		g.mu.Unlock()
		if inUse {
			jsonOut(w, 409, map[string]string{"error": "in_use", "message": "这个配置正在使用，请先停止 GPU"})
			return
		}
		os.RemoveAll(d)
		jsonOut(w, 200, map[string]string{"deleted": id})
	default:
		jsonOut(w, 405, map[string]string{"error": "method_not_allowed"})
	}
}

// ---------------- app API ----------------

var errGateOutdated = errors.New("网关还不支持自定义模型：网关需要更新 gate-ai.py（在 VPS 上重新运行 python3 gate-ai.py /opt/comfy-colab-kit 并重建容器）")

// customGate calls /_gate/custom-profiles and turns "route missing" into errGateOutdated.
func customGate(method, p string, in, out any) error {
	err := gateJSON(method, "/_gate/custom-profiles"+p, in, out)
	if err == nil || localMode() {
		return err
	}
	var ge *GateError
	if errors.As(err, &ge) && (ge.Status == 404 && ge.Code != "unknown_profile" || ge.Code == "cf_access" || ge.Code == "bad_json" || ge.Status == 405) {
		var st any
		if gateJSON("GET", "/_gate/status", nil, &st) == nil { // the gate answers, just not this route
			return errGateOutdated
		}
	}
	return err
}

func templateFor(profileID string) string {
	for _, t := range listTemplates() {
		if t.Profile == profileID {
			return t.ID
		}
	}
	return ""
}

func linkTemplate(profileID, tplID string) {
	for _, t := range listTemplates() {
		want := tplID != "" && t.ID == tplID
		if want && t.Profile != profileID {
			t.Profile = profileID
			saveTemplate(t)
		} else if !want && t.Profile == profileID {
			t.Profile = ""
			saveTemplate(t)
		}
	}
}

func gateErrOut(w http.ResponseWriter, err error) {
	code := 502
	var ge *GateError
	if errors.As(err, &ge) && ge.Status >= 400 {
		code = ge.Status
	}
	out := map[string]any{"error": err.Error(), "status": code}
	if errors.Is(err, errGateOutdated) {
		out["code"], code = "gate_outdated", 409
	}
	jsonOut(w, code, out)
}

func registerCustomModelRoutes(api *http.ServeMux) {
	api.HandleFunc("POST /api/custom-models/inspect", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			URL string `json:"url"`
		}
		json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&in)
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		res, err := inspectHF(ctx, in.URL)
		if err != nil {
			errOut(w, 400, err)
			return
		}
		jsonOut(w, 200, res)
	})
	api.HandleFunc("GET /api/custom-models", func(w http.ResponseWriter, r *http.Request) {
		var out struct {
			Items []struct {
				ID     string `json:"id"`
				Title  string `json:"title"`
				YAML   string `json:"yaml"`
				Loaded bool   `json:"loaded"`
				Error  string `json:"error"`
			} `json:"items"`
		}
		if err := customGate("GET", "", nil, &out); err != nil {
			gateErrOut(w, err)
			return
		}
		items := []map[string]any{}
		for _, it := range out.Items {
			s, err := specFromYAML([]byte(it.YAML))
			e := it.Error
			if err != nil && e == "" {
				e = err.Error()
			}
			s.Template = templateFor(it.ID)
			items = append(items, map[string]any{"id": it.ID, "title": firstNonEmpty(s.Title, it.Title), "loaded": it.Loaded, "error": e, "spec": s})
		}
		jsonOut(w, 200, map[string]any{"items": items, "mode": map[bool]string{true: "local", false: "gateway"}[localMode()]})
	})
	api.HandleFunc("POST /api/custom-models", func(w http.ResponseWriter, r *http.Request) {
		var s CustomSpec
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&s); err != nil {
			errOut(w, 400, err)
			return
		}
		text, id, err := buildProfile(s)
		if err != nil {
			errOut(w, 400, err)
			return
		}
		if builtinIDs()[id] {
			errOut(w, 400, errors.New(id+" 是内置配置，不能覆盖"))
			return
		}
		var out map[string]any
		if err := customGate("POST", "", map[string]string{"yaml": string(text)}, &out); err != nil {
			gateErrOut(w, err)
			return
		}
		linkTemplate(id, s.Template)
		jsonOut(w, 200, map[string]any{"id": id, "yaml": string(text)})
	})
	api.HandleFunc("POST /api/custom-models/preview", func(w http.ResponseWriter, r *http.Request) {
		var s CustomSpec
		json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&s)
		text, id, err := buildProfile(s)
		if err != nil {
			errOut(w, 400, err)
			return
		}
		jsonOut(w, 200, map[string]any{"id": id, "yaml": string(text)})
	})
	api.HandleFunc("DELETE /api/custom-models/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !strings.HasPrefix(id, customPrefix) {
			errOut(w, 400, errors.New("内置配置不能删除"))
			return
		}
		var out any
		if err := customGate("DELETE", "/"+url.PathEscape(id), nil, &out); err != nil {
			gateErrOut(w, err)
			return
		}
		linkTemplate(id, "")
		jsonOut(w, 200, map[string]any{"deleted": id})
	})
}
