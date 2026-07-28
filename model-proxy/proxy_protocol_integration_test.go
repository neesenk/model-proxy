package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// --- UC1: anthropic request translated via claude_mapping + forwarded with /v1 kept ---

func TestUC_AnthropicMappingAndPathKept(t *testing.T) {
	var hitPath string
	var hitModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		json.Unmarshal(b, &m)
		hitModel = m.Model
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "aqp"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
		ClaudeMapping: map[string]string{"claude-opus-4-8": "glm-5.2"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"aqp": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/messages", `{"model":"claude-opus-4-8","messages":[]}`)

	if hitModel != "glm-5.2" {
		t.Errorf("upstream model=%q want glm-5.2 (claude_mapping should translate)", hitModel)
	}
	if hitPath != "/v1/messages" {
		t.Errorf("upstream path=%q want /v1/messages (anthropic keeps /v1)", hitPath)
	}
}

// --- UC2: openai request strips the client /v1 prefix ---

func TestUC_OpenAIStripsV1Prefix(t *testing.T) {
	var hitPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/chat/completions", `{"model":"gpt-5.5","messages":[]}`)

	if hitPath != "/chat/completions" {
		t.Errorf("upstream path=%q want /chat/completions (openai strips /v1)", hitPath)
	}
}
