package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseHFLink(t *testing.T) {
	ok := []struct{ in, repo, rev, path string }{
		{"https://huggingface.co/SG161222/RealVisXL_V5.0", "SG161222/RealVisXL_V5.0", "", ""},
		{"huggingface.co/SG161222/RealVisXL_V5.0/", "SG161222/RealVisXL_V5.0", "", ""},
		{"https://huggingface.co/o/r/blob/main/a/b.safetensors", "o/r", "", "a/b.safetensors"},
		{"https://huggingface.co/o/r/resolve/v1.0/x.gguf?download=true", "o/r", "v1.0", "x.gguf"},
		{"https://huggingface.co/o/r/tree/dev", "o/r", "dev", ""},
		{"https://hf.co/o/r", "o/r", "", ""},
		{"hf://o/r/x.gguf", "o/r", "", "x.gguf"},
		{"hf://o/r@abc123/dir/x.gguf", "o/r", "abc123", "dir/x.gguf"},
		{"o/r", "o/r", "", ""},
		{"https://huggingface.co/api/models/o/r", "o/r", "", ""},
	}
	for _, c := range ok {
		r, err := parseHFLink(c.in)
		if err != nil || r.Repo != c.repo || r.Rev != c.rev || r.Path != c.path {
			t.Errorf("%s -> %+v %v", c.in, r, err)
		}
	}
	for _, bad := range []string{"", "https://example.com/o/r", "https://huggingface.co/datasets/o/r", "https://huggingface.co/o",
		"hf://o", "https://huggingface.co/o/r/blob/main/../x", "a/b/c", "https://huggingface.co/o/r/discussions"} {
		if r, err := parseHFLink(bad); err == nil {
			t.Errorf("%q accepted: %+v", bad, r)
		}
	}
	if u := (hfRef{Repo: "o/r", Rev: "v2"}).hfURL("a.gguf"); u != "hf://o/r@v2/a.gguf" {
		t.Error(u)
	}
	if u, err := normalizeURL("https://huggingface.co/o/r/blob/main/x/y.safetensors"); err != nil || u != "hf://o/r/x/y.safetensors" {
		t.Error(u, err)
	}
	for _, bad := range []string{"https://huggingface.co/o/r", "http://x/y.bin", "https://x/y.bin?token=1"} {
		if _, err := normalizeURL(bad); err == nil {
			t.Error("accepted", bad)
		}
	}
}

func TestRecommendGPUAndID(t *testing.T) {
	for _, c := range []struct {
		gb  float64
		gpu string
	}{{0.5, "L4"}, {6.94, "L4"}, {15, "L4"}, {16, "G4"}, {29.05, "G4"}, {65, "G4"}, {65.1, "H100"}, {200, "H100"}} {
		g, need, note := recommendGPU(c.gb)
		if g != c.gpu || (g == "H100") != (note != "") || need < c.gb {
			t.Errorf("%v GB -> %s %v %q, want %s", c.gb, g, need, note, c.gpu)
		}
	}
	for in, want := range map[string]string{
		"RealVisXL_V5.0":         "custom-realvisxl-v5-0",
		"  Qwen3.8 27B · 对话 ":    "custom-qwen3-8-27b",
		"custom-already":         "custom-already",
		"":                       "custom-model",
		"__":                     "custom-model",
		strings.Repeat("ab", 60): "custom-" + strings.Repeat("ab", 28),
	} {
		got := customID(in)
		if got != want || !profileIDRE.MatchString(got) || len(got) > 63 {
			t.Errorf("customID(%q) = %q, want %q", in, got, want)
		}
	}
	if q := ggufQuant("Model-7B-Q4_K_M.gguf"); q != "Q4_K_M" {
		t.Error(q)
	}
	if q := ggufQuant("model.IQ4_XS.gguf"); q != "IQ4_XS" {
		t.Error(q)
	}
	if q := ggufQuant("Qwen3.8-27B-abliterated-Q8_0.gguf"); q != "Q8_0" {
		t.Error(q)
	}
}

func TestPickQuant(t *testing.T) {
	small := []hfFile{{Path: "m-Q4_K_M.gguf", SizeGB: 4.7, Quant: "Q4_K_M"}, {Path: "m-Q8_0.gguf", SizeGB: 8.1, Quant: "Q8_0"}}
	if _, sel := candidates("llm", small, ""); sel[0] != "m-Q8_0.gguf" { // both fit L4 -> Q8_0
		t.Error(sel)
	}
	mid := []hfFile{{Path: "m-Q4_K_M.gguf", SizeGB: 14, Quant: "Q4_K_M"}, {Path: "m-Q8_0.gguf", SizeGB: 25, Quant: "Q8_0"}}
	if _, sel := candidates("llm", mid, ""); sel[0] != "m-Q4_K_M.gguf" { // Q8_0 would need a bigger GPU
		t.Error(sel)
	}
	if _, sel := candidates("llm", mid, "m-Q8_0.gguf"); sel[0] != "m-Q8_0.gguf" { // a file link wins
		t.Error(sel)
	}
}

