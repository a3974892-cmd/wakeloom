package main

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

type aiSkillUsage struct {
	ID                  string   `json:"id"`
	Name                string   `json:"name"`
	References          []string `json:"references"`
	Omitted             int      `json:"omitted"`
	Chars               int      `json:"chars"`
	Auto                bool     `json:"auto"`
	MainExcerpted       bool     `json:"main_excerpted,omitempty"`
	ExcerptedReferences []string `json:"excerpted_references,omitempty"`
}

var aiSkillWords = regexp.MustCompile(`[a-z][a-z0-9_-]{2,}`)

func aiSkillQueryTerms(query string) []string {
	r := []rune(strings.ToLower(query))
	if len(r) > 500 {
		r = r[:500]
	}
	text := string(r)
	set := map[string]bool{}
	for _, word := range aiSkillWords.FindAllString(text, 100) {
		if len(word) > 2 {
			set[word] = true
		}
	}
	for i := 1; i < len(r); i++ {
		if unicode.Is(unicode.Han, r[i-1]) && unicode.Is(unicode.Han, r[i]) {
			set[string(r[i-1:i+1])] = true
		}
	}
	out := make([]string, 0, len(set))
	for term := range set {
		out = append(out, term)
	}
	sort.Strings(out)
	return out
}

func aiApplyConfiguredSkills(messages []map[string]any, ids []string, selection map[string][]string) ([]map[string]any, []aiSkillUsage, error) {
	return aiApplySkillBudget(messages, ids, selection, aiTextLimit, false)
}

