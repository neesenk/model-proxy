package main

// live_e2e_test.go — LIVE end-to-end tests against REAL upstreams, using the
// real ./config.yaml and login-managed credentials (~/.model-proxy). These
// verify protocol conversion against real vendor dialects that mocks cannot
// cover (thinking dialects, vision gating, custom tools).
//
// ALL tests here are skipped unless MODEL_PROXY_LIVE=1 is set — a plain
// `go test ./...` stays hermetic. Run with:
//
//	MODEL_PROXY_LIVE=1 go test -run TestLive -count=1 -timeout 10m .
//
// Cost note: every test makes real (tiny) paid calls — prompts are minimal.
// Sensitive data discipline: response bodies are NEVER logged in full;
// failures print at most 500 chars.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/catalog"
)

// liveTimeout caps one upstream round trip.
const liveTimeout = 60 * time.Second

// realHome is captured at package init — BEFORE the package TestMain
// redirects HOME into a temp dir (state_isolation_test.go). Live tests need
// the real ~/.model-proxy credentials, so they restore it.
var realHome, _ = os.UserHomeDir()

// liveProxy builds a Proxy from the REAL ./config.yaml with Routes replaced
// by the given single-target routes (no failover noise, explicit protocol:
// for determinism — no wirecap verdict involved). Skips when the config is
// missing, the provider is absent, or no credential can build its impl.
func liveProxy(t *testing.T, routes map[string][]RouteTarget, needProviders ...string) (*httptest.Server, *Proxy) {
	t.Helper()
	if realHome == "" {
		t.Skip("live: cannot determine real home directory")
	}
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", realHome)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })
	cfg, err := LoadConfig(configPath(nil))
	if err != nil {
		t.Skipf("live: cannot load ./config.yaml: %v", err)
	}
	for _, name := range needProviders {
		if _, ok := cfg.Providers[name]; !ok {
			t.Skipf("live: provider %q not in config", name)
		}
	}
	cfg.Routes = routes
	p := newProxyWithStatePath(cfg, t.TempDir()+"/state.json")
	t.Cleanup(p.Close)
	for _, name := range needProviders {
		if p.providers[name] == nil {
			t.Skipf("live: provider %q has no runtime impl (not logged in)", name)
		}
	}
	return httptest.NewServer(http.HandlerFunc(p.handler)), p
}

// liveModel picks a model for a provider: preferred if the provider serves
// it, else the LAST entry in provider.Models (newer entries last — zhipu's
// resource-pack-restricted older models 429/1113 on chat), else skip.
func liveModel(t *testing.T, cfg *Config, provider, preferred string) string {
	t.Helper()
	models := cfg.Providers[provider].Models
	for _, m := range models {
		if m == preferred {
			return m
		}
	}
	if len(models) > 0 {
		return models[len(models)-1]
	}
	t.Skipf("live: provider %q declares no models", provider)
	return ""
}

// livePost posts a JSON body to the proxy and reads the full response
// (stream included). (status, body)
func livePost(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: liveTimeout}
	resp, err := client.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("live: POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("live: read body: %v", err)
	}
	return resp.StatusCode, string(b)
}

// liveExcerpt caps failure output (never dump full bodies).
func liveExcerpt(s string) string {
	if len(s) > 500 {
		return s[:500] + "…"
	}
	return s
}

// liveStatusOK passes expected statuses, SKIPS on upstream-quota signals
// (429, or zhipu's resource-pack 1113) — those are account/resource issues,
// not conversion defects.
func liveStatusOK(t *testing.T, status int, body string) {
	t.Helper()
	if status == 429 || strings.Contains(body, `"code":"1113"`) || strings.Contains(body, `"code":1113`) {
		t.Skipf("live: upstream quota/resource limit (status %d) — not a conversion defect", status)
	}
	if status != http.StatusOK {
		t.Fatalf("live: status %d: %s", status, liveExcerpt(body))
	}
}

// liveResponsesTurn is one /responses turn (stream) returning the raw SSE text.
func liveResponsesTurn(t *testing.T, srv *httptest.Server, body string) string {
	t.Helper()
	status, raw := livePost(t, srv, "/v1/responses", body)
	liveStatusOK(t, status, raw)
	return raw
}

// liveWeatherTool is the shared function-tool fixture (JSON in a Go string).
const liveWeatherTool = `{"type":"function","name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`

