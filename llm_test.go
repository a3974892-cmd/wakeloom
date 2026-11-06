package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockLLM speaks all three wire formats and records the last request body.
func mockLLM(t *testing.T, last *map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		*last = m
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			if r.Header.Get("Authorization") != "Bearer k1" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"error":{"message":"bad key"}}`)
				return
			}
			for _, s := range []string{"integrated_", "multimodal_description: ", "[Shot 1] ok"} {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", s)
			}
			fmt.Fprint(w, "data: [DONE]\n\n")
		case r.URL.Path == "/v1/messages":
			if r.Header.Get("x-api-key") != "k2" {
				w.WriteHeader(401)
				fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
				return
			}
			fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			for _, s := range []string{"Hello", " Claude"} {
				fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", s)
			}
			fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		case strings.Contains(r.URL.Path, ":streamGenerateContent"):
			fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"thinking\",\"thought\":true},{\"text\":\"Hi\"}]}}]}\n\n")
			fmt.Fprint(w, "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\" Gemini\"}]}}]}\n\n")
		case r.URL.Path == "/v1/models", r.URL.Path == "/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"b-model"},{"id":"a-model"}]}`)
		case r.URL.Path == "/v1beta/models":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"models":[{"name":"models/gem-x","supportedGenerationMethods":["generateContent"]},{"name":"models/embed","supportedGenerationMethods":["embedContent"]}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestStreamAllKinds(t *testing.T) {
	var last map[string]any
	srv := mockLLM(t, &last)
	defer srv.Close()
	req := llmRequest{System: "sys", User: "u", Images: []llmImage{{Mime: "image/jpeg", B64: "QUJD"}}}
	cases := []struct {
		p    LLMProvider
		want string
	}{
		{LLMProvider{Kind: "openai", BaseURL: srv.URL, APIKey: "k1", Model: "m"}, "integrated_multimodal_description: [Shot 1] ok"},
		{LLMProvider{Kind: "anthropic", BaseURL: srv.URL, APIKey: "k2", Model: "m"}, "Hello Claude"},
		{LLMProvider{Kind: "gemini", BaseURL: srv.URL, APIKey: "k3", Model: "models/gem"}, "Hi Gemini"},
	}
	for _, c := range cases {
		var out strings.Builder
		if err := streamLLM(context.Background(), c.p, req, 100, func(s string) { out.WriteString(s) }); err != nil {
			t.Fatalf("%s: %v", c.p.Kind, err)
		}
		if out.String() != c.want {
			t.Fatalf("%s: got %q", c.p.Kind, out.String())
		}
		js, _ := json.Marshal(last)
		if !strings.Contains(string(js), "QUJD") {
			t.Fatalf("%s: image not sent: %s", c.p.Kind, js)
		}
	}
	// auth errors surface the provider's message
	err := streamLLM(context.Background(), LLMProvider{Kind: "anthropic", BaseURL: srv.URL, APIKey: "bad", Model: "m"}, req, 10, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "invalid x-api-key") {
		t.Fatalf("want auth error, got %v", err)
	}
	ms, err := listModels(context.Background(), LLMProvider{Kind: "gemini", BaseURL: srv.URL})
	if err != nil || strings.Join(ms, ",") != "gem-x" {
		t.Fatalf("gemini models %v %v", ms, err)
	}
	ms, _ = listModels(context.Background(), LLMProvider{Kind: "openai", BaseURL: srv.URL})
	if strings.Join(ms, ",") != "a-model,b-model" {
		t.Fatalf("openai models %v", ms)
	}
}

func TestBuildPromptRequest(t *testing.T) {
	r := buildPromptRequest("FL2VA", 8, "女孩撑伞", "", "", 2)
	if !strings.Contains(r.System, "integrated_multimodal_description") || !strings.Contains(r.User, "8.00") || !strings.Contains(r.User, "2 reference picture") {
		t.Fatalf("bad request: %s", r.User)
	}
	e := buildPromptRequest("T2VA", 30, "", "OLD PROMPT", "镜头推近", 0)
	if !strings.Contains(e.User, "OLD PROMPT") || !strings.Contains(e.User, "15 seconds") {
		t.Fatalf("bad edit request: %s", e.User)
	}
}
