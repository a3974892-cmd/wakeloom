package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// builtinPromptGuide is Wakeloom's own short, generic advice for writing video prompts. The official MiniMax
// H3 prompt guide is not shipped (its community license limits where it may be redistributed): users who
// have it can import it in settings, and it is then used instead (see userGuidePath).
const builtinPromptGuide = `You write prompts for an AI model that turns a text description (and optional reference pictures) into a short video with sound.

Think like a director and make every sentence something a camera could film:
- Subject: who or what is on screen; looks, clothing, expression, position in the frame.
- Action: what happens, in order, at a pace that fits the requested length. Avoid too many events for a short clip.
- Camera: shot size and angle, how the camera moves (or stays still) and how fast; say clearly where one shot ends and the next begins.
- Light and look: time of day, light direction and quality, colours, overall visual style.
- Sound: background ambience, sound effects, spoken lines with the speaker, and whether there is music.

With reference pictures, say what each picture is for (opening frame, closing frame, or what a character or place should look like) and keep what is visible in them consistent.
Prefer concrete nouns and verbs over adjectives; do not describe things the viewer cannot see or hear.`

const guideMaxBytes = 1 << 20

func userGuidePath() string { return filepath.Join(dataDir, "prompts", "h3_guide.md") }

// promptGuide returns the imported guide if there is one, else the built-in advice.
func promptGuide() (text string, imported bool) {
	if b, err := os.ReadFile(userGuidePath()); err == nil && len(strings.TrimSpace(string(b))) > 0 {
		return string(b), true
	}
	return builtinPromptGuide, false
}

// LLMProvider is one configured model endpoint. Kind: "openai" (any OpenAI-compatible API),
// "anthropic" or "gemini".
type LLMProvider struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

const keyMask = "••••••••"

func defaultBase(kind string) string {
	switch kind {
	case "anthropic":
		return "https://api.anthropic.com"
	case "gemini":
		return "https://generativelanguage.googleapis.com"
	}
	return "https://api.openai.com/v1"
}

func (p LLMProvider) base() string {
	b := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if b == "" {
		b = defaultBase(p.Kind)
	}
	return b
}

func findProvider(id string) (LLMProvider, error) {
	c := getConfig()
	for _, p := range c.LLMs {
		if p.ID == id {
			return p, nil
		}
	}
	if id == "" && len(c.LLMs) > 0 {
		for _, p := range c.LLMs {
			if p.ID == c.DefaultLLM {
				return p, nil
			}
		}
		return c.LLMs[0], nil
	}
	return LLMProvider{}, errors.New("还没有配置大模型 API（到「设置 → 大模型 API」添加）")
}

// ---- messages ----

type llmImage struct {
	Mime string // image/png
	B64  string // raw base64
}

type llmRequest struct {
	System string
	User   string
	Images []llmImage
}

var llmClient = &http.Client{Transport: transport}

func llmPost(ctx context.Context, url string, headers map[string]string, body any) (*http.Response, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := llmClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		return nil, httpErr(resp.StatusCode, raw)
	}
	return resp, nil
}

func apiErrorText(raw []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != nil {
		switch v := e.Error.(type) {
		case string:
			return v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return m
			}
		}
	}
	return truncate(strings.TrimSpace(string(raw)), 300)
}

