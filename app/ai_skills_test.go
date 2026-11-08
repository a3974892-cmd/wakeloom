package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const aiTestSkill = "---\nname: review-helper\ndescription: Review code with the supplied checklist\n---\nAlways report correctness issues first.\nRead references/checklist.md.\n"

func TestAISkillInstallApplyReplaceAndRemove(t *testing.T) {
	withTempConfig(t, Config{})
	a, w := aiTestUpload(t, map[string][]byte{"review/SKILL.md": []byte(aiTestSkill), "review/references/checklist.md": []byte("Check boundary conditions."), "review/scripts/check.py": []byte("raise SystemExit('not executed')")})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	install := func(replace bool) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(map[string]any{"attachment": a.ID, "replace": replace})
		w := httptest.NewRecorder()
		aiSkillInstall(w, httptest.NewRequest("POST", "/api/ai/skills/install", bytes.NewReader(raw)))
		return w
	}
	w = install(false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var skill aiSkill
	json.Unmarshal(w.Body.Bytes(), &skill)
	if skill.Name != "review-helper" || skill.Files != 3 || skill.Instructions != "" {
		t.Fatalf("bad summary %+v", skill)
	}
	if w = install(false); w.Code != 409 {
		t.Fatal("duplicate replaced without confirmation")
	}
	if w = install(true); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	msgs, err := aiApplySkills([]map[string]any{{"role": "system", "content": "用中文回答"}, {"role": "user", "content": "帮我审查"}}, []string{skill.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatal("system instructions not merged")
	}
	sys := msgs[0]["content"].(string)
	for _, want := range []string{"用中文回答", "correctness issues first", "Check boundary conditions", "cannot execute scripts"} {
		if !strings.Contains(sys, want) {
			t.Fatal("missing skill content", want)
		}
	}
	if _, err := os.Stat(filepath.Join(dataDir, "not executed")); !os.IsNotExist(err) {
		t.Fatal("script executed")
	}
	list := httptest.NewRecorder()
	aiSkillList(list, httptest.NewRequest("GET", "/api/ai/skills", nil))
	if strings.Contains(list.Body.String(), "Always report") {
		t.Fatal("list returns full prompt")
	}
	r := httptest.NewRequest("DELETE", "/api/ai/skills/"+skill.ID, nil)
	r.SetPathValue("id", skill.ID)
	w = httptest.NewRecorder()
	aiSkillDelete(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := aiApplySkills([]map[string]any{}, []string{skill.ID}); err == nil {
		t.Fatal("deleted skill silently ignored")
	}
}

type aiSkillTestTransport func(*http.Request) (*http.Response, error)

func (f aiSkillTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAIGitHubSkillFolderDownloadAndValidation(t *testing.T) {
	zipBody := aiTestZIP(t, map[string][]byte{"owner-repo-hash/skills/review/SKILL.md": []byte(aiTestSkill), "owner-repo-hash/skills/review/references/checklist.md": []byte("From GitHub"), "owner-repo-hash/skills/other/SKILL.md": []byte(aiTestSkill)})
	client := &http.Client{Transport: aiSkillTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://api.github.com/repos/owner/repo/zipball/main" {
			t.Fatal(r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(zipBody)), Header: make(http.Header)}, nil
	})}
	s, err := aiDownloadGitHubSkillWithClient(httptest.NewRequest("POST", "/api/ai/skills/install", nil), "https://github.com/owner/repo/tree/main/skills/review", client)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "review-helper" || len(s.References) != 1 || s.References[0].Text != "From GitHub" {
		t.Fatalf("wrong skill %+v", s)
	}
	if _, err := aiSkillFromGitHubZIP(zipBody, ""); err == nil {
		t.Fatal("multi-skill repository selected silently")
	}
	for _, raw := range []string{"http://github.com/owner/repo", "https://evil.test/owner/repo", "https://github.com@evil.test/owner/repo", "https://github.com:443/owner/repo", "https://github.com/owner/repo/tree/main/../secret"} {
		if _, _, err := aiGitHubSkillURL(raw); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, raw := range []string{"https://evil.test/file.zip", "http://codeload.github.com/owner/repo", "https://codeload.github.com:444/file"} {
		u, _ := url.Parse(raw)
		if aiGitHubRedirect(&http.Request{URL: u}, nil) == nil {
			t.Fatal("redirect accepted", raw)
		}
	}
	if _, sub, e := aiGitHubSkillURL("https://github.com/owner/repo/blob/main/skills/review/SKILL.md"); e != nil || sub != "skills/review" {
		t.Fatal(sub, e)
	}
}

func TestAISkillMalformedPackageAndContextLimit(t *testing.T) {
	for _, text := range []string{"just text", "---\nname: missing-description\n---\nbody", "---\nname: x\ndescription: y\n---\n"} {
		if _, err := aiSkillFromAttachment(&aiAttachment{Parts: []aiAttachmentPart{{Name: "SKILL.md", Text: text}}}); err == nil {
			t.Fatal("accepted malformed skill")
		}
	}
	withTempConfig(t, Config{})
	a, w := aiTestUpload(t, map[string][]byte{"SKILL.md": []byte(aiTestSkill)})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	loaded, _ := loadAIAttachment(a.ID)
	s, err := aiSkillFromAttachment(loaded)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := aiSkillPath(s.ID)
	os.MkdirAll(filepath.Dir(p), 0700)
	writeJSON(p, s)
	if _, e := aiApplySkills([]map[string]any{{"role": "user", "content": strings.Repeat("字", aiTextLimit)}}, []string{s.ID}); e == nil {
		t.Fatal("oversize context accepted")
	}
}