// fakeHF serves testdata/hf/<name>/ as the Hugging Face API for repo ids mapped below.
func fakeHF(t *testing.T) {
	repos := map[string]string{"huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF": "gguf", "SG161222/RealVisXL_V5.0": "sdxl",
		"lodestones/Chroma1-HD": "chroma", "black-forest-labs/FLUX.1-dev": "flux"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		for repo, dir := range repos {
			var f string
			switch {
			case p == "/api/models/"+repo:
				f = "model.json"
			case p == "/api/models/"+repo+"/tree/main":
				f = "tree.json"
			case p == "/"+repo+"/resolve/main/model_index.json":
				f = "model_index.json"
			}
			if f != "" {
				b, err := os.ReadFile(filepath.Join("testdata/hf", dir, f))
				if err != nil {
					http.NotFound(w, r)
					return
				}
				w.Write(b)
				return
			}
		}
		if strings.Contains(p, "private") {
			http.Error(w, "unauthorized", 401)
			return
		}
		http.NotFound(w, r)
	}))
	old := hfAPIBase
	hfAPIBase = srv.URL
	t.Cleanup(func() { hfAPIBase = old; srv.Close() })
}

func TestInspectFourKinds(t *testing.T) {
	fakeHF(t)
	ctx := context.Background()

	r, err := inspectHF(ctx, "https://huggingface.co/huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "llm" || r.License != "apache-2.0" || r.Arch != "qwen3" || r.ContextLen != 262144 {
		t.Fatalf("gguf: %+v", r)
	}
	for _, f := range r.Files {
		if strings.Contains(f.Path, "mmproj") || strings.Contains(f.Path, "-of-") {
			t.Errorf("gguf: %s should be ignored", f.Path)
		}
	}
	if len(r.Files) != 4 || r.Selected[0] != "Huihui-Qwen3.8-27B-abliterated-Q8_0.gguf" || r.GPU != "G4" {
		t.Errorf("gguf files/selection/gpu: %d %v %s", len(r.Files), r.Selected, r.GPU)
	}
	if r.Params["ctx"] != int64(32768) || r.Params["think"] != true || r.ID != "custom-huihui-qwen3-8-27b-abliterated" {
		t.Errorf("gguf params/id: %v %s", r.Params, r.ID)
	}
	// a link to one quantisation selects it
	r, _ = inspectHF(ctx, "https://huggingface.co/huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF/blob/main/Huihui-Qwen3.8-27B-abliterated-Q5_K_M.gguf")
	if r.Selected[0] != "Huihui-Qwen3.8-27B-abliterated-Q5_K_M.gguf" {
		t.Error(r.Selected)
	}

	r, err = inspectHF(ctx, "SG161222/RealVisXL_V5.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "sdxl" || !strings.Contains(r.KindReason, "model_index") || r.Selected[0] != "RealVisXL_V5.0_fp16.safetensors" ||
		r.GPU != "L4" || len(r.Files) != 2 || r.License != "openrail++" {
		t.Errorf("sdxl: %+v", r)
	}

	r, err = inspectHF(ctx, "https://huggingface.co/lodestones/Chroma1-HD")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "chroma" || r.Selected[0] != "Chroma1-HD.safetensors" || r.GPU != "G4" || len(r.Extras) != 2 {
		t.Fatalf("chroma: %+v", r)
	}
	roles := map[string]string{}
	for _, e := range r.Extras {
		roles[e.Role] = e.URL
	}
	if !strings.Contains(roles["clip"], "t5xxl_fp16") || !strings.Contains(roles["vae"], "Comfy-Org") {
		t.Errorf("chroma extras: %v", roles)
	}

	r, err = inspectHF(ctx, "https://huggingface.co/black-forest-labs/FLUX.1-dev")
	if err != nil {
		t.Fatal(err)
	}
	if r.Kind != "comfy" || !r.Gated || r.GatedNote == "" || r.License != "flux-1-dev-non-commercial-license" {
		t.Errorf("flux: %+v", r)
	}
	dirs := map[string]string{}
	for _, f := range r.Files {
		dirs[f.Path] = f.Dir
	}
	if dirs["flux1-dev.safetensors"] != "diffusion_models" || dirs["ae.safetensors"] != "vae" {
		t.Errorf("flux dirs: %v", dirs)
	}

	for link, want := range map[string]string{"https://huggingface.co/nobody/missing": "404", "https://huggingface.co/x/private-repo": "权限"} {
		if _, err := inspectHF(ctx, link); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", link, err)
		}
	}
	hfAPIBase = "http://127.0.0.1:1" // nothing listens
	if _, err := inspectHF(ctx, "o/r"); err == nil || !strings.Contains(err.Error(), "连不上 Hugging Face") {
		t.Error(err)
	}
}