// streamLLM calls the provider and invokes onDelta for every text chunk.
func streamLLM(ctx context.Context, p LLMProvider, r llmRequest, maxTokens int, onDelta func(string)) error {
	if strings.TrimSpace(p.Model) == "" {
		return errors.New("这个 API 还没填模型名")
	}
	switch p.Kind {
	case "anthropic":
		content := []map[string]any{}
		for _, im := range r.Images {
			content = append(content, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": im.Mime, "data": im.B64}})
		}
		content = append(content, map[string]any{"type": "text", "text": r.User})
		body := map[string]any{"model": p.Model, "max_tokens": maxTokens, "stream": true, "system": r.System,
			"messages": []any{map[string]any{"role": "user", "content": content}}}
		resp, err := llmPost(ctx, p.base()+"/v1/messages", map[string]string{"x-api-key": p.APIKey, "anthropic-version": "2023-06-01"}, body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return readSSE(resp.Body, func(event, data string) error {
			var d struct {
				Type  string `json:"type"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &d) != nil {
				return nil
			}
			if d.Error != nil {
				return errors.New(d.Error.Message)
			}
			if d.Type == "content_block_delta" && d.Delta.Text != "" {
				onDelta(d.Delta.Text)
			}
			return nil
		})
	case "gemini":
		parts := []map[string]any{}
		for _, im := range r.Images {
			parts = append(parts, map[string]any{"inline_data": map[string]any{"mime_type": im.Mime, "data": im.B64}})
		}
		parts = append(parts, map[string]any{"text": r.User})
		body := map[string]any{
			"system_instruction": map[string]any{"parts": []any{map[string]any{"text": r.System}}},
			"contents":           []any{map[string]any{"role": "user", "parts": parts}},
			"generationConfig":   map[string]any{"maxOutputTokens": maxTokens},
		}
		model := strings.TrimPrefix(p.Model, "models/")
		resp, err := llmPost(ctx, p.base()+"/v1beta/models/"+model+":streamGenerateContent?alt=sse", map[string]string{"x-goog-api-key": p.APIKey}, body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return readSSE(resp.Body, func(event, data string) error {
			var d struct {
				Candidates []struct {
					Content struct {
						Parts []struct {
							Text    string `json:"text"`
							Thought bool   `json:"thought"`
						} `json:"parts"`
					} `json:"content"`
				} `json:"candidates"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &d) != nil {
				return nil
			}
			if d.Error != nil {
				return errors.New(d.Error.Message)
			}
			for _, c := range d.Candidates {
				for _, pt := range c.Content.Parts {
					if pt.Text != "" && !pt.Thought {
						onDelta(pt.Text)
					}
				}
			}
			return nil
		})
	default: // OpenAI-compatible
		var userContent any = r.User
		if len(r.Images) > 0 {
			arr := []map[string]any{}
			for _, im := range r.Images {
				arr = append(arr, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + im.Mime + ";base64," + im.B64}})
			}
			arr = append(arr, map[string]any{"type": "text", "text": r.User})
			userContent = arr
		}
		body := map[string]any{"model": p.Model, "stream": true, "max_tokens": maxTokens,
			"messages": []any{map[string]any{"role": "system", "content": r.System}, map[string]any{"role": "user", "content": userContent}}}
		h := map[string]string{}
		if p.APIKey != "" {
			h["Authorization"] = "Bearer " + p.APIKey
		}
		resp, err := llmPost(ctx, p.base()+"/chat/completions", h, body)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		return readSSE(resp.Body, func(event, data string) error {
			if data == "[DONE]" {
				return io.EOF
			}
			var d struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(data), &d) != nil {
				return nil
			}
			if d.Error != nil {
				return errors.New(d.Error.Message)
			}
			for _, c := range d.Choices {
				if c.Delta.Content != "" {
					onDelta(c.Delta.Content)
				}
			}
			return nil
		})
	}
}

