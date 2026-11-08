package main

import (
	"strings"
	"testing"
)

func TestOptionalVisionProfileKeepsTextProfileUnchanged(t *testing.T) {
	withTempConfig(t, Config{})
	profiles := localProfiles()
	text, vision := profiles["qwen38-27b-chat"], profiles["qwen38-27b-vision"]
	if text == nil || vision == nil {
		t.Fatal("missing profile")
	}
	old := text.aiSummary()
	if len(old["models"].([]string)) != 1 || old["llm"].(map[string]any)["projector_dest"] != nil {
		t.Fatal("text-only profile changed")
	}
	summary := vision.aiSummary()
	llm := summary["llm"].(map[string]any)
	if len(summary["models"].([]string)) != 2 || llm["projector_dest"] != "llm/mmproj-model-bf16.gguf" || llm["name"] == old["llm"].(map[string]any)["name"] {
		t.Fatal(summary)
	}
	raw, e := kitFS.ReadFile("kit/profiles/qwen38-27b-vision/profile.yaml")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = validateProfileYAML(raw); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"projector: missing", "projector: qwen38-27b-q8"} {
		if _, e = validateProfileYAML([]byte(strings.Replace(string(raw), "projector: qwen38-vision-projector", bad, 1))); e == nil {
			t.Fatal("bad projector accepted", bad)
		}
	}
}
