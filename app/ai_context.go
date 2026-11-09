package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

type aiContextUsage struct {
	HistoryTotal     int `json:"history_total"`
	HistoryUsed      int `json:"history_used"`
	HistoryOmitted   int `json:"history_omitted"`
	HistoryExcerpted int `json:"history_excerpted"`
	SkillExcerpts    int `json:"skill_excerpts"`
	SkippedSkills    int `json:"skipped_skills"`
	InputTokens      int `json:"input_tokens,omitempty"`
	ContextTokens    int `json:"context_tokens,omitempty"`
}

// Allocate small main instructions in full; divide the remainder between larger ones.
func aiShareContext(needs []int, budget int) []int {
	out := make([]int, len(needs))
	order := make([]int, len(needs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return needs[order[i]] < needs[order[j]] })
	for n, i := range order {
		out[i] = min(needs[i], max(0, budget/(len(order)-n)))
		budget -= out[i]
	}
	return out
}

// Explicit excerpt markers prevent a partial skill/reference from masquerading as a full file.
func aiContextExcerpt(text, query string, budget int) string {
	if budget <= 100 {
		return ""
	}
	r := []rune(text)
	if len(r) <= budget {
		return text
	}
	terms := aiSkillQueryTerms(query)
	type piece struct {
		start, end, score int
		text              string
	}
	parts := []piece{}
	chunk := min(500, max(80, budget/3))
	for start := 0; start < len(r); start += chunk {
		end := min(len(r), start+chunk)
		t := string(r[start:end])
		score := 0
		if start == 0 {
			score += 1000
		}
		lower := strings.ToLower(t)
		for _, word := range terms {
			if strings.Contains(lower, word) {
				score += 100
			}
		}
		parts = append(parts, piece{start: start, end: end, score: score, text: t})
	}
	sort.SliceStable(parts, func(i, j int) bool {
		if parts[i].score != parts[j].score {
			return parts[i].score > parts[j].score
		}
		return parts[i].start < parts[j].start
	})
	intro := "[Selected excerpts only; omitted text is not included.]\n"
	used := utf8.RuneCountInString(intro)
	chosen := []piece{}
	for _, p := range parts {
		p.text = fmt.Sprintf("[characters %d-%d]\n%s\n", p.start+1, p.end, p.text)
		n := utf8.RuneCountInString(p.text)
		if used+n <= budget {
			chosen = append(chosen, p)
			used += n
		}
	}
	if len(chosen) == 0 {
		return ""
	}
	sort.Slice(chosen, func(i, j int) bool { return chosen[i].start < chosen[j].start })
	var out strings.Builder
	out.WriteString(intro)
	for _, p := range chosen {
		out.WriteString(p.text)
	}
	return out.String()
}