// readSSE parses a text/event-stream body; fn returning io.EOF ends the stream cleanly.
func readSSE(r io.Reader, fn func(event, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	event, data := "", []string{}
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		err := fn(event, strings.Join(data, "\n"))
		event, data = "", nil
		return err
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := flush(); err != nil && err != io.EOF {
		return err
	}
	return sc.Err()
}

// listModels returns model ids the provider exposes (best effort).
func listModels(ctx context.Context, p LLMProvider) ([]string, error) {
	var url string
	h := map[string]string{}
	switch p.Kind {
	case "anthropic":
		url = p.base() + "/v1/models?limit=100"
		h["x-api-key"], h["anthropic-version"] = p.APIKey, "2023-06-01"
	case "gemini":
		url = p.base() + "/v1beta/models?pageSize=200"
		h["x-goog-api-key"] = p.APIKey
	default:
		url = p.base() + "/models"
		if p.APIKey != "" {
			h["Authorization"] = "Bearer " + p.APIKey
		}
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
	for k, v := range h {
		req.Header.Set(k, v)
	}
	resp, err := llmClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		return nil, httpErr(resp.StatusCode, raw)
	}
	var d struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	json.Unmarshal(raw, &d)
	var out []string
	for _, m := range d.Data {
		out = append(out, m.ID)
	}
	for _, m := range d.Models {
		ok := len(m.Methods) == 0
		for _, x := range m.Methods {
			if x == "generateContent" {
				ok = true
			}
		}
		if ok {
			out = append(out, strings.TrimPrefix(m.Name, "models/"))
		}
	}
	sort.Strings(out)
	return out, nil
}

// ---- prompt assembly ----

var modeHint = map[string]string{
	"T2VA":   "T2VA (text to video with audio). There is no reference picture.",
	"I2VA":   "I2VA (first frame to video). <Picture 1> is the first frame of the video.",
	"FL2VA":  "FL2VA (first and last frame). Picture 1 is the first frame and Picture 2 is the last frame.",
	"L2VA":   "L2VA (last frame). <Picture 1> is the last frame of the video.",
	"Ref2VA": "Ref2VA (full reference). The attached pictures, in the given order, show the subjects and look to keep; the video itself is newly composed.",
}

func buildPromptRequest(mode string, dur float64, idea, current, instruction string, nImages int) llmRequest {
	if _, ok := modeHint[mode]; !ok {
		mode = "T2VA"
	}
	if dur < 4 {
		dur = 4
	}
	if dur > 15 {
		dur = 15
	}
	guide, imported := promptGuide()
	intro := "You are an expert prompt writer for an audio-video generation model. Use the advice below.\n\n"
	if imported {
		intro = "You are an expert prompt writer for the MiniMax H3 audio-video generation model. Follow the guide below exactly.\n\n"
	}
	sys := intro + guide +
		"\n\n# Output contract\n" +
		"- Output ONLY the final prompt text, ready to paste into the model. No preamble, no explanations, no markdown code fences, no headings other than the required field names.\n" +
		"- Write in English, except dialogue/lyrics inside <d> and visible on-screen text, which keep the user's original language (usually Chinese).\n" +
		"- Match all timing to the requested duration.\n"
	var u strings.Builder
	fmt.Fprintf(&u, "Mode: %s\nTarget duration: %.0f seconds (use %.2f where the format needs S.SS).\n", modeHint[mode], dur, dur)
	if nImages > 0 {
		fmt.Fprintf(&u, "%d reference picture(s) are attached in order (Picture 1, Picture 2, …). Ground the description in what is actually visible in them.\n", nImages)
	} else if mode != "T2VA" {
		u.WriteString("No picture is attached; write the prompt assuming the referenced picture(s) match the idea.\n")
	}
	if strings.TrimSpace(current) != "" && strings.TrimSpace(instruction) != "" {
		u.WriteString("\nRevise the existing prompt below according to the instruction, keeping everything else intact and the format valid.\n")
		u.WriteString("\nInstruction (may be Chinese):\n" + instruction + "\n")
		u.WriteString("\nExisting prompt:\n" + current + "\n")
	} else {
		u.WriteString("\nUser idea (may be Chinese; expand it into a vivid, concrete, shot-by-shot prompt):\n" + idea + "\n")
	}
	return llmRequest{System: sys, User: u.String()}
}

// ---- HTTP handlers ----

func llmWriteHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider    string   `json:"provider"`
		Mode        string   `json:"mode"`
		Duration    float64  `json:"duration"`
		Idea        string   `json:"idea"`
		Current     string   `json:"current"`
		Instruction string   `json:"instruction"`
		Images      []string `json:"images"` // data URLs
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&in); err != nil {
		errOut(w, 400, err)
		return
	}
	p, err := findProvider(in.Provider)
	if err != nil {
		errOut(w, 400, err)
		return
	}
	if strings.TrimSpace(in.Idea) == "" && strings.TrimSpace(in.Instruction) == "" {
		errOut(w, 400, errString("先写下你的想法"))
		return
	}
	req := buildPromptRequest(in.Mode, in.Duration, in.Idea, in.Current, in.Instruction, len(in.Images))
	for _, du := range in.Images {
		if i := strings.Index(du, ";base64,"); strings.HasPrefix(du, "data:") && i > 0 {
			req.Images = append(req.Images, llmImage{Mime: du[5:i], B64: du[i+8:]})
		}
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	send := func(ev string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev, b)
		if fl != nil {
			fl.Flush()
		}
	}
	send("start", map[string]string{"provider": p.Name, "model": p.Model})
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	err = streamLLM(ctx, p, req, 4096, func(s string) { send("delta", s) })
	if err != nil {
		send("error", err.Error())
		return
	}
	send("done", true)
}