// specsForKinds returns one saved spec per kind, as the UI would send them.
func specsForKinds(t *testing.T) []CustomSpec {
	fakeHF(t)
	var out []CustomSpec
	for _, link := range []string{"huihui-ai/Huihui-Qwen3.8-27B-abliterated-GGUF", "SG161222/RealVisXL_V5.0", "lodestones/Chroma1-HD", "black-forest-labs/FLUX.1-dev"} {
		r, err := inspectHF(context.Background(), link)
		if err != nil {
			t.Fatal(err)
		}
		ref := hfRef{Repo: r.Repo}
		s := CustomSpec{ID: r.ID, Title: r.Title, Kind: r.Kind, GPU: r.GPU, Source: r.Link}
		for _, p := range r.Selected {
			s.Files = append(s.Files, specFile{Role: "model", URL: ref.hfURL(p), Dir: map[string]string{"llm": "llm", "sdxl": "checkpoints", "chroma": "diffusion_models"}[r.Kind], SizeGB: sizeOfPath(r.Files, p), License: r.License})
		}
		s.Files = append(s.Files, r.Extras...)
		if r.Kind == "comfy" {
			for _, f := range r.Files {
				s.Files = append(s.Files, specFile{Role: "extra", URL: ref.hfURL(f.Path), Dir: f.Dir, SizeGB: f.SizeGB, License: r.License})
			}
		}
		b, _ := json.Marshal(r.Params)
		switch r.Kind {
		case "llm":
			s.LLM = &pyLLM{}
			json.Unmarshal(b, s.LLM)
		case "sdxl", "chroma":
			s.Image = &pyImage{}
			json.Unmarshal(b, s.Image)
		}
		out = append(out, s)
	}
	return out
}

func TestBuildProfileAndRoundTrip(t *testing.T) {
	for _, s := range specsForKinds(t) {
		text, id, err := buildProfile(s)
		if err != nil {
			t.Fatalf("%s: %v", s.Kind, err)
		}
		if !strings.HasPrefix(id, "custom-") || !strings.Contains(string(text), "commit: 2c75dc1b") || !strings.Contains(string(text), "license:") ||
			!strings.Contains(string(text), "size_gb:") || !strings.Contains(string(text), "url: hf://") {
			t.Errorf("%s yaml:\n%s", s.Kind, text)
		}
		back, err := specFromYAML(text)
		// the UI gets the spec as JSON and sends it back unchanged when editing
		js, _ := json.Marshal(back)
		if s.Kind == "sdxl" && !strings.Contains(string(js), `"width":1024`) || s.Kind == "llm" && !strings.Contains(string(js), `"ctx":32768`) {
			t.Errorf("%s: spec JSON uses the wrong keys: %s", s.Kind, js)
		}
		back = CustomSpec{}
		json.Unmarshal(js, &back)
		if err != nil || back.Kind != s.Kind || back.ID != id || back.Title != s.Title || len(back.Files) != len(s.Files) || back.Source != s.Source {
			t.Errorf("%s round trip: %+v %v", s.Kind, back, err)
		}
		again, _, err := buildProfile(back)
		if err != nil || !bytes.Equal(again, text) {
			t.Errorf("%s: rebuilding from the saved spec changed the YAML (%v):\n%s\n---\n%s", s.Kind, err, text, again)
		}
	}
}

