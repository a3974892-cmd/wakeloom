package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Template is a ComfyUI workflow in API format ("Export (API)") plus a display name.
type Template struct {
	ID      string                    `json:"id"`
	Name    string                    `json:"name"`
	Created int64                     `json:"created"`
	Graph   map[string]map[string]any `json:"graph"`
	Hidden  []string                  `json:"hidden,omitempty"`  // detected fields the user moved to "advanced"
	Profile string                    `json:"profile,omitempty"` // start profile to wake for this template (custom models)
}

type Field struct {
	Key      string `json:"key"`  // "<node>.<input>"
	Kind     string `json:"kind"` // text | image | file | seed | number | bool | string
	Label    string `json:"label"`
	Node     string `json:"node"`
	NodeName string `json:"node_name"`
	Input    string `json:"input"`
	Default  any    `json:"default"`
	Primary  bool   `json:"primary"`
	Accept   string `json:"accept,omitempty"`
	Negative bool   `json:"negative,omitempty"`
	Class    string `json:"class,omitempty"`
	LabelCN  string `json:"label_cn,omitempty"`
	Hint     string `json:"hint,omitempty"`
	Rec      any    `json:"rec,omitempty"` // recommended value
}

var (
	textKeys = map[string]bool{"text": true, "prompt": true, "positive": true, "negative": true, "text_g": true, "text_l": true,
		"prompt_text": true, "positive_prompt": true, "negative_prompt": true, "caption": true, "system_prompt": true, "user_prompt": true}
	numKeys = map[string]bool{"width": true, "height": true, "length": true, "frames": true, "num_frames": true, "frame_count": true,
		"batch_size": true, "duration": true, "fps": true, "frame_rate": true, "steps": true, "cfg": true, "denoise": true, "resolution": true, "megapixels": true}
	seedKeys = map[string]bool{"seed": true, "noise_seed": true}
	imgExt   = []string{".png", ".jpg", ".jpeg", ".webp", ".bmp", ".gif"}
	avExt    = []string{".mp4", ".webm", ".mov", ".mkv", ".wav", ".mp3", ".flac", ".ogg", ".m4a", ".aac"}
)

func hasExt(s string, exts []string) bool {
	l := strings.ToLower(s)
	for _, e := range exts {
		if strings.HasSuffix(l, e) {
			return true
		}
	}
	return false
}

func isLink(v any) bool {
	a, ok := v.([]any)
	if !ok || len(a) != 2 {
		return false
	}
	_, s := a[0].(string)
	_, n := a[1].(float64)
	return s && n
}

func parseWorkflow(raw []byte) (map[string]map[string]any, error) {
	var any0 map[string]any
	if err := json.Unmarshal(raw, &any0); err != nil {
		return nil, errors.New("不是有效的 JSON 文件")
	}
	if _, ok := any0["nodes"]; ok {
		if _, ok2 := any0["links"]; ok2 {
			return nil, errors.New("这是 ComfyUI 的「界面格式」工作流。请在 ComfyUI 里用 菜单 → 导出 → 导出 (API) 另存一份再导入")
		}
	}
	if p, ok := any0["prompt"].(map[string]any); ok { // {"prompt": {...}} wrapper
		any0 = p
	}
	g := map[string]map[string]any{}
	for k, v := range any0 {
		n, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := n["class_type"].(string); !ok {
			continue
		}
		if _, ok := n["inputs"].(map[string]any); !ok {
			n["inputs"] = map[string]any{}
		}
		g[k] = n
	}
	if len(g) == 0 {
		return nil, errors.New("没有找到任何节点（需要 API 格式：{\"1\": {\"class_type\": ..., \"inputs\": {...}}}）")
	}
	return g, nil
}

func sortedNodeIDs(g map[string]map[string]any) []string {
	ids := make([]string, 0, len(g))
	for k := range g {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, ea := strconv.Atoi(strings.Split(ids[i], ":")[0])
		b, eb := strconv.Atoi(strings.Split(ids[j], ":")[0])
		if ea == nil && eb == nil && a != b {
			return a < b
		}
		return ids[i] < ids[j]
	})
	return ids
}

func nodeTitle(n map[string]any) string {
	if m, ok := n["_meta"].(map[string]any); ok {
		if t, ok := m["title"].(string); ok && t != "" {
			return t
		}
	}
	s, _ := n["class_type"].(string)
	return s
}