// ---- imported prompt guide (settings → 大模型 API) ----

func promptGuideHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		out := map[string]any{"imported": false}
		if st, err := os.Stat(userGuidePath()); err == nil {
			out = map[string]any{"imported": true, "size": st.Size(), "modified": st.ModTime().Unix()}
		}
		jsonOut(w, 200, out)
	case "POST":
		var in struct {
			Name string `json:"name"`
			Text string `json:"text"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4*guideMaxBytes)).Decode(&in); err != nil {
			errOut(w, 400, errString("读取失败："+err.Error()))
			return
		}
		ext := strings.ToLower(filepath.Ext(in.Name))
		switch {
		case ext != ".md" && ext != ".txt":
			errOut(w, 400, errString("只支持 .md 或 .txt 文件"))
		case strings.TrimSpace(in.Text) == "":
			errOut(w, 400, errString("文件是空的"))
		case len(in.Text) > guideMaxBytes:
			errOut(w, 400, errString("文件太大（最大 1 MB）"))
		case !utf8.ValidString(in.Text):
			errOut(w, 400, errString("文件不是 UTF-8 文本"))
		default:
			os.MkdirAll(filepath.Dir(userGuidePath()), 0o700)
			if err := os.WriteFile(userGuidePath(), []byte(in.Text), 0o600); err != nil {
				errOut(w, 500, err)
				return
			}
			jsonOut(w, 200, map[string]any{"imported": true, "size": len(in.Text)})
		}
	case "DELETE":
		os.Remove(userGuidePath())
		jsonOut(w, 200, map[string]any{"imported": false})
	}
}

func llmTestHandler(w http.ResponseWriter, r *http.Request) {
	var p LLMProvider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errOut(w, 400, err)
		return
	}
	p = withStoredKey(p)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var out strings.Builder
	start := time.Now()
	err := streamLLM(ctx, p, llmRequest{System: "Reply with exactly: OK", User: "ping"}, 16, func(s string) { out.WriteString(s) })
	if err != nil {
		errOut(w, 502, err)
		return
	}
	jsonOut(w, 200, map[string]any{"reply": strings.TrimSpace(out.String()), "ms": time.Since(start).Milliseconds()})
}

func llmModelsHandler(w http.ResponseWriter, r *http.Request) {
	var p LLMProvider
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		errOut(w, 400, err)
		return
	}
	p = withStoredKey(p)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	ms, err := listModels(ctx, p)
	if err != nil {
		errOut(w, 502, err)
		return
	}
	jsonOut(w, 200, map[string]any{"models": ms})
}

// withStoredKey swaps a masked key coming from the UI for the real one on disk.
func withStoredKey(p LLMProvider) LLMProvider {
	if p.APIKey == keyMask {
		p.APIKey = ""
		for _, q := range getConfig().LLMs {
			if q.ID == p.ID {
				p.APIKey = q.APIKey
			}
		}
	}
	return p
}

func httpErr(code int, raw []byte) error {
	msg := apiErrorText(raw)
	switch code {
	case 401, 403:
		return fmt.Errorf("API Key 无效或没有权限（%d）：%s。请确认 Key 复制完整、而且是这家服务（同一个 Base URL）发的 Key", code, msg)
	case 404:
		return fmt.Errorf("接口或模型不存在（404）：%s。检查 Base URL 和模型名", msg)
	case 429:
		return fmt.Errorf("请求太频繁或余额不足（429）：%s", msg)
	}
	return fmt.Errorf("HTTP %d：%s", code, msg)
}
