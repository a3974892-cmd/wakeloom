package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func aiTestSavePackage(t *testing.T, pkg *aiSkillPackage) {
	t.Helper()
	if len(pkg.Candidates) == 0 {
		if e := pkg.discover(); e != nil {
			t.Fatal(e)
		}
	}
	pkg.ID = strings.Repeat("b", 32)
	dir := filepath.Join(dataDir, "ai-skill-imports")
	os.MkdirAll(dir, 0700)
	if e := writeJSON(filepath.Join(dir, pkg.ID+".json"), pkg); e != nil {
		t.Fatal(e)
	}
}
func aiTestBatch(t *testing.T, pkg *aiSkillPackage, paths []string, all bool) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"import_id": pkg.ID, "paths": paths, "all": all})
	w := httptest.NewRecorder()
	aiSkillBatchInstall(w, httptest.NewRequest("POST", "/api/ai/skills/install-batch", bytes.NewReader(raw)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var final map[string]any
	for _, line := range strings.Split(strings.TrimSpace(w.Body.String()), "\n") {
		if e := json.Unmarshal([]byte(line), &final); e != nil {
			t.Fatal(e)
		}
	}
	return final
}

func TestAISkillBatchAllDuplicateNamesAndRetry(t *testing.T) {
	withTempConfig(t, Config{})
	pkg := &aiSkillPackage{}
	for i := 0; i < 89; i++ {
		name := fmt.Sprintf("batch-helper-%d", i)
		if i == 88 {
			name = "batch-helper-0"
		}
		pkg.addText(fmt.Sprintf("skills/s%02d/SKILL.md", i), []byte("---\nname: "+name+"\ndescription: Batch test\n---\nOnly review the supplied text.\n"))
	}
	aiTestSavePackage(t, pkg)
	first := aiTestBatch(t, pkg, nil, true)
	if first["installed"] != float64(89) || len(first["failed"].([]any)) != 0 {
		t.Fatal(first)
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "ai-skills"))
	if len(entries) != 89 {
		t.Fatal("same-name skills overwritten", len(entries))
	}
	second := aiTestBatch(t, pkg, nil, true)
	if second["existing"] != float64(89) || second["installed"] != float64(0) {
		t.Fatal(second)
	}
	// A bad path must not prevent good entries from completing; retry only the failed path.
	partial := aiTestBatch(t, pkg, []string{pkg.Candidates[0].Path, "missing/skill"}, false)
	if partial["existing"] != float64(1) || len(partial["failed"].([]any)) != 1 {
		t.Fatal(partial)
	}
	retry := aiTestBatch(t, pkg, []string{"missing/skill"}, false)
	if retry["installed"] != float64(0) || len(retry["failed"].([]any)) != 1 {
		t.Fatal(retry)
	}
	// Single-install and bulk-install use the same identity for a duplicate-name entry.
	c := pkg.Candidates[88]
	raw, _ := json.Marshal(map[string]any{"import_id": pkg.ID, "skill_path": c.Path})
	w := httptest.NewRecorder()
	aiSkillInstall(w, httptest.NewRequest("POST", "/", bytes.NewReader(raw)))
	if w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestAISkillExplicitNameOnlyUsesUserText(t *testing.T) {
	withTempConfig(t, Config{})
	pkg := &aiSkillPackage{}
	pkg.addText("skills/review/SKILL.md", []byte(strings.Replace(aiTestSkill, "review-helper", "code-audit", 1)))
	aiTestSavePackage(t, pkg)
	aiTestBatch(t, pkg, nil, true)
	for _, tc := range []struct {
		text string
		want int
	}{{"请使用 code-audit 技能审查这段代码", 1}, {"Use code-audit to review this", 1}, {"不要使用 code-audit 技能", 0}, {"do not use code-audit", 0}, {"介绍一下 code-audit", 0}, {"", 0}} {
		ids, e := aiResolveNamedSkills(nil, []map[string]any{{"role": "assistant", "content": "使用 code-audit"}, {"role": "user", "content": tc.text, "attachments": []map[string]string{{"id": "fake"}}}})
		if e != nil || len(ids) != tc.want {
			t.Errorf("%q: %v %v", tc.text, ids, e)
		}
	}
}
