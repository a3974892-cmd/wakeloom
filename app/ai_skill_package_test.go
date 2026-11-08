package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func aiTestSkillPreview(t *testing.T, files map[string][]byte) aiSkillPackage {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	names := []string{}
	for name, b := range files {
		f, e := mw.CreateFormFile("files", filepath.Base(name))
		if e != nil {
			t.Fatal(e)
		}
		f.Write(b)
		names = append(names, name)
	}
	raw, _ := json.Marshal(names)
	mw.WriteField("paths", string(raw))
	mw.Close()
	r := httptest.NewRequest("POST", "/api/ai/skills/import", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	aiSkillPackagePreview(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var pkg aiSkillPackage
	if json.Unmarshal(w.Body.Bytes(), &pkg) != nil {
		t.Fatal(w.Body.String())
	}
	return pkg
}

func TestAISkillLargePackageInstallsWithoutIncreasingChatBudget(t *testing.T) {
	withTempConfig(t, Config{})
	large := []byte("BIG_REFERENCE_TOKEN\n" + strings.Repeat("x", 700000))
	pkg := aiTestSkillPreview(t, map[string][]byte{"review/SKILL.md": []byte(aiTestSkill), "review/references/large.md": large, "review/references/second.md": large, "review/references/checklist.md": []byte("Check boundary conditions.")})
	if len(pkg.Candidates) != 1 || pkg.Candidates[0].Chars < 1000000 || len(pkg.Parts) > 0 {
		t.Fatalf("bad preview %+v", pkg.Candidates)
	}
	body, _ := json.Marshal(map[string]any{"import_id": pkg.ID, "skill_path": pkg.Candidates[0].Path})
	w := httptest.NewRecorder()
	aiSkillInstall(w, httptest.NewRequest("POST", "/api/ai/skills/install", bytes.NewReader(body)))
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var s aiSkill
	json.Unmarshal(w.Body.Bytes(), &s)
	loaded, e := loadAISkill(s.ID)
	if e != nil || len(loaded.References) != 3 {
		t.Fatal("large installed skill did not persist", e)
	}
	msgs, usage, e := aiApplyConfiguredSkills([]map[string]any{{"role": "user", "content": "check boundary conditions"}}, []string{s.ID}, nil)
	if e != nil {
		t.Fatal(e)
	}
	sys := msgs[0]["content"].(string)
	if !strings.Contains(sys, "correctness issues first") || !strings.Contains(sys, "Check boundary conditions.") || strings.Contains(sys, "BIG_REFERENCE_TOKEN") || utf8.RuneCountInString(sys) > aiTextLimit {
		t.Fatal("incorrect large-skill context")
	}
	if usage[0].Omitted != 2 || len(usage[0].References) != 1 {
		t.Fatalf("omitted files not reported %+v", usage)
	}
	if _, _, e = aiApplyConfiguredSkills([]map[string]any{{"role": "user", "content": "test"}}, []string{s.ID}, map[string][]string{s.ID: {"references/large.md"}}); e == nil {
		t.Fatal("manual over-limit reference silently truncated")
	}
	msgs, usage, e = aiApplyConfiguredSkills([]map[string]any{{"role": "user", "content": "test"}}, []string{s.ID}, map[string][]string{s.ID: {}})
	if e != nil || len(usage[0].References) != 0 || usage[0].Auto || strings.Contains(msgs[0]["content"].(string), "Check boundary conditions.") {
		t.Fatal("instructions-only selection ignored", e)
	}
}

func TestAISkillRepositoryCatalogKeepsNestedSkillsSeparate(t *testing.T) {
	withTempConfig(t, Config{})
	zipBody := aiTestZIP(t, map[string][]byte{"repo/skills/SKILL.md": []byte(aiTestSkill), "repo/skills/INDEX.md": []byte("root index"), "repo/skills/child/SKILL.md": []byte(strings.Replace(aiTestSkill, "review-helper", "child-helper", 1)), "repo/skills/child/references/guide.md": []byte("child private reference")})
	pkg := &aiSkillPackage{}
	if e := pkg.addZIP(zipBody, true, ""); e != nil {
		t.Fatal(e)
	}
	if e := pkg.discover(); e != nil {
		t.Fatal(e)
	}
	if len(pkg.Candidates) != 2 {
		t.Fatal(pkg.Candidates)
	}
	root, e := pkg.selectSkill("skills")
	if e != nil || len(root.References) != 1 || root.References[0].Text != "root index" {
		t.Fatal("root absorbed nested skills", e)
	}
	child, e := pkg.selectSkill("skills/child")
	if e != nil || len(child.References) != 1 || child.References[0].Text != "child private reference" {
		t.Fatal("child did not import references", e)
	}
	if _, e = pkg.selectSkill("../../outside"); e == nil {
		t.Fatal("unlisted skill directory allowed")
	}
	if e = (&aiSkillPackage{}).addZIP(aiTestZIP(t, map[string][]byte{"repo/../escape.md": []byte("bad")}), true, ""); e == nil {
		t.Fatal("unsafe ZIP path accepted")
	}
}

func TestAISkillPackageRetainsDocumentReferenceSupport(t *testing.T) {
	pkg := &aiSkillPackage{}
	if err := pkg.addText("example/SKILL.md", []byte(aiTestSkill)); err != nil {
		t.Fatal(err)
	}
	if err := pkg.addText("example/references/guide.pdf", aiTestPDF()); err != nil {
		t.Fatal(err)
	}
	if err := pkg.discover(); err != nil {
		t.Fatal(err)
	}
	s, err := pkg.selectSkill("example")
	if err != nil || len(s.References) != 1 || !strings.Contains(s.References[0].Text, "Attachment PDF test") {
		t.Fatal("PDF reference support lost", err)
	}
}

func TestAISkillReferenceRankingAndExplicitSelection(t *testing.T) {
	withTempConfig(t, Config{})
	s := &aiSkill{ID: strings.Repeat("a", 32), Name: "ranker", Description: "test", Instructions: "Use the relevant guide.", References: []aiAttachmentPart{{Name: "references/general.md", Text: strings.Repeat("g", 9000)}, {Name: "references/android.md", Text: "ANDROID_GUIDE " + strings.Repeat("a", 9000)}}}
	p, _ := aiSkillPath(s.ID)
	os.MkdirAll(filepath.Dir(p), 0700)
	writeJSON(p, s)
	msgs, usage, e := aiApplyConfiguredSkills([]map[string]any{{"role": "user", "content": "android analysis"}}, []string{s.ID}, nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(usage[0].References) != 1 || usage[0].References[0] != "references/android.md" || !strings.Contains(msgs[0]["content"].(string), "ANDROID_GUIDE") {
		t.Fatal("reference ranking ignored query", usage)
	}
	_, usage, e = aiApplyConfiguredSkills([]map[string]any{{"role": "user", "content": "android analysis"}}, []string{s.ID}, map[string][]string{s.ID: {"references/general.md"}})
	if e != nil || usage[0].References[0] != "references/general.md" {
		t.Fatal("manual reference did not override automatic choice", e)
	}
}

func TestAISkillActualRepositoryPackage(t *testing.T) {
	withTempConfig(t, Config{})
	p := os.Getenv("WAKELOOM_TEST_SKILL_ZIP")
	if p == "" {
		t.Skip("optional user-supplied repository ZIP")
	}
	b, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	pkg := &aiSkillPackage{}
	if e = pkg.addZIP(b, true, ""); e != nil {
		t.Fatal(e)
	}
	if e = pkg.discover(); e != nil {
		t.Fatal(e)
	}
	if len(pkg.Candidates) < 2 {
		t.Fatal("expected a repository with multiple skills")
	}
	found := false
	for _, c := range pkg.Candidates {
		if c.Path == "skills" {
			found = true
			s, e := pkg.selectSkill(c.Path)
			if e != nil || s.Name != "reverse-skill-router" {
				t.Fatal(e)
			}
		}
	}
	if !found {
		t.Fatal("main router not found")
	}
	t.Logf("actual repository: %d skill candidates; %d text bytes", len(pkg.Candidates), pkg.Bytes)
	aiTestSavePackage(t, pkg)
	result := aiTestBatch(t, pkg, nil, true)
	if result["installed"] != float64(len(pkg.Candidates)) || len(result["failed"].([]any)) != 0 {
		t.Fatal(result)
	}
	result = aiTestBatch(t, pkg, nil, true)
	if result["existing"] != float64(len(pkg.Candidates)) {
		t.Fatal(result)
	}
}
