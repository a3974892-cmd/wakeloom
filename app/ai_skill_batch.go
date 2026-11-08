package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func (pkg *aiSkillPackage) skillIDs() map[string]string {
	ids, owners := map[string]string{}, map[string]string{}
	for _, c := range pkg.Candidates {
		h := sha256.Sum256([]byte(c.Name))
		id := hex.EncodeToString(h[:16])
		if old, ok := owners[id]; ok && old != c.Path {
			h = sha256.Sum256([]byte(c.Name + "\x00" + c.Path))
			id = hex.EncodeToString(h[:16])
		}
		owners[id] = c.Path
		ids[c.Path] = id
	}
	return ids
}

func aiSkillBatchInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ImportID string   `json:"import_id"`
		Paths    []string `json:"paths"`
		All      bool     `json:"all"`
		Replace  bool     `json:"replace"`
	}
	if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil {
		errOut(w, 400, errors.New("批量安装请求无效"))
		return
	}
	pkg, err := loadAISkillPackage(in.ImportID)
	if err != nil {
		errOut(w, 400, err)
		return
	}
	if in.All {
		in.Paths = nil
		for _, c := range pkg.Candidates {
			in.Paths = append(in.Paths, c.Path)
		}
	}
	if len(in.Paths) == 0 || len(in.Paths) > 256 {
		errOut(w, 400, errors.New("请先选择技能"))
		return
	}
	// Assign distinct IDs for distinct same-name entries in this package; ordinary existing IDs are retained.
	ids := pkg.skillIDs()
	unique := []string{}
	seenPaths := map[string]bool{}
	for _, p := range in.Paths {
		if !seenPaths[p] {
			seenPaths[p] = true
			unique = append(unique, p)
		}
	}
	in.Paths = unique
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")
	fl, _ := w.(http.Flusher)
	enc := json.NewEncoder(w)
	installed, existing := 0, 0
	failed := []string{}
	seen := map[string]bool{}
	for i, root := range in.Paths {
		if r.Context().Err() != nil {
			return
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		s, e := pkg.selectSkill(root)
		status := "installed"
		message := ""
		id := ""
		name := root
		if e == nil {
			s.ID = ids[root]
			s.SourcePath = root
			id, name = s.ID, s.Name
			p, _ := aiSkillPath(s.ID)
			aiSkillMu.Lock()
			if _, statErr := os.Stat(p); statErr == nil && !in.Replace {
				status = "exists"
			} else {
				e = os.MkdirAll(filepath.Dir(p), 0700)
				if e == nil {
					e = writeJSON(p, s)
				}
			}
			aiSkillMu.Unlock()
		}
		if e != nil {
			status = "error"
			message = e.Error()
			failed = append(failed, root)
		} else if status == "exists" {
			existing++
		} else {
			installed++
		}
		if enc.Encode(map[string]any{"done": i + 1, "total": len(in.Paths), "status": status, "path": root, "id": id, "name": name, "message": message}) != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
	enc.Encode(map[string]any{"finished": true, "installed": installed, "existing": existing, "failed": failed})
}

// Only explicit names in the user's own latest text activate an otherwise unchecked skill.
func aiResolveNamedSkills(ids []string, messages []map[string]any) ([]string, error) {
	if len(ids) > 8 {
		return nil, errors.New("每次最多使用 8 个技能，请取消部分固定启用的技能")
	}
	query := ""
	for _, m := range messages {
		if m["role"] == "user" {
			query, _ = m["content"].(string)
		}
	}
	if query == "" {
		return ids, nil
	}
	names := map[string]bool{}
	for _, id := range ids {
		s, e := loadAISkill(id)
		if e != nil {
			return nil, e
		}
		names[s.Name] = true
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "ai-skills"))
	matches := []aiSkill{}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		s, e := loadAISkill(strings.TrimSuffix(entry.Name(), ".json"))
		if e != nil || names[s.Name] {
			continue
		}
		re := regexp.MustCompile("(?i)(?:使用|启用|按照|按|use|using|apply|enable)\\s*[「『【`\"'“]*\\s*" + regexp.QuoteMeta(s.Name) + "(?:\\b|技能|[\\s`\"'”]|$)")
		found := false
		for _, span := range re.FindAllStringIndex(query, -1) {
			before := strings.TrimSpace(strings.ToLower(query[:span[0]]))
			negative := false
			for _, word := range []string{"不要", "禁止", "取消", "不", "别", "勿", "not", "don't"} {
				negative = negative || strings.HasSuffix(before, word)
			}
			if !negative {
				found = true
				break
			}
		}
		if found {
			matches = append(matches, *s)
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if strings.Count(matches[i].SourcePath, "/") != strings.Count(matches[j].SourcePath, "/") {
			return strings.Count(matches[i].SourcePath, "/") < strings.Count(matches[j].SourcePath, "/")
		}
		return matches[i].SourcePath < matches[j].SourcePath
	})
	for _, s := range matches {
		if names[s.Name] {
			continue
		}
		ids = append(ids, s.ID)
		names[s.Name] = true
	}
	if len(ids) > 8 {
		return nil, errors.New("指定和固定启用的技能合计超过 8 个，请减少本次使用的技能")
	}
	return ids, nil
}