func aiApplySkillBudget(messages []map[string]any, ids []string, selection map[string][]string, limit int, compact bool) ([]map[string]any, []aiSkillUsage, error) {
	if len(ids) == 0 {
		return messages, nil, nil
	}
	if len(ids) > 8 {
		return nil, nil, errors.New("每次最多启用 8 个技能")
	}
	var out strings.Builder
	out.WriteString("The user enabled the following skills. Apply their instructions where relevant. Bundled scripts are reference text only; this chat cannot execute scripts, install dependencies, browse the web or use external tools. Do not claim such actions occurred. Only reference files included below are available in this request; do not claim to have read omitted files.\n")
	chars := 2
	query := ""
	for _, m := range messages {
		c, _ := m["content"].(string)
		chars += utf8.RuneCountInString(c)
		if m["role"] == "user" {
			query = c
		}
	}
	type reference struct {
		owner  int
		part   aiAttachmentPart
		prompt string
		score  int
		manual bool
	}
	refs := []reference{}
	usage := []aiSkillUsage{}
	seen := map[string]bool{}
	terms := aiSkillQueryTerms(query)
	if compact && limit-chars <= utf8.RuneCountInString(out.String()) {
		return messages, nil, nil
	}
	mainBudgets := []int{}
	if compact {
		needs := []int{}
		hasReferences := false
		for _, id := range ids {
			s, e := loadAISkill(id)
			if e != nil {
				return nil, nil, e
			}
			needs = append(needs, utf8.RuneCountInString(fmt.Sprintf("\n<skill name=%q>\n%s\n%s\n</skill>\n", s.Name, s.Description, s.Instructions)))
			names, manual := selection[id]
			if len(s.References) > 0 && (!manual || names == nil || len(names) > 0) {
				hasReferences = true
			}
		}
		available := max(0, limit-chars-utf8.RuneCountInString(out.String()))
		referenceReserve := 0
		if hasReferences {
			referenceReserve = min(2500, available/3)
		}
		mainBudgets = aiShareContext(needs, available-referenceReserve)
	}
	for item, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		s, err := loadAISkill(id)
		if err != nil {
			return nil, nil, err
		}
		main := fmt.Sprintf("\n<skill name=%q>\n%s\n%s\n</skill>\n", s.Name, s.Description, s.Instructions)
		excerpted := false
		if compact && utf8.RuneCountInString(main) > mainBudgets[item] {
			header, footer := fmt.Sprintf("\n<skill name=%q>\n", s.Name), "\n</skill>\n"
			body := aiContextExcerpt(s.Description+"\n"+s.Instructions, query, mainBudgets[item]-utf8.RuneCountInString(header+footer))
			if body == "" {
				continue
			}
			main = header + body + footer
			excerpted = true
		}
		index := len(usage)
		chosen, exists := selection[id]
		manual := exists && chosen != nil
		usage = append(usage, aiSkillUsage{ID: id, Name: s.Name, References: []string{}, Omitted: len(s.References), Auto: !manual, MainExcerpted: excerpted})
		out.WriteString(main)
		usage[index].Chars = utf8.RuneCountInString(main)
		wanted := map[string]bool{}
		for _, name := range chosen {
			wanted[name] = true
		}
		for _, p := range s.References {
			if manual && !wanted[p.Name] {
				continue
			}
			delete(wanted, p.Name)
			name := strings.ToLower(p.Name)
			explicit := strings.Contains(strings.ToLower(query), name) || strings.Contains(strings.ToLower(query), strings.ToLower(path.Base(name)))
			document := strings.HasPrefix(name, "references/") || strings.HasPrefix(name, "docs/") || strings.HasPrefix(name, "guides/") || (!strings.Contains(name, "/") && (strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".txt")))
			if !manual && !document && !explicit {
				continue
			} // scripts, tests and journals stay available for manual selection
			prompt := fmt.Sprintf("\n--- Skill reference: %s / %s ---\n%s\n", s.Name, p.Name, p.Text)
			text := strings.ToLower(p.Text)
			if len(text) > 64000 {
				text = text[:64000]
			}
			score := 0
			if strings.HasPrefix(name, "references/") {
				score += 15
			}
			if strings.Contains(strings.ToLower(s.Instructions), name) {
				score += 20
			}
			for _, term := range terms {
				if strings.Contains(name, term) {
					score += 10
				}
				if strings.Contains(text, term) {
					score++
				}
			}
			refs = append(refs, reference{owner: index, part: p, prompt: prompt, score: score, manual: manual})
		}
		if len(wanted) > 0 {
			return nil, nil, fmt.Errorf("技能「%s」的手选参考文件已变化，请重新选择", s.Name)
		}
	}
	chars += utf8.RuneCountInString(out.String())
	if chars > limit {
		return nil, nil, fmt.Errorf("技能主说明、附件和对话超过 %d 字，请减少启用技能或新建对话；已安装的技能不受影响", limit)
	}
	sort.SliceStable(refs, func(i, j int) bool {
		if refs[i].manual != refs[j].manual {
			return refs[i].manual
		}
		if refs[i].score != refs[j].score {
			return refs[i].score > refs[j].score
		}
		return refs[i].part.Name < refs[j].part.Name
	})
	for _, ref := range refs {
		n := utf8.RuneCountInString(ref.prompt)
		if chars+n > limit {
			if compact {
				if !ref.manual {
					continue
				}
				header := fmt.Sprintf("\n--- Skill reference excerpts: %s / %s ---\n", usage[ref.owner].Name, ref.part.Name)
				body := aiContextExcerpt(ref.part.Text, query, limit-chars-utf8.RuneCountInString(header))
				if body == "" {
					continue
				}
				ref.prompt = header + body
				n = utf8.RuneCountInString(ref.prompt)
				usage[ref.owner].ExcerptedReferences = append(usage[ref.owner].ExcerptedReferences, ref.part.Name)
			} else {
				if ref.manual {
					return nil, nil, fmt.Errorf("手选参考文件「%s」使本次文字超过 %d 字，请取消部分参考或新建对话", ref.part.Name, limit)
				}
				continue
			}
		}
		out.WriteString(ref.prompt)
		chars += n
		u := &usage[ref.owner]
		u.References = append(u.References, ref.part.Name)
		u.Omitted--
		u.Chars += n
	}
	if len(usage) == 0 {
		return messages, nil, nil
	}
	if len(messages) > 0 && messages[0]["role"] == "system" {
		c, _ := messages[0]["content"].(string)
		messages[0]["content"] = c + "\n\n" + out.String()
		return messages, usage, nil
	}
	return append([]map[string]any{{"role": "system", "content": out.String()}}, messages...), usage, nil
}