// TestLive_ResponsesToChat_ToolRoundTrip: responses client → chat backend
// (r→chat). Turn 1 forces a tool call; turn 2 feeds the call + output back —
// this is the path that 400d on thinking-dialect upstreams before the
// placeholder reasoning_content fix (deepseek: "reasoning_content must be
// passed back").
func TestLive_ResponsesToChat_ToolRoundTrip(t *testing.T) {
	if os.Getenv("MODEL_PROXY_LIVE") == "" {
		t.Skip("live tests disabled (set MODEL_PROXY_LIVE=1)")
	}
	providers := []struct{ name, prefer string }{
		{"deepseek", "deepseek-v4-pro"},
		{"zhipu", "glm-4.7"},
		{"volcengine", "deepseek-v4-pro"},
		{"kimi-code", "k3"},
	}
	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadConfig(configPath(nil))
			if err != nil {
				t.Skipf("live: %v", err)
			}
			model := liveModel(t, cfg, tc.name, tc.prefer)
			srv, _ := liveProxy(t, map[string][]RouteTarget{
				"live-m": {{Provider: tc.name, Model: model, Protocol: "openai"}},
			}, tc.name)
			defer srv.Close()

			// Turn 1: force the tool call.
			turn1 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
				`"tools":[` + liveWeatherTool + `],` +
				`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."}]}]}`
			raw1 := liveResponsesTurn(t, srv, turn1)
			callID, callName, callArgs := liveExtractFunctionCall(t, raw1)
			if callName == "" {
				t.Fatalf("live: no function_call in turn 1 stream:\n%s", liveExcerpt(raw1))
			}

			// Turn 2: feed the call + its output back (the placeholder-reasoning path).
			turn2 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
				`"tools":[` + liveWeatherTool + `],` +
				`"input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."}]},` +
				`{"type":"function_call","call_id":"` + callID + `","name":"` + callName + `","arguments":` + mustJSONStr(t, callArgs) + `},` +
				`{"type":"function_call_output","call_id":"` + callID + `","output":"sunny, 22°C"}]}`
			raw2 := liveResponsesTurn(t, srv, turn2)
			if !strings.Contains(raw2, "response.completed") && !strings.Contains(raw2, "response.incomplete") {
				t.Fatalf("live: turn 2 has no terminal event:\n%s", liveExcerpt(raw2))
			}
		})
	}
}

// liveExtractFunctionCall finds the first function_call in a responses SSE
// stream (done frames preferred — they carry full arguments).
func liveExtractFunctionCall(t *testing.T, raw string) (callID, name, arguments string) {
	t.Helper()
	events := parseSSE(raw)
	for _, ev := range events {
		if ev.data == "[DONE]" {
			continue
		}
		m := unmarshalMap(t, []byte(ev.data))
		// output_item.done with a function_call item.
		if item := asMap(m["item"]); item != nil && strOpt(item["type"]) == "function_call" {
			callID = firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]), callID)
			name = firstNonEmpty(strOpt(item["name"]), name)
			if a := strOpt(item["arguments"]); a != "" {
				arguments = a
			}
		}
		// response.completed → response.output items.
		if resp := asMap(m["response"]); resp != nil {
			for _, it := range resp["output"].([]any) {
				item := asMap(it)
				if strOpt(item["type"]) == "function_call" {
					callID = firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]), callID)
					name = firstNonEmpty(strOpt(item["name"]), name)
					if a := strOpt(item["arguments"]); a != "" {
						arguments = a
					}
				}
			}
		}
	}
	if arguments == "" {
		arguments = `{"city":"Paris"}`
	}
	return
}

