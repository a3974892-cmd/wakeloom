package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

type aiSkill struct {
	SourcePath   string             `json:"source_path,omitempty"`
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Description  string             `json:"description"`
	Instructions string             `json:"instructions,omitempty"`
	References   []aiAttachmentPart `json:"references,omitempty"`
	Files        int                `json:"files"`
	Chars        int                `json:"chars"`
	Skipped      []string           `json:"skipped,omitempty"`
}

var aiSkillMu sync.Mutex

func aiSkillPath(id string) (string, error) {
	b, err := hex.DecodeString(id)
	if err != nil || len(b) != 16 {
		return "", errors.New("技能编号无效")
	}
	return filepath.Join(dataDir, "ai-skills", id+".json"), nil
}

func loadAISkill(id string) (*aiSkill, error) {
	p, err := aiSkillPath(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, errors.New("技能已被删除，请重新选择")
	}
	defer f.Close()
	var s aiSkill
	if json.NewDecoder(io.LimitReader(f, 24<<20)).Decode(&s) != nil {
		return nil, errors.New("技能内容无法读取，请重新安装")
	}
	return &s, nil
}

func aiSkillFromAttachment(a *aiAttachment) (*aiSkill, error) {
	for i := range a.Parts {
		if a.Parts[i].TextID != "" {
			text, e := aiPartFullText(a.Parts[i])
			if e != nil {
				return nil, e
			}
			a.Parts[i].Text = text
		}
	}
	var main *aiAttachmentPart
	for i := range a.Parts {
		if a.Parts[i].Image == "" && strings.EqualFold(path.Base(a.Parts[i].Name), "SKILL.md") {
			if main != nil {
				return nil, errors.New("发现多个 SKILL.md，请只选择一个技能的文件夹或 ZIP")
			}
			main = &a.Parts[i]
		}
	}
	if main == nil {
		return nil, errors.New("未找到 SKILL.md，请选择技能文件、技能文件夹或包含它的 ZIP")
	}
	text := strings.ReplaceAll(main.Text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, errors.New("SKILL.md 需要以包含 name 和 description 的 YAML 说明开头")
	}
	front, body, ok := strings.Cut(text[4:], "\n---")
	if !ok {
		return nil, errors.New("SKILL.md 的 YAML 说明没有结束分隔线")
	}
	var meta struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
	}
	if yaml.Unmarshal([]byte(front), &meta) != nil || strings.TrimSpace(meta.Name) == "" || strings.TrimSpace(meta.Description) == "" {
		return nil, errors.New("SKILL.md 的 name 或 description 缺失或格式不正确")
	}
	if len(meta.Name) > 200 || len(meta.Description) > 4000 {
		return nil, errors.New("技能名称或说明过长")
	}
	if strings.TrimSpace(body) == "" {
		return nil, errors.New("SKILL.md 没有技能指令正文")
	}
	h := sha256.Sum256([]byte(strings.TrimSpace(meta.Name)))
	s := &aiSkill{ID: hex.EncodeToString(h[:16]), Name: strings.TrimSpace(meta.Name), Description: strings.TrimSpace(meta.Description), Instructions: strings.TrimSpace(body), Files: 1, Chars: utf8.RuneCountInString(text), Skipped: append([]string{}, a.Skipped...)}
	root := path.Dir(main.Name)
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}
	for _, p := range a.Parts {
		if p.Name == main.Name {
			continue
		}
		if !strings.HasPrefix(p.Name, prefix) {
			s.Skipped = append(s.Skipped, p.Name)
			continue
		}
		if p.Image != "" {
			s.Skipped = append(s.Skipped, p.Name+"（图片资源不作为技能指令）")
			continue
		}
		p.Name = strings.TrimPrefix(p.Name, prefix)
		s.References = append(s.References, p)
		s.Files++
		s.Chars += utf8.RuneCountInString(p.Text)
	}
	return s, nil
}

func aiSkillList(w http.ResponseWriter, r *http.Request) {
	aiSkillMu.Lock()
	defer aiSkillMu.Unlock()
	entries, _ := os.ReadDir(filepath.Join(dataDir, "ai-skills"))
	list := []aiSkill{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		s, e := loadAISkill(strings.TrimSuffix(entry.Name(), ".json"))
		if e != nil {
			continue
		}
		s.Instructions = ""
		s.References = nil
		list = append(list, *s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	jsonOut(w, 200, map[string]any{"skills": list})
}

func aiSkillInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Attachment string `json:"attachment"`
		GitHub     string `json:"github"`
		Replace    bool   `json:"replace"`
		ImportID   string `json:"import_id"`
		SkillPath  string `json:"skill_path"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
		errOut(w, 400, errors.New("安装请求无效"))
		return
	}
	var s *aiSkill
	var err error
	if in.ImportID != "" {
		var pkg *aiSkillPackage
		pkg, err = loadAISkillPackage(in.ImportID)
		if err == nil {
			s, err = pkg.selectSkill(in.SkillPath)
			if s != nil {
				s.SourcePath = in.SkillPath
				s.ID = pkg.skillIDs()[in.SkillPath]
			}
		}
	} else if in.GitHub != "" {
		s, err = aiDownloadGitHubSkill(r, in.GitHub)
	} else {
		var a *aiAttachment
		a, err = loadAIAttachment(in.Attachment)
		if err == nil {
			s, err = aiSkillFromAttachment(a)
		}
	}
	if err != nil {
		errOut(w, 400, err)
		return
	}
	aiSkillMu.Lock()
	defer aiSkillMu.Unlock()
	p, _ := aiSkillPath(s.ID)
	if _, err := os.Stat(p); err == nil && !in.Replace {
		jsonOut(w, 409, map[string]any{"error": "同名技能已安装，是否替换？", "name": s.Name})
		return
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err == nil {
		err = writeJSON(p, s)
	}
	if err != nil {
		errOut(w, 500, err)
		return
	}
	s.Instructions = ""
	s.References = nil
	jsonOut(w, 200, s)
}

func aiSkillDelete(w http.ResponseWriter, r *http.Request) {
	p, err := aiSkillPath(r.PathValue("id"))
	if err != nil {
		errOut(w, 400, err)
		return
	}
	aiSkillMu.Lock()
	defer aiSkillMu.Unlock()
	if err = os.Remove(p); err != nil && !os.IsNotExist(err) {
		errOut(w, 500, err)
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}

func aiSkillDetail(w http.ResponseWriter, r *http.Request) {
	s, err := loadAISkill(r.PathValue("id"))
	if err != nil {
		errOut(w, 404, err)
		return
	}
	jsonOut(w, 200, s)
}

func aiApplySkills(messages []map[string]any, ids []string) ([]map[string]any, error) {
	msgs, _, err := aiApplyConfiguredSkills(messages, ids, nil)
	return msgs, err
}
