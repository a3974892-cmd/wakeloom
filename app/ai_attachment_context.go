package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

type aiAttachmentExcerpt struct {
	Name  string `json:"name"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}
type aiAttachmentUsage struct {
	Name          string                `json:"name"`
	Files         int                   `json:"files"`
	UsedFiles     int                   `json:"used_files"`
	Chars         int                   `json:"chars"`
	TotalChars    int                   `json:"total_chars"`
	Images        int                   `json:"images"`
	OmittedImages int                   `json:"omitted_images"`
	Excerpts      []aiAttachmentExcerpt `json:"excerpts"`
}

func aiPartFullText(p aiAttachmentPart) (string, error) {
	if p.TextID == "" {
		return p.Text, nil
	}
	name, e := aiContentPath(p.TextID, ".txt")
	if e != nil {
		return "", e
	}
	f, e := os.Open(name)
	if e != nil {
		return "", e
	}
	defer f.Close()
	b, e := aiReadBounded(f, aiSkillPackageTextLimit)
	return string(b), e
}

func aiEachPartChunk(ctx context.Context, p aiAttachmentPart, fn func(string, int, int)) error {
	var r io.Reader
	var f *os.File
	if p.TextID != "" {
		name, e := aiContentPath(p.TextID, ".txt")
		if e != nil {
			return e
		}
		f, e = os.Open(name)
		if e != nil {
			return errors.New("附件文字已不存在，请重新添加")
		}
		defer f.Close()
		r = f
	} else {
		r = strings.NewReader(p.Text)
	}
	reader := bufio.NewReaderSize(r, 64<<10)
	chunk := make([]rune, 0, 1000)
	position := 0
	start := 1
	for {
		ru, _, e := reader.ReadRune()
		if e == io.EOF {
			if len(chunk) > 0 {
				fn(string(chunk), start, position)
			}
			return nil
		}
		if e != nil {
			return e
		}
		chunk = append(chunk, ru)
		position++
		if len(chunk) == 1000 {
			if e = ctx.Err(); e != nil {
				return e
			}
			fn(string(chunk), start, position)
			chunk = chunk[:0]
			start = position + 1
		}
	}
}

func aiReadPartImage(p aiAttachmentPart) (string, error) {
	if p.ImageID == "" {
		return p.Image, nil
	}
	name, e := aiContentPath(p.ImageID, ".jpg")
	if e != nil {
		return "", e
	}
	b, e := os.ReadFile(name)
	if e != nil {
		return "", errors.New("图片附件已不存在，请重新添加")
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func aiExpandStoredAttachments(ctx context.Context, messages []map[string]any, reserved int, limits ...int) (bool, []aiAttachmentUsage, error) {
	limit := aiTextLimit
	if len(limits) > 0 {
		limit = limits[0]
	}
	type ref struct {
		owner, msg               int
		part                     aiAttachmentPart
		text                     string
		start, end, score, order int
	}
	refs := []ref{}
	pictures := []ref{}
	usage := []aiAttachmentUsage{}
	base := reserved
	query := ""
	hasAttachments := false
	directImages := 0
	for _, m := range messages {
		c, _ := m["content"].(string)
		base += utf8.RuneCountInString(c)
		if m["role"] == "user" {
			query = c
		}
		if imgs, ok := m["images"].([]any); ok {
			directImages += len(imgs)
		}
		if imgs, ok := m["images"].([]string); ok {
			directImages += len(imgs)
		}
	}
	if directImages > 8 {
		return false, nil, errors.New("每次最多发送 8 张图片")
	}
	terms := aiSkillQueryTerms(query)
	order := 0
	maxRefs := max(64, limit/1000+8)
	trimRefs := func() {
		sort.SliceStable(refs, func(i, j int) bool {
			if refs[i].score != refs[j].score {
				return refs[i].score > refs[j].score
			}
			return refs[i].order < refs[j].order
		})
		if len(refs) > maxRefs {
			refs = refs[:maxRefs]
		}
	}
	keep := func(candidate ref) {
		refs = append(refs, candidate)
		if len(refs) >= maxRefs*2 {
			trimRefs()
		}
	}
	for index, m := range messages {
		var ids []struct {
			ID string `json:"id"`
		}
		if raw, ok := m["attachments"]; ok {
			b, e := json.Marshal(raw)
			if e != nil || json.Unmarshal(b, &ids) != nil || len(ids) > 8 {
				return false, nil, errors.New("附件列表无效，每条消息最多 8 组附件")
			}
		}
		if len(ids) > 0 && m["role"] != "user" {
			return false, nil, errors.New("只有用户消息可以包含附件")
		}
		hasAttachments = hasAttachments || len(ids) > 0
		for _, id := range ids {
			a, e := loadAIAttachment(id.ID)
			if e != nil {
				return false, nil, e
			}
			owner := len(usage)
			usage = append(usage, aiAttachmentUsage{Name: a.Name, Files: a.Files, TotalChars: a.Chars, Excerpts: []aiAttachmentExcerpt{}})
			for _, part := range a.Parts {
				name := strings.ToLower(part.Name)
				nameScore := 0
				for _, term := range terms {
					if strings.Contains(name, term) {
						nameScore += 20
					}
				}
				if part.Image != "" || part.ImageID != "" {
					pictures = append(pictures, ref{owner: owner, msg: index, part: part, score: nameScore, order: order})
					order++
					continue
				}
				chunkIndex := 0
				e = aiEachPartChunk(ctx, part, func(text string, start, end int) {
					score := nameScore * 100
					lower := strings.ToLower(text)
					for _, term := range terms {
						if strings.Contains(lower, term) {
							score += 100
						}
					}
					if chunkIndex == 0 {
						score += 10
					}
					keep(ref{owner: owner, msg: index, part: part, text: text, start: start, end: end, score: score, order: order})
					order++
					chunkIndex++
				})
				if e != nil {
					return false, nil, e
				}
			}
		}
		delete(m, "attachments")
	}
	if !hasAttachments {
		if directImages > 8 {
			return false, nil, errors.New("每次最多发送 8 张图片")
		}
		return directImages > 0, nil, nil
	}
	trimRefs()
	if base > limit {
		return false, nil, fmt.Errorf("本次聊天与技能超过 %d 字的发送预算；完整记录和附件仍保存在本机", limit)
	}
	selected := []ref{}
	used := map[string]bool{}
	sort.SliceStable(pictures, func(i, j int) bool {
		if pictures[i].score != pictures[j].score {
			return pictures[i].score > pictures[j].score
		}
		return pictures[i].order < pictures[j].order
	})
	chosenPictures := []ref{}
	count := directImages
	for _, p := range pictures {
		caption := "\n[图片附件：" + p.part.Name + "]"
		if count >= 8 || base+utf8.RuneCountInString(caption) > limit {
			usage[p.owner].OmittedImages++
			continue
		}
		base += utf8.RuneCountInString(caption)
		chosenPictures = append(chosenPictures, p)
		count++
	}
	for _, candidate := range refs {
		kind := "附件内容"
		if candidate.part.BinaryID != "" {
			kind = "程序静态报告（仅结构/字符串线索，未执行、未反编译，不能据此确认实际行为）"
		}
		prompt := fmt.Sprintf("\n\n--- %s：%s（第 %d–%d 字）---\n%s\n--- 附件片段结束 ---", kind, candidate.part.Name, candidate.start, candidate.end, candidate.text)
		n := utf8.RuneCountInString(prompt)
		if base+n > limit {
			continue
		}
		candidate.text = prompt
		selected = append(selected, candidate)
		base += n
		u := &usage[candidate.owner]
		u.Chars += candidate.end - candidate.start + 1
		u.Excerpts = append(u.Excerpts, aiAttachmentExcerpt{Name: candidate.part.Name, Start: candidate.start, End: candidate.end})
		used[fmt.Sprintf("%d/%s", candidate.owner, candidate.part.Name)] = true
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].order < selected[j].order })
	for _, candidate := range selected {
		c, _ := messages[candidate.msg]["content"].(string)
		messages[candidate.msg]["content"] = c + candidate.text
	}
	for _, p := range chosenPictures {
		u := &usage[p.owner]
		encoded, e := aiReadPartImage(p.part)
		if e != nil {
			return false, nil, e
		}
		var imgs []string
		if old, ok := messages[p.msg]["images"].([]string); ok {
			imgs = old
		}
		if old, ok := messages[p.msg]["images"].([]any); ok {
			for _, im := range old {
				if s, ok := im.(string); ok {
					imgs = append(imgs, s)
				}
			}
		}
		imgs = append(imgs, encoded)
		messages[p.msg]["images"] = imgs
		c, _ := messages[p.msg]["content"].(string)
		messages[p.msg]["content"] = c + "\n[图片附件：" + p.part.Name + "]"
		u.Images++
		used[fmt.Sprintf("%d/%s", p.owner, p.part.Name)] = true
	}
	for i := range usage {
		for key := range used {
			if strings.HasPrefix(key, fmt.Sprintf("%d/", i)) {
				usage[i].UsedFiles++
			}
		}
	}
	return count > 0, usage, nil
}

func aiSkillReservedRunes(ids []string, selection map[string][]string) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	total := 500
	for _, id := range ids {
		s, e := loadAISkill(id)
		if e != nil {
			return 0, e
		}
		total += utf8.RuneCountInString(s.Name+s.Description+s.Instructions) + 100
		if names, ok := selection[id]; ok && names != nil {
			wanted := map[string]bool{}
			for _, name := range names {
				wanted[name] = true
			}
			for _, p := range s.References {
				if wanted[p.Name] {
					total += utf8.RuneCountInString(p.Text+p.Name+s.Name) + 100
				}
			}
		}
	}
	return total, nil
}
