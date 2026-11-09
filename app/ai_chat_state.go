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
	"sync"
)

type aiChatState struct {
	Conversations   json.RawMessage     `json:"conversations"`
	Current         string              `json:"current"`
	Skills          []string            `json:"skills"`
	SkillReferences map[string][]string `json:"skill_references,omitempty"`
	Exists          bool                `json:"exists"`
}

var aiChatStateMu sync.Mutex

const aiChatStateMaxBytes = 128 << 20 // 1M conversations can exceed browser/local 8 MB storage.

func aiChatStatePath(profile string) (string, error) {
	if profile == "" || len(profile) > 200 {
		return "", errors.New("模型配置编号无效")
	}
	h := sha256.Sum256([]byte(profile))
	return filepath.Join(dataDir, "ai-chats", hex.EncodeToString(h[:16])+".json"), nil
}

func readAIChatState(p string) (aiChatState, error) {
	s := aiChatState{Conversations: json.RawMessage("[]"), Skills: []string{}}
	f, e := os.Open(p)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	defer f.Close()
	e = json.NewDecoder(io.LimitReader(f, aiChatStateMaxBytes)).Decode(&s)
	return s, e
}

func aiChatStateHandler(w http.ResponseWriter, r *http.Request) {
	p, err := aiChatStatePath(r.PathValue("profile"))
	if err != nil {
		errOut(w, 400, err)
		return
	}
	aiChatStateMu.Lock()
	defer aiChatStateMu.Unlock()
	state, err := readAIChatState(p)
	if err != nil {
		errOut(w, 500, errors.New("本机对话记录读取失败，请先备份数据目录再重试"))
		return
	}
	if r.Method == "GET" {
		jsonOut(w, 200, state)
		return
	}
	var in struct {
		Conversations   json.RawMessage      `json:"conversations"`
		Current         *string              `json:"current"`
		Skills          *[]string            `json:"skills"`
		SkillReferences *map[string][]string `json:"skill_references"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, aiChatStateMaxBytes)
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		errOut(w, 400, errors.New("对话记录过大或格式无效"))
		return
	}
	if len(in.Conversations) > 0 {
		var list []json.RawMessage
		if json.Unmarshal(in.Conversations, &list) != nil || len(list) > 60 {
			errOut(w, 400, errors.New("对话列表格式无效，最多保存 60 个对话"))
			return
		}
		state.Conversations = in.Conversations
	}
	if in.Current != nil {
		state.Current = *in.Current
	}
	if in.Skills != nil {
		if len(*in.Skills) > 8 {
			errOut(w, 400, errors.New("每次最多启用 8 个技能"))
			return
		}
		state.Skills = *in.Skills
	}
	state.Exists = true
	if in.SkillReferences != nil {
		if len(*in.SkillReferences) > 256 {
			errOut(w, 400, errors.New("技能参考配置过多"))
			return
		}
		state.SkillReferences = *in.SkillReferences
	}
	// Keep the written representation within the same limit used by the reader.
	raw, marshalErr := json.Marshal(state)
	if marshalErr != nil {
		errOut(w, 500, marshalErr)
		return
	}
	if len(raw) > aiChatStateMaxBytes {
		errOut(w, 400, errors.New("对话记录超过 128 MB，请先导出重要内容并减少旧对话"))
		return
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err == nil {
		err = os.WriteFile(p+".tmp", raw, 0600)
		if err == nil {
			err = os.Rename(p+".tmp", p)
		}
	}
	if err != nil {
		errOut(w, 500, err)
		return
	}
	jsonOut(w, 200, map[string]bool{"ok": true})
}
