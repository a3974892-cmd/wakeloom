package main

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const aiSkillPackageTextLimit = 8 << 20
const aiSkillPackageFileLimit = 2000

type aiSkillCandidate struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Files       int    `json:"files"`
	Chars       int    `json:"chars"`
}

type aiSkillPackage struct {
	ID         string             `json:"id"`
	Parts      []aiAttachmentPart `json:"parts,omitempty"`
	Skipped    []string           `json:"skipped,omitempty"`
	Candidates []aiSkillCandidate `json:"candidates"`
	Bytes      int                `json:"bytes"`
}

// Installation uses its own bounded package budget, separate from disk-backed chat uploads.
func (p *aiSkillPackage) addText(name string, b []byte) error {
	name, err := aiSafeAttachmentName(name)
	if err != nil {
		return err
	}
	ext := strings.ToLower(path.Ext(name))
	base := path.Base(name)
	if aiAttachmentSkip(name) || (!aiTextExtension(ext) && ext != ".pdf" && ext != ".docx" && base != "LICENSE" && base != "README" && base != "Dockerfile") {
		p.Skipped = append(p.Skipped, name)
		return nil
	}
	if len(b) > 1<<20 && ext != ".pdf" && ext != ".docx" {
		p.Skipped = append(p.Skipped, name+"（超过 1 MB，未作为文字导入）")
		return nil
	}
	var text string
	if ext == ".pdf" {
		text, err = aiPDFText(b)
	} else if ext == ".docx" {
		text, err = aiDOCXText(b)
	} else {
		text, err = aiDecodeText(b)
	}
	if err != nil {
		p.Skipped = append(p.Skipped, name+"（未能读取："+err.Error()+"）")
		return nil
	}
	p.Bytes += len(text)
	if p.Bytes > aiSkillPackageTextLimit {
		return errors.New("技能包文字超过 8 MB，请只选择需要的技能目录")
	}
	p.Parts = append(p.Parts, aiAttachmentPart{Name: name, Text: text})
	if len(p.Parts) > aiSkillPackageFileLimit {
		return errors.New("技能包文件过多，请只选择需要的目录")
	}
	return nil
}

func (p *aiSkillPackage) addZIP(raw []byte, stripRoot bool, subdir string) error {
	z, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return errors.New("技能 ZIP 无法读取，请确认完整且没有加密")
	}
	if len(z.File) > 10000 {
		return errors.New("压缩包项目过多，请只选择技能目录")
	}
	prefix := strings.Trim(subdir, "/")
	if prefix != "" {
		prefix += "/"
	}
	var total uint64
	count := 0
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := f.Name
		if stripRoot {
			var ok bool
			_, name, ok = strings.Cut(name, "/")
			if !ok {
				continue
			}
		}
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		if _, err := aiSafeAttachmentName(name); err != nil {
			return err
		}
		if f.Mode()&os.ModeSymlink != 0 {
			p.Skipped = append(p.Skipped, name+"（符号链接）")
			continue
		}
		count++
		total += f.UncompressedSize64
		if count > aiSkillPackageFileLimit || total > 64<<20 {
			return errors.New("技能包解压后过大，请只选择技能目录")
		}
		ext := strings.ToLower(path.Ext(name))
		base := path.Base(name)
		if aiAttachmentSkip(name) || (!aiTextExtension(ext) && ext != ".pdf" && ext != ".docx" && base != "LICENSE" && base != "README" && base != "Dockerfile") {
			p.Skipped = append(p.Skipped, name)
			continue
		}
		limit := int64(1 << 20)
		if ext == ".pdf" || ext == ".docx" {
			limit = aiFileLimit
		}
		if f.UncompressedSize64 > uint64(limit) {
			p.Skipped = append(p.Skipped, name+"（超过 1 MB，未作为文字导入）")
			continue
		}
		r, e := f.Open()
		if e != nil {
			return errors.New("技能 ZIP 无法读取，可能已加密")
		}
		b, e := aiReadBounded(r, limit)
		r.Close()
		if e != nil {
			return e
		}
		if e = p.addText(name, b); e != nil {
			return e
		}
	}
	return nil
}

func (p *aiSkillPackage) discover() error {
	seen := map[string]bool{}
	for _, part := range p.Parts {
		if !strings.EqualFold(path.Base(part.Name), "SKILL.md") {
			continue
		}
		root := path.Dir(part.Name)
		if seen[root] {
			return fmt.Errorf("技能目录 %s 有重复 SKILL.md", root)
		}
		seen[root] = true
		s, err := aiSkillFromAttachment(&aiAttachment{Parts: []aiAttachmentPart{part}})
		if err != nil {
			p.Skipped = append(p.Skipped, part.Name+"（"+err.Error()+"）")
			continue
		}
		p.Candidates = append(p.Candidates, aiSkillCandidate{Path: root, Name: s.Name, Description: s.Description})
		if len(p.Candidates) > 256 {
			return errors.New("此技能库超过 256 个技能，请选择更具体的目录")
		}
	}
	if len(p.Candidates) == 0 {
		return errors.New("未找到有效 SKILL.md，请确认文件包含 name、description 和技能正文")
	}
	sort.Slice(p.Candidates, func(i, j int) bool {
		di, dj := strings.Count(p.Candidates[i].Path, "/"), strings.Count(p.Candidates[j].Path, "/")
		if di != dj {
			return di < dj
		}
		return p.Candidates[i].Path < p.Candidates[j].Path
	})
	for i, c := range p.Candidates {
		s, err := p.selectSkill(c.Path)
		if err != nil {
			return err
		}
		p.Candidates[i].Files = s.Files
		p.Candidates[i].Chars = s.Chars
	}
	return nil
}