// TestLive_ResponsesToChat_CustomTool: a custom/freeform tool wraps to
// {input: string} and unwraps back to custom_tool_call in the stream.
func TestLive_ResponsesToChat_CustomTool(t *testing.T) {
	if os.Getenv("MODEL_PROXY_LIVE") == "" {
		t.Skip("live tests disabled (set MODEL_PROXY_LIVE=1)")
	}
	cfg, err := LoadConfig(configPath(nil))
	if err != nil {
		t.Skipf("live: %v", err)
	}
	model := liveModel(t, cfg, "deepseek", "deepseek-v4-pro")
	srv, _ := liveProxy(t, map[string][]RouteTarget{
		"live-m": {{Provider: "deepseek", Model: model, Protocol: "openai"}},
	}, "deepseek")
	defer srv.Close()

	turn1 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
		`"tools":[{"type":"custom","name":"apply_patch","description":"Apply a code patch to a file"}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Create hello.txt containing just the word hi. You MUST use the apply_patch tool."}]}]}`
	raw1 := liveResponsesTurn(t, srv, turn1)
	if !strings.Contains(raw1, "custom_tool_call") {
		t.Fatalf("live: no custom_tool_call in stream:\n%s", liveExcerpt(raw1))
	}
	callID := ""
	for _, ev := range parseSSE(raw1) {
		if ev.data == "[DONE]" {
			continue
		}
		m := unmarshalMap(t, []byte(ev.data))
		if item := asMap(m["item"]); item != nil && strOpt(item["type"]) == "custom_tool_call" {
			callID = firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]))
			break
		}
	}

	// Turn 2: custom_tool_call + custom_tool_call_output in history.
	turn2 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
		`"tools":[{"type":"custom","name":"apply_patch","description":"Apply a code patch to a file"}],` +
		`"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Create hello.txt containing just the word hi. You MUST use the apply_patch tool."}]},` +
		`{"type":"custom_tool_call","call_id":"` + callID + `","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch"},` +
		`{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"patch applied"}]}`
	raw2 := liveResponsesTurn(t, srv, turn2)
	if !strings.Contains(raw2, "response.completed") && !strings.Contains(raw2, "response.incomplete") {
		t.Fatalf("live: turn 2 has no terminal event:\n%s", liveExcerpt(raw2))
	}
}

// TestLive_AnthropicToChat_MediaVisionGate: a tool_result image in history
// must not 400 a text-only chat model (deepseek 400d "unknown variant
// image_url" before the vision gate) — the placeholder text rides instead.
func TestLive_AnthropicToChat_MediaVisionGate(t *testing.T) {
	if os.Getenv("MODEL_PROXY_LIVE") == "" {
		t.Skip("live tests disabled (set MODEL_PROXY_LIVE=1)")
	}
	cfg, err := LoadConfig(configPath(nil))
	if err != nil {
		t.Skipf("live: %v", err)
	}
	model := liveModel(t, cfg, "deepseek", "deepseek-v4-pro")
	srv, p := liveProxy(t, map[string][]RouteTarget{
		"live-m": {{Provider: "deepseek", Model: model, Protocol: "openai"}},
	}, "deepseek")
	defer srv.Close()
	// Synthetic text-only catalog for the target model. We do NOT use the real
	// models.dev catalog here: request-aware routing would see the image in
	// the history and reroute away from the (text-only) target — defeating the
	// point of testing the conversion gate. With a catalog whose every model
	// is text-only, the cross-route fallback finds no vision candidate and
	// falls back to the original target (intentional-behaviors.md #9), so the
	// request still reaches deepseek with the image gated to placeholder text.
	p.catalog = catalog.New(map[string]catalog.Model{
		model: {Modalities: catalog.Modalities{Input: []string{"text"}}},
	})

	// 1x1 transparent PNG (fixture, not a real screenshot).
	const px = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	body := `{"model":"live-m","max_tokens":512,"messages":[` +
		`{"role":"user","content":"what is in this screenshot?"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"screenshot","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + px + `"}}]}]},` +
		`{"role":"user","content":"describe it briefly"}]}`
	status, raw := livePost(t, srv, "/v1/messages", body)
	liveStatusOK(t, status, raw)
	if !strings.Contains(raw, `"text"`) {
		t.Fatalf("live: no text content in response:\n%s", liveExcerpt(raw))
	}
}

// TestLive_AnthropicPassthrough: sanity control — anthropic client →
// anthropic backend (zhipu anthropic_base_url), byte-level passthrough works.
func TestLive_AnthropicPassthrough(t *testing.T) {
	if os.Getenv("MODEL_PROXY_LIVE") == "" {
		t.Skip("live tests disabled (set MODEL_PROXY_LIVE=1)")
	}
	cfg, err := LoadConfig(configPath(nil))
	if err != nil {
		t.Skipf("live: %v", err)
	}
	prov, ok := cfg.Providers["zhipu"]
	if !ok || prov.AnthropicBaseURL == "" {
		t.Skip("live: zhipu has no anthropic_base_url")
	}
	model := liveModel(t, cfg, "zhipu", "glm-4.7")
	srv, _ := liveProxy(t, map[string][]RouteTarget{
		"live-m": {{Provider: "zhipu", Model: model, Protocol: "anthropic"}},
	}, "zhipu")
	defer srv.Close()

	body := `{"model":"live-m","max_tokens":64,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`
	status, raw := livePost(t, srv, "/v1/messages", body)
	liveStatusOK(t, status, raw)
	if !strings.Contains(raw, `"type":"message"`) {
		t.Fatalf("live: not an anthropic message response:\n%s", liveExcerpt(raw))
	}
}
