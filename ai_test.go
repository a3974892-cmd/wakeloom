package main

import (
	"io/fs"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestAISummaryBuiltins(t *testing.T) {
	ps := localProfiles()
	q, r := ps["qwen38-27b-chat"], ps["realvisxl-v5"]
	if q == nil || r == nil {
		t.Fatal("ai profiles not embedded")
	}
	qa := q.aiSummary()
	if qa["type"] != "llm" || qa["llm"].(map[string]any)["name"] != "qwen3.8-27b" || qa["llm"].(map[string]any)["ctx"] != 32768 {
		t.Fatalf("qwen ai: %#v", qa)
	}
	ch := ps["chroma1-hd"].aiSummary()["image"].(map[string]any)
	if ch["arch"] != "chroma" || ch["clip_name"] != "t5xxl_fp16.safetensors" || ch["steps"] != 26 {
		t.Fatalf("chroma ai: %#v", ch)
	}
	ra := r.aiSummary()
	img := ra["image"].(map[string]any)
	if img["checkpoint"] != "RealVisXL_V5.0_fp16.safetensors" || img["cfg"] != 5.0 || img["width"] != 832 {
		t.Fatalf("realvis ai: %#v", ra)
	}
	sum := localProfilesSummary()
	for _, p := range sum["profiles"].([]map[string]any) {
		if p["id"] == "qwen38-27b-chat" && p["size_gb_default"] != 29.05 {
			t.Fatalf("size %v", p["size_gb_default"])
		}
		if p["id"] == "minimax-h3" && p["ai"] != nil {
			t.Fatal("h3 should have no ai")
		}
	}
	// the H3 profile must not change
	b, _ := fs.ReadFile(kitFS, "kit/profiles/minimax-h3/profile.yaml")
	var lp LocalProfile
	yaml.Unmarshal(b, &lp)
	if lp.aiSummary() != nil {
		t.Fatal("unexpected ai")
	}
}

func TestSDXLGraphFields(t *testing.T) {
	g, ckeys := chromaGraph("u.safetensors", "t5.safetensors", "ae.safetensors", "neg", 1024, 1024, 26, 3.8, 1, "")
	ct := &Template{ID: "c", Graph: g}
	cf := map[string]Field{}
	for _, f := range detectFields(ct) {
		cf[f.Key] = f
	}
	for _, k := range ckeys {
		if _, ok := cf[k]; !ok {
			t.Fatalf("chroma key %s not a field: %v", k, cf)
		}
	}
	for id, n := range g { // every link must point at an existing node
		for _, v := range n["inputs"].(map[string]any) {
			if isLink(v) {
				if _, ok := g[v.([]any)[0].(string)]; !ok {
					t.Fatalf("node %s links to missing %v", id, v)
				}
			}
		}
	}
	tp := &Template{ID: "x", Graph: sdxlGraph("a.safetensors", "neg", 832, 1216, 30, 5, "", "")}
	keys := map[string]Field{}
	for _, f := range detectFields(tp) {
		keys[f.Key] = f
	}
	for _, k := range []string{"2.text", "3.text", "4.width", "4.height", "5.seed", "5.steps", "5.cfg"} {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing field %s in %v", k, keys)
		}
	}
	if !keys["3.text"].Negative || keys["2.text"].Negative {
		t.Fatal("negative detection")
	}
}