var badProfiles = map[string]string{
	"secret key":     "api_key: abc\n",
	"secret value":   "note: hf_abcdefghijklmnopqrstuvwxyz\n",
	"bad id":         "id: Bad_ID\n",
	"bad dest":       "models: [{id: m, dest: ../x.gguf, url: hf://o/r/x.gguf}]\n",
	"unknown dir":    "models: [{id: m, dest: nowhere/x.gguf, url: hf://o/r/x.gguf}]\n",
	"query url":      "models: [{id: m, dest: llm/x.gguf, url: \"https://h/x.gguf?token=1\"}]\n",
	"llm not gguf":   "models: [{id: m, dest: checkpoints/x.safetensors, url: hf://o/r/x.safetensors}]\nai: {type: llm, llm: {model: m}}\n",
	"bad ollama":     "models: [{id: m, dest: llm/x.gguf, url: hf://o/r/x.gguf}]\nai: {type: llm, llm: {model: m, name: Big Name}}\n",
	"sdxl dir":       "models: [{id: m, dest: llm/x.gguf, url: hf://o/r/x.gguf}]\nai: {type: image, image: {model: m}}\n",
	"chroma no clip": "models: [{id: m, dest: diffusion_models/x.safetensors, url: hf://o/r/x.safetensors}]\nai: {type: image, image: {arch: chroma, model: m}}\n",
	"bad arch":       "models: [{id: m, dest: checkpoints/x.safetensors, url: hf://o/r/x.safetensors}]\nai: {type: image, image: {arch: flux, model: m}}\n",
	"bad ai type":    "ai: {type: video}\n",
	"reserved arg":   "comfyui: {repo: https://github.com/comfyanonymous/ComfyUI, commit: abc, args: [--port, \"1\"]}\n",
	"dup model":      "models: [{id: m, dest: llm/x.gguf, url: hf://o/r/x.gguf}, {id: m, dest: llm/y.gguf, url: hf://o/r/y.gguf}]\n",
}

// badProfile merges an override into a minimal valid profile (later keys win in YAML -> use a map).
func badProfile(over string) []byte {
	base := map[string]any{"id": "custom-x", "comfyui": map[string]any{"repo": "https://github.com/comfyanonymous/ComfyUI", "commit": "abc"}}
	var o map[string]any
	if err := yaml.Unmarshal([]byte(over), &o); err == nil {
		for k, v := range o {
			base[k] = v
		}
	}
	b, _ := yaml.Marshal(base)
	return b
}

func TestValidateRejects(t *testing.T) {
	for name, over := range badProfiles {
		if _, err := validateProfileYAML(badProfile(over)); err == nil {
			t.Errorf("%s accepted:\n%s", name, badProfile(over))
		}
	}
	if _, err := validateProfileYAML(badProfile("")); err != nil {
		t.Error("minimal profile rejected:", err)
	}
	if _, err := validateProfileYAML(append(badProfile(""), []byte("# "+strings.Repeat("x", customMaxBytes))...)); err == nil {
		t.Error("oversized accepted")
	}
}

// python3 + the kit's own parse_profile must agree with the Go validator (skipped without python3 / PyYAML).
func pythonParse(t *testing.T, files map[string][]byte) map[string]string {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	if exec.Command(py, "-c", "import yaml").Run() != nil {
		t.Skip("PyYAML not installed")
	}
	dir := t.TempDir()
	for name, b := range files {
		os.WriteFile(filepath.Join(dir, name+".yaml"), b, 0o644)
	}
	kit, _ := filepath.Abs("kit")
	script := `
import json, sys, pathlib, yaml
sys.path.insert(0, sys.argv[1])
from gate.profiles import parse_profile, MODEL_DIRS
out = {"__dirs__": " ".join(sorted(MODEL_DIRS))}
for f in sorted(pathlib.Path(sys.argv[2]).glob("*.yaml")):
    try:
        parse_profile(yaml.safe_load(f.read_text()), f.parent)
        out[f.stem] = "ok"
    except Exception as e:
        out[f.stem] = "error: " + str(e)
print(json.dumps(out))
`
	o, err := exec.Command(py, "-I", "-c", script, kit, dir).CombinedOutput()
	if err != nil {
		t.Fatalf("python: %v\n%s", err, o)
	}
	res := map[string]string{}
	if err := json.Unmarshal(o, &res); err != nil {
		t.Fatalf("python output: %s", o)
	}
	return res
}