// detectFields lists every literal input; the obvious ones are marked primary.
func detectFields(t *Template) []Field {
	hidden := map[string]bool{}
	for _, h := range t.Hidden {
		hidden[h] = true
	}
	var out []Field
	roles := fieldRoles(t.Graph)
	for _, id := range sortedNodeIDs(t.Graph) {
		n := t.Graph[id]
		cls, _ := n["class_type"].(string)
		lcls := strings.ToLower(cls)
		title := nodeTitle(n)
		inputs := n["inputs"].(map[string]any)
		keys := make([]string, 0, len(inputs))
		for k := range inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := inputs[k]
			if isLink(v) {
				continue
			}
			lk := strings.ToLower(k)
			f := Field{Key: id + "." + k, Node: id, NodeName: title, Input: k, Default: v, Label: title + " · " + k}
			switch val := v.(type) {
			case string:
				switch {
				case (strings.HasPrefix(lcls, "loadimage") || lk == "image") && (hasExt(val, imgExt) || strings.HasPrefix(lcls, "loadimage")):
					f.Kind, f.Primary, f.Accept = "image", true, "image/*"
				case (strings.Contains(lcls, "loadaudio") || strings.Contains(lcls, "loadvideo") || lk == "audio" || lk == "video" || lk == "file") && hasExt(val, avExt):
					f.Kind, f.Primary, f.Accept = "file", true, "video/*,audio/*"
				case textKeys[lk] || ((strings.Contains(lcls, "text") || strings.Contains(lcls, "prompt") || strings.Contains(lcls, "string")) && (lk == "value" || lk == "string" || lk == "text")):
					f.Kind, f.Primary = "text", true
					tl := strings.ToLower(title)
					f.Negative = strings.Contains(lk, "negative") || strings.Contains(tl, "negative") || strings.Contains(title, "负")
				default:
					f.Kind = "string"
				}
			case float64:
				switch {
				case seedKeys[lk]:
					f.Kind, f.Primary = "seed", true
				case numKeys[lk]:
					f.Kind, f.Primary = "number", true
				default:
					f.Kind = "number"
				}
			case bool:
				f.Kind = "bool"
			default:
				continue // lists/objects: leave untouched
			}
			f.NodeName, f.Class = nodeNameCN(n), cls
			if r, ok := roles[f.Key]; ok {
				f.LabelCN, f.Hint, f.Rec, f.Primary = r.label, r.hint, r.rec, r.primary
			} else if info, ok := inputCN[strings.ToLower(f.Input)]; ok {
				f.LabelCN, f.Hint = info.label, info.hint
			}
			if hidden[f.Key] {
				f.Primary = false
			}
			out = append(out, f)
		}
	}
	return out
}

func cloneGraph(g map[string]map[string]any) map[string]map[string]any {
	b, _ := json.Marshal(g)
	var c map[string]map[string]any
	json.Unmarshal(b, &c)
	return c
}

func setInput(g map[string]map[string]any, key string, v any) error {
	i := strings.Index(key, ".") // node ids never contain "." (subgraphs use ":"), but input names can (e.g. "format.codec")
	if i < 0 {
		return fmt.Errorf("bad key %s", key)
	}
	n, ok := g[key[:i]]
	if !ok {
		return fmt.Errorf("节点 %s 不存在", key[:i])
	}
	n["inputs"].(map[string]any)[key[i+1:]] = v
	return nil
}

// ---- storage ----

func tplPath(id string) string { return filepath.Join(dataDir, "templates", id+".json") }

func listTemplates() []*Template {
	var out []*Template
	ents, _ := os.ReadDir(filepath.Join(dataDir, "templates"))
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if t, err := loadTemplate(strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

func loadTemplate(id string) (*Template, error) {
	if strings.ContainsAny(id, `/\.`) {
		return nil, errors.New("bad id")
	}
	b, err := os.ReadFile(tplPath(id))
	if err != nil {
		return nil, err
	}
	var t Template
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func saveTemplate(t *Template) error {
	if t.ID == "" {
		t.ID = newID()
		t.Created = time.Now().Unix()
	}
	return writeJSON(tplPath(t.ID), t)
}

func newID() string {
	return strconv.FormatInt(time.Now().UnixNano()/1000, 36)
}