func (p *aiSkillPackage) selectSkill(root string) (*aiSkill, error) {
	found := false
	for _, c := range p.Candidates {
		if c.Path == root {
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New("请从预览列表选择技能")
	}
	prefix := ""
	if root != "." {
		prefix = root + "/"
	}
	a := &aiAttachment{}
	for _, skipped := range p.Skipped {
		if strings.HasPrefix(skipped, prefix) {
			a.Skipped = append(a.Skipped, skipped)
		}
	}
	for _, part := range p.Parts {
		if !strings.HasPrefix(part.Name, prefix) {
			continue
		}
		nested := false
		for _, other := range p.Candidates {
			if other.Path != root && strings.HasPrefix(other.Path, prefix) && strings.HasPrefix(part.Name, other.Path+"/") {
				nested = true
				break
			}
		}
		if !nested {
			if strings.EqualFold(path.Base(part.Name), "SKILL.md") && path.Dir(part.Name) != root {
				continue
			}
			a.Parts = append(a.Parts, part)
		}
	}
	return aiSkillFromAttachment(a)
}

func aiSkillPackagePath(id string) (string, error) {
	b, e := hex.DecodeString(id)
	if e != nil || len(b) != 16 {
		return "", errors.New("技能包编号无效")
	}
	return filepath.Join(dataDir, "ai-skill-imports", id+".json"), nil
}

func loadAISkillPackage(id string) (*aiSkillPackage, error) {
	p, e := aiSkillPackagePath(id)
	if e != nil {
		return nil, e
	}
	f, e := os.Open(p)
	if e != nil {
		return nil, errors.New("技能预览已不存在，请重新选择文件或链接")
	}
	defer f.Close()
	var pkg aiSkillPackage
	if json.NewDecoder(io.LimitReader(f, 24<<20)).Decode(&pkg) != nil {
		return nil, errors.New("技能预览无法读取，请重新导入")
	}
	return &pkg, nil
}

func aiSkillPackagePreview(w http.ResponseWriter, r *http.Request) {
	pkg := &aiSkillPackage{}
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var in struct {
			GitHub string `json:"github"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in) != nil {
			errOut(w, 400, errors.New("导入请求无效"))
			return
		}
		var raw []byte
		var subdir string
		raw, subdir, err = aiFetchGitHubSkillZIP(r, in.GitHub, aiGitHubSkillClient())
		if err == nil {
			err = pkg.addZIP(raw, true, subdir)
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, aiUploadLimit)
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			if r.MultipartForm != nil {
				r.MultipartForm.RemoveAll()
			}
			errOut(w, 400, errors.New("技能包读取失败，每次最多 32 MB"))
			return
		}
		defer r.MultipartForm.RemoveAll()
		files := r.MultipartForm.File["files"]
		var names []string
		if raw := r.FormValue("paths"); raw != "" {
			if json.Unmarshal([]byte(raw), &names) != nil || len(names) != len(files) {
				errOut(w, 400, errors.New("技能文件列表无效"))
				return
			}
		}
		if len(files) == 0 || len(files) > aiSkillPackageFileLimit {
			errOut(w, 400, errors.New("请选择技能文件或技能目录，每次最多 2000 个文件"))
			return
		}
		for i, f := range files {
			name := f.Filename
			if len(names) > 0 {
				name = names[i]
			}
			if _, e := aiSafeAttachmentName(name); e != nil {
				err = e
				break
			}
			in, e := f.Open()
			if e != nil {
				err = e
				break
			}
			raw, e := aiReadBounded(in, aiUploadLimit)
			in.Close()
			if e != nil {
				err = e
				break
			}
			if strings.EqualFold(path.Ext(name), ".zip") {
				err = pkg.addZIP(raw, false, "")
			} else {
				err = pkg.addText(name, raw)
			}
			if err != nil {
				break
			}
		}
	}
	if err == nil {
		err = pkg.discover()
	}
	if err != nil {
		errOut(w, 400, err)
		return
	}
	id := make([]byte, 16)
	if _, e := rand.Read(id); e != nil {
		errOut(w, 500, e)
		return
	}
	pkg.ID = hex.EncodeToString(id)
	p, _ := aiSkillPackagePath(pkg.ID)
	if err = os.MkdirAll(filepath.Dir(p), 0700); err == nil {
		err = writeJSON(p, pkg)
	}
	if err != nil {
		errOut(w, 500, err)
		return
	}
	pkg.Parts = nil
	jsonOut(w, 200, pkg)
}