func TestProfilesMatchPython(t *testing.T) {
	files := map[string][]byte{}
	for _, s := range specsForKinds(t) {
		text, _, err := buildProfile(s)
		if err != nil {
			t.Fatal(err)
		}
		files["good-"+s.Kind] = text
	}
	for name, over := range badProfiles {
		files["bad-"+strings.ReplaceAll(name, " ", "-")] = badProfile(over)
	}
	res := pythonParse(t, files)
	var dirs []string
	for d := range modelDirs {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	if res["__dirs__"] != strings.Join(dirs, " ") {
		t.Errorf("MODEL_DIRS differ:\n go: %v\n py: %s", dirs, res["__dirs__"])
	}
	for name, b := range files {
		_, goErr := validateProfileYAML(b)
		py := res[name]
		if (goErr == nil) != (py == "ok") {
			t.Errorf("%s: Go %v, Python %s\n%s", name, goErr, py, b)
		}
		if strings.HasPrefix(name, "good-") && py != "ok" {
			t.Errorf("%s rejected by parse_profile: %s", name, py)
		}
	}
}

func TestLocalCustomStore(t *testing.T) {
	old := dataDir
	dataDir = t.TempDir()
	t.Cleanup(func() { dataDir = old })
	specs := specsForKinds(t)
	text, id, err := buildProfile(specs[1]) // sdxl
	if err != nil {
		t.Fatal(err)
	}
	g := &LocalGate{state: "sleeping"}
	call := func(method, p string, body any) (int, map[string]any) {
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		w := httptest.NewRecorder()
		g.customProfiles(w, httptest.NewRequest(method, p, rd))
		var out map[string]any
		json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}
	if c, out := call("POST", "/_gate/custom-profiles", map[string]string{"yaml": string(text)}); c != 200 || out["id"] != id {
		t.Fatal(c, out)
	}
	if p := localProfiles()[id]; p == nil || p.builtin || p.aiSummary() == nil {
		t.Fatalf("not loaded as a local profile: %+v", p)
	}
	if c, out := call("GET", "/_gate/custom-profiles", nil); c != 200 || len(out["items"].([]any)) != 1 {
		t.Fatal(c, out)
	}
	builtin := strings.Replace(string(text), "id: "+id, "id: realvisxl-v5", 1)
	if c, _ := call("POST", "/_gate/custom-profiles", map[string]string{"yaml": builtin}); c != 400 {
		t.Error("built-in id accepted", c)
	}
	noPrefix := strings.Replace(string(text), "id: "+id, "id: my-model", 1)
	if c, _ := call("POST", "/_gate/custom-profiles", map[string]string{"yaml": noPrefix}); c != 400 {
		t.Error("id without custom- accepted", c)
	}
	if c, _ := call("DELETE", "/_gate/custom-profiles/realvisxl-v5", nil); c != 404 {
		t.Error("built-in delete", c)
	}
	g.state, g.profile = "ready", id
	if c, _ := call("DELETE", "/_gate/custom-profiles/"+id, nil); c != 409 {
		t.Error("in-use delete", c)
	}
	g.state = "sleeping"
	if c, _ := call("DELETE", "/_gate/custom-profiles/"+id, nil); c != 200 {
		t.Error("delete", c)
	}
	if localProfiles()[id] != nil {
		t.Error("still loaded after delete")
	}
}

func TestHFTokenEnv(t *testing.T) {
	cfgMu.Lock()
	old := cfg.HFToken
	cfg.HFToken = " hf_abc "
	cfgMu.Unlock()
	t.Cleanup(func() { cfgMu.Lock(); cfg.HFToken = old; cfgMu.Unlock() })
	if got := hfTokenEnv(); got != "os.environ['HF_TOKEN']=\"hf_abc\"\n" {
		t.Errorf("%q", got)
	}
	cfgMu.Lock()
	cfg.HFToken = "x'); import os; ('"
	cfgMu.Unlock()
	if hfTokenEnv() != "" {
		t.Error("quote not rejected")
	}
}

func TestGatewayCustomProfiles(t *testing.T) {
	patched := false
	var saved string
	gate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/_gate/status":
			jsonOut(w, 200, map[string]string{"state": "sleeping"})
		case strings.HasPrefix(r.URL.Path, "/_gate/custom-profiles") && patched:
			if r.Method == "POST" {
				var in map[string]string
				json.NewDecoder(r.Body).Decode(&in)
				saved = in["yaml"]
				jsonOut(w, 200, map[string]string{"id": "x"})
				return
			}
			jsonOut(w, 200, map[string]any{"items": []any{}})
		default: // an unpatched gate shows its wake page for unknown paths
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>Comfy Colab Kit</html>"))
		}
	}))
	defer gate.Close()
	cfgMu.Lock()
	old := cfg
	cfg = Config{GateURL: gate.URL, Mode: "gateway"}
	cfgMu.Unlock()
	t.Cleanup(func() { cfgMu.Lock(); cfg = old; cfgMu.Unlock() })

	var out any
	if err := customGate("GET", "", nil, &out); !errors.Is(err, errGateOutdated) {
		t.Fatalf("unpatched gate: %v", err)
	}
	patched = true
	if err := customGate("GET", "", nil, &out); err != nil {
		t.Fatal(err)
	}
	text, _, _ := buildProfile(specsForKinds(t)[0])
	if err := customGate("POST", "", map[string]string{"yaml": string(text)}, &out); err != nil || saved != string(text) {
		t.Fatal(err)
	}
}