// Pack the request without deleting or rewriting the conversation stored on this computer.
// Recent turns are retained; oversized replies must not evict the whole conversation.
func aiPrepareChatContext(messages []map[string]any, ids []string, selection map[string][]string, limits ...int) ([]map[string]any, []aiSkillUsage, aiContextUsage, error) {
	limit := aiTextLimit
	if len(limits) > 0 {
		limit = limits[0]
	}
	info := aiContextUsage{}
	last := -1
	system := ""
	attachmentIDs := []map[string]string{}
	seenIDs := map[string]bool{}
	for i, m := range messages {
		if m["role"] == "user" {
			last = i
		}
		if m["role"] == "system" {
			s, _ := m["content"].(string)
			if s != "" {
				if system != "" {
					system += "\n"
				}
				system += s
			}
		}
	}
	if last < 0 {
		return nil, nil, info, errors.New("请先输入一条消息或添加附件")
	}
	if last > 0 {
		if system != "" {
			system += "\n\n"
		}
		system += "Conversation continuity: use the user's earlier results and corrections. When troubleshooting, do not recommend an unchanged action the user already reported as failed. If retrying is necessary, explain the specific changed condition or diagnostic purpose; otherwise choose a different next check. Do not assume a suggested step was executed or succeeded. If a missing result is essential, ask for it rather than restarting the same instructions."
	}
	query, _ := messages[last]["content"].(string)
	// Reuse attachments for follow-up questions even when their old turns leave the request window.
	for i := last; i >= 0 && len(attachmentIDs) < 8; i-- {
		if messages[i]["role"] != "user" {
			continue
		}
		raw, ok := messages[i]["attachments"]
		if !ok {
			continue
		}
		refs, e := aiContextAttachmentIDs(raw)
		if e != nil {
			return nil, nil, info, e
		}
		for _, id := range refs {
			if !seenIDs[id] && len(attachmentIDs) < 8 {
				seenIDs[id] = true
				attachmentIDs = append(attachmentIDs, map[string]string{"id": id})
			}
		}
	}
	base := utf8.RuneCountInString(system + query)
	if base > limit-256 {
		return nil, nil, info, errors.New("当前这一条消息或系统提示词过长，请缩短本条文字；历史记录和完整附件仍保存在本机")
	}
	attachmentReserve := 0
	if len(attachmentIDs) > 0 {
		attachmentCap := 5000
		if limit > aiTextLimit {
			attachmentCap = limit / 3
		}
		attachmentReserve = min(attachmentCap, max(1000, (limit-base)/3))
	}
	historyReserve := 1000
	if last > 0 {
		historyReserve = min(max(6000, limit/3), max(1000, (limit-base-attachmentReserve)/2))
	}
	skillBudget := min(8000, max(0, limit-base-attachmentReserve-historyReserve))
	skillInput := []map[string]any{{"role": "user", "content": query}}
	skillMessages, skills, e := aiApplySkillBudget(skillInput, ids, selection, skillBudget+utf8.RuneCountInString(query)+2, true)
	if e != nil {
		return nil, nil, info, e
	}
	if len(skillMessages) > 1 && skillMessages[0]["role"] == "system" {
		s := skillMessages[0]["content"].(string)
		if system != "" {
			system += "\n\n"
		}
		system += s
	}
	info.SkippedSkills = len(ids) - len(skills)
	for _, s := range skills {
		if s.MainExcerpted {
			info.SkillExcerpts++
		}
	}
	base = utf8.RuneCountInString(system + query)
	const historyNote = "Some earlier messages are omitted or excerpted below, not a complete transcript. Use the available history to answer the latest user message; do not invent missing details or repeat completed steps unless asked."
	historyBudget := max(0, limit-base-attachmentReserve-utf8.RuneCountInString(historyNote)-4)
	history := []map[string]any{}
	starts := []int{}
	for i, m := range messages[:last] {
		if m["role"] != "system" {
			info.HistoryTotal++
		}
		if m["role"] == "user" {
			starts = append(starts, i)
		}
	}
	for n := len(starts) - 1; n >= 0; n-- {
		start, end := starts[n], last
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		cost := 0
		turn := []map[string]any{}
		for _, m := range messages[start:end] {
			if m["role"] == "system" {
				continue
			}
			s, _ := m["content"].(string)
			cost += utf8.RuneCountInString(s)
			copy := aiContextMessageCopy(m)
			turn = append(turn, copy)
		}
		if cost > historyBudget {
			// Leave room for older questions when a single code-heavy turn is huge.
			budget := historyBudget
			if n > 0 && budget >= 1024 {
				budget = budget * 3 / 4
			}
			if budget < 128*len(turn) {
				break
			}
			needs := make([]int, len(turn))
			for i, m := range turn {
				text, _ := m["content"].(string)
				needs[i] = utf8.RuneCountInString(text)
			}
			shares := aiShareContext(needs, budget)
			cost = 0
			for i, m := range turn {
				text, _ := m["content"].(string)
				if needs[i] > shares[i] {
					m["content"] = aiHistoryExcerpt(text, shares[i])
					info.HistoryExcerpted++
				}
				part, _ := m["content"].(string)
				cost += utf8.RuneCountInString(part)
			}
		}
		historyBudget -= cost
		history = append(turn, history...)
	}
	info.HistoryUsed = len(history)
	info.HistoryOmitted = max(0, info.HistoryTotal-info.HistoryUsed)
	if info.HistoryOmitted > 0 || info.HistoryExcerpted > 0 {
		if system != "" {
			system += "\n\n"
		}
		system += historyNote
	}
	out := []map[string]any{}
	if system != "" {
		out = append(out, map[string]any{"role": "system", "content": system})
	}
	out = append(out, history...)
	current := aiContextMessageCopy(messages[last])
	if len(attachmentIDs) > 0 {
		current["attachments"] = attachmentIDs
	}
	out = append(out, current)
	return out, skills, info, nil
}

// Keep the beginning and end verbatim: the original request and the reply's
// conclusion remain usable for follow-ups. This is an excerpt, not a summary.
func aiHistoryExcerpt(text string, budget int) string {
	r := []rune(text)
	if len(r) <= budget {
		return text
	}
	const marker = "\n[Earlier message excerpt: middle omitted; full text remains in local history.]\n"
	available := max(0, budget-utf8.RuneCountInString(marker))
	if available == 0 {
		return ""
	}
	head := (available + 1) / 2
	tail := available - head
	return string(r[:head]) + marker + string(r[len(r)-tail:])
}

func aiContextMessageCopy(m map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"role", "content", "images"} {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func aiContextAttachmentIDs(raw any) ([]string, error) {
	b, e := json.Marshal(raw)
	var refs []struct {
		ID string `json:"id"`
	}
	if e != nil || json.Unmarshal(b, &refs) != nil || len(refs) > 8 {
		return nil, errors.New("附件列表无效，每条消息最多 8 组附件")
	}
	ids := []string{}
	for _, ref := range refs {
		if _, e := aiAttachmentPath(ref.ID); e != nil {
			return nil, e
		}
		ids = append(ids, ref.ID)
	}
	return ids, nil
}
