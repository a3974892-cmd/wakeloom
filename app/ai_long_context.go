package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

const aiLongContext = 1000000

// Long requests are allowed only after the running VM confirms its dedicated
// YaRN runtime and allocated slot. A UI/config number alone is never sufficient.
func aiVerifyLongContext(r *http.Request, model string) error {
	resp, err := aiRequest(r, "GET", "info", nil)
	if err != nil {
		return fmt.Errorf("读取 1M 服务状态失败：%w", err)
	}
	defer resp.Body.Close()
	var info struct {
		Ready    bool `json:"ready"`
		Verified bool `json:"context_verified"`
		Tokens   int  `json:"context_tokens"`
		LLM      struct {
			Name   string `json:"name"`
			Engine string `json:"engine"`
		} `json:"llm"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info) != nil || !info.Ready || !info.Verified || info.Tokens != aiLongContext || info.LLM.Name != model || info.LLM.Engine != "llama_cpp_1m" {
		return errors.New("当前 GPU 尚未启用 1M 服务。请用「Qwen3.8 · 1M 实验」配置重新唤醒；网关模式需要更新网关包")
	}
	return nil
}

func aiCountLongContext(r *http.Request, model string, messages []map[string]any, think *bool) (int, int, error) {
	body, _ := json.Marshal(map[string]any{"model": model, "messages": messages, "think": think, "options": map[string]any{"num_ctx": aiLongContext}})
	resp, err := aiRequest(r, "POST", "api/context-check", body)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	var count struct {
		Tokens  int    `json:"tokens"`
		Budget  int    `json:"input_budget"`
		Context int    `json:"context_tokens"`
		Error   string `json:"error"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&count); err != nil {
		return 0, 0, errors.New("1M token 计数返回无效，未发送生成请求")
	}
	if resp.StatusCode != 200 {
		return 0, 0, fmt.Errorf("1M token 检查失败：%s", count.Error)
	}
	if count.Tokens <= 0 || count.Context != aiLongContext || count.Budget != aiLongContext-16384-4096 {
		return 0, 0, errors.New("1M token 预算未通过核对，未发送生成请求")
	}
	return count.Tokens, count.Budget, nil
}

func aiPrepareRequestContext(r *http.Request, model string, original []map[string]any, ids []string, selection map[string][]string, ctx int, think *bool) ([]map[string]any, []aiSkillUsage, aiContextUsage, bool, []aiAttachmentUsage, error) {
	limit := aiTextLimit
	var info aiContextUsage
	if ctx > 262144 {
		if ctx != aiLongContext {
			return nil, nil, info, false, nil, errors.New("长上下文请使用 1M 实验启动配置")
		}
		if err := aiVerifyLongContext(r, model); err != nil {
			return nil, nil, info, false, nil, err
		}
		// This is a packing ceiling, not an assumed token ratio. The VM tokenizer
		// measures the rendered template before any generation is allowed.
		limit = 4 * (aiLongContext - 16384 - 4096)
	}
	for attempt := 0; attempt < 5; attempt++ {
		messages, skills, usage, err := aiPrepareChatContext(original, ids, selection, limit)
		if err != nil {
			return nil, nil, usage, false, nil, err
		}
		images, attachments, err := aiExpandStoredAttachments(r.Context(), messages, 0, limit)
		if err != nil {
			return nil, nil, usage, false, nil, err
		}
		if ctx != aiLongContext {
			return messages, skills, usage, images, attachments, nil
		}
		if images {
			return nil, nil, usage, false, nil, errors.New("1M 实验配置目前仅支持文字、文档和程序静态报告；图片请使用原看图配置")
		}
		count, budget, err := aiCountLongContext(r, model, messages, think)
		if err != nil {
			return nil, nil, usage, false, nil, err
		}
		if count <= budget {
			usage.InputTokens, usage.ContextTokens = count, aiLongContext
			return messages, skills, usage, false, attachments, nil
		}
		limit = max(1024, int(int64(limit)*int64(budget)*90/int64(count)/100))
	}
	return nil, nil, info, false, nil, errors.New("整理后仍超出 1M 输入预算，请缩短最新消息或减少附件；完整聊天保留在本机")
}
