package app

// live_e2e_test.go — LIVE end-to-end tests against REAL upstreams, using the
// repository-root config.yaml (or MODEL_PROXY_LIVE_CONFIG) and login-managed
// credentials (~/.model-proxy). These verify protocol conversion against real
// vendor dialects that mocks cannot cover (thinking dialects, vision gating,
// custom tools).
//
// ALL tests here are skipped unless MODEL_PROXY_LIVE=1 is set — a plain
// `go test ./...` stays hermetic. Run with:
//
//	MODEL_PROXY_LIVE=1 go test -run 'TestLive_' -count=1 -timeout 10m ./internal/app
//
// Cost note: every test makes real (tiny) paid calls — prompts are minimal.
// Sensitive data discipline: response bodies are NEVER logged in full;
// failures print at most 500 chars.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/catalog"
	"model-proxy/internal/observe/counters"
)

// liveTimeout caps one upstream round trip.
const liveTimeout = 60 * time.Second

// realHome is captured at package init — BEFORE the package TestMain
// redirects HOME into a temp dir (state_isolation_test.go). Live tests need
// the real ~/.model-proxy credentials, so they restore it.
var realHome, _ = os.UserHomeDir()

const liveConfigEnv = "MODEL_PROXY_LIVE_CONFIG"

// liveRepositoryRoot finds the checkout root from this source file. Falling
// back to the process working directory keeps the helper usable with builds
// that trim source paths, while walking upward makes it independent of the
// package working directory selected by `go test`.
func liveRepositoryRoot() (string, error) {
	var starts []string
	if _, source, _, ok := runtime.Caller(0); ok {
		starts = append(starts, filepath.Dir(source))
	}
	if wd, err := os.Getwd(); err == nil {
		starts = append(starts, wd)
	}
	for _, start := range starts {
		root, ok := findLiveModuleRoot(start)
		if ok {
			return root, nil
		}
	}
	return "", fmt.Errorf("cannot locate repository root containing go.mod")
}

func findLiveModuleRoot(start string) (string, bool) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", false
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !info.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// liveConfigPath resolves the live configuration independently of the test
// package's working directory. An absolute override is used verbatim; a
// relative override is interpreted from the repository root.
func liveConfigPath() (string, error) {
	override := os.Getenv(liveConfigEnv)
	if override != "" && filepath.IsAbs(override) {
		return filepath.Clean(override), nil
	}
	root, err := liveRepositoryRoot()
	if err != nil {
		return "", err
	}
	if override != "" {
		return filepath.Join(root, override), nil
	}
	return filepath.Join(root, "config.yaml"), nil
}

// liveUseRealHome restores login-managed credentials after package TestMain
// redirects HOME. Live tests are intentionally sequential because HOME is
// process-global.
func liveUseRealHome(t *testing.T) {
	t.Helper()
	if realHome == "" {
		t.Skip("live: cannot determine real home directory")
	}
	oldHome := os.Getenv("HOME")
	if oldHome == realHome {
		return
	}
	if err := os.Setenv("HOME", realHome); err != nil {
		t.Fatalf("live: restore HOME: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("HOME", oldHome) })
}

// liveConfig is the single entry point for live-test configuration. Once the
// live gate is explicitly enabled, a missing or unreadable configuration is a
// test failure rather than a skip: otherwise the whole E2E suite can go green
// without exercising an upstream.
func liveConfig(t *testing.T) *Config {
	t.Helper()
	if os.Getenv("MODEL_PROXY_LIVE") != "1" {
		t.Skip("live tests disabled (set MODEL_PROXY_LIVE=1)")
	}
	liveUseRealHome(t)
	path, err := liveConfigPath()
	if err != nil {
		t.Fatalf("live: resolve config path: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("live: load config %q: %v", path, err)
	}
	return cfg
}

// liveProxy builds a Proxy from the live config with Routes replaced
// by the given single-target routes (no failover noise, explicit protocol:
// for determinism — no wirecap verdict involved). Skips when the provider is
// absent or no credential can build its impl.
func liveProxy(t *testing.T, cfg *Config, routes map[string][]RouteTarget, needProviders ...string) (*httptest.Server, *Proxy) {
	t.Helper()
	liveUseRealHome(t)
	for _, name := range needProviders {
		if _, ok := cfg.Providers[name]; !ok {
			t.Skipf("live: provider %q not in config", name)
		}
	}
	cfg.Routes = routes
	p := NewProxyWithStatePath(cfg, t.TempDir()+"/state.json")
	t.Cleanup(p.Close)
	for _, name := range needProviders {
		if p.providers[name] == nil {
			t.Skipf("live: provider %q has no runtime impl (not logged in)", name)
		}
	}
	return httptest.NewServer(http.HandlerFunc(p.Handler)), p
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
	liveRequireResponsesCompleted(t, raw)
	return raw
}

// liveRequireResponsesCompleted rejects every non-clean Responses terminal.
// These live cases use generous output limits and are intended to prove a full
// round trip, so an incomplete/failed/error terminal is a regression rather
// than an acceptable HTTP-200 response.
func liveRequireResponsesCompleted(t *testing.T, raw string) []sseEvent {
	t.Helper()
	events := parseSSE(raw)
	if err := validateLiveResponsesCompleted(events); err != nil {
		t.Fatalf("live: %v:\n%s", err, liveExcerpt(raw))
	}
	return events
}

func validateLiveResponsesCompleted(events []sseEvent) error {
	completed := sseCount(events, "response.completed")
	incomplete := sseCount(events, "response.incomplete")
	failed := sseCount(events, "response.failed") + sseCount(events, "response.cancelled")
	if completed != 1 || incomplete != 0 || failed != 0 {
		return fmt.Errorf("Responses terminals completed/incomplete/failed = %d/%d/%d, want 1/0/0",
			completed, incomplete, failed)
	}
	for _, event := range events {
		eventType := sseEventType(event)
		if eventType == "error" {
			return fmt.Errorf("unexpected SSE error event")
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(event.data), &body); err != nil {
			if eventType == "response.completed" {
				return fmt.Errorf("malformed response.completed payload: %w", err)
			}
			continue
		}
		if body["error"] != nil {
			return fmt.Errorf("unexpected SSE error payload")
		}
		response := asMap(body["response"])
		if eventType == "response.completed" && response == nil {
			return fmt.Errorf("response.completed is missing nested response object")
		}
		if response == nil {
			continue
		}
		if response["error"] != nil {
			return fmt.Errorf("unexpected nested response.error payload")
		}
		if eventType == "response.completed" {
			status, present := response["status"]
			if !present {
				return fmt.Errorf("response.completed is missing nested response.status")
			}
			if strOpt(status) != "completed" {
				return fmt.Errorf("response.completed nested status = %q, want completed", strOpt(status))
			}
		}
	}
	return nil
}

// liveWeatherTool is the shared function-tool fixture (JSON in a Go string).
const liveWeatherTool = `{"type":"function","name":"get_weather","description":"Get current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`

// TestLive_ResponsesToChat_ToolRoundTrip: responses client → chat backend
// (r→chat). Turn 1 forces a tool call; turn 2 feeds the call + output back —
// this is the path that 400d on thinking-dialect upstreams before the
// placeholder reasoning_content fix (deepseek: "reasoning_content must be
// passed back").
func TestLive_ResponsesToChat_ToolRoundTrip(t *testing.T) {
	providers := []struct{ name, prefer string }{
		{"deepseek", "deepseek-v4-pro"},
		{"zhipu", "glm-4.7"},
		{"volcengine", "deepseek-v4-pro"},
		{"kimi-code", "k3"},
	}
	for _, tc := range providers {
		t.Run(tc.name, func(t *testing.T) {
			cfg := liveConfig(t)
			model := liveModel(t, cfg, tc.name, tc.prefer)
			srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
				"live-m": {{Provider: tc.name, Model: model, Protocol: "openai"}},
			}, tc.name)
			defer srv.Close()

			// Turn 1: force the tool call.
			turn1 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
				`"tools":[` + liveWeatherTool + `],` +
				`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."}]}]}`
			raw1 := liveResponsesTurn(t, srv, turn1)
			callID, callName, callArgs := liveExtractFunctionCall(t, raw1)
			liveRequireWeatherCall(t, callID, callName, callArgs, raw1)

			// Turn 2: feed the call + its output back (the placeholder-reasoning path).
			turn2 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
				`"tools":[` + liveWeatherTool + `],` +
				`"input":[` +
				`{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in Paris right now? You MUST call the get_weather tool to answer."}]},` +
				`{"type":"function_call","call_id":"` + callID + `","name":"` + callName + `","arguments":` + mustJSONStr(t, callArgs) + `},` +
				`{"type":"function_call_output","call_id":"` + callID + `","output":"sunny, 22°C"}]}`
			liveResponsesTurn(t, srv, turn2)
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
		// response.completed → response.Output items.
		if resp := asMap(m["response"]); resp != nil {
			items, _ := resp["output"].([]any)
			for _, it := range items {
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
	return
}

func liveRequireWeatherCall(t *testing.T, callID, name, arguments, raw string) {
	t.Helper()
	if err := validateLiveWeatherCall(callID, name, arguments); err != nil {
		t.Fatalf("live: %v:\n%s", err, liveExcerpt(raw))
	}
}

func validateLiveWeatherCall(callID, name, arguments string) error {
	if callID == "" {
		return fmt.Errorf("weather call id is empty")
	}
	if name == "" {
		return fmt.Errorf("weather call name is empty")
	}
	if arguments == "" {
		return fmt.Errorf("weather call arguments are empty")
	}
	if name != "get_weather" {
		return fmt.Errorf("tool name = %q, want get_weather", name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return fmt.Errorf("tool arguments are not a valid JSON object: %w", err)
	}
	city, _ := args["city"].(string)
	if city != "Paris" {
		return fmt.Errorf("tool city = %q, want Paris", city)
	}
	return nil
}

func liveExtractCustomToolCall(t *testing.T, raw string) (callID, name, input string) {
	t.Helper()
	var inputDelta strings.Builder
	for _, ev := range parseSSE(raw) {
		if ev.data == "[DONE]" {
			continue
		}
		m := unmarshalMap(t, []byte(ev.data))
		if item := asMap(m["item"]); item != nil && strOpt(item["type"]) == "custom_tool_call" {
			callID = firstNonEmpty(strOpt(item["call_id"]), strOpt(item["id"]), callID)
			name = firstNonEmpty(strOpt(item["name"]), name)
			if value := strOpt(item["input"]); value != "" {
				input = value
			}
		}
		switch sseEventType(ev) {
		case "response.custom_tool_call_input.delta":
			inputDelta.WriteString(strOpt(m["delta"]))
		case "response.custom_tool_call_input.done":
			if value := strOpt(m["input"]); value != "" {
				input = value
			}
		}
	}
	if input == "" {
		input = inputDelta.String()
	}
	return
}

// TestLive_ResponsesToChat_CustomTool: a custom/freeform tool wraps to
// {input: string} and unwraps back to custom_tool_call in the stream.
func TestLive_ResponsesToChat_CustomTool(t *testing.T) {
	cfg := liveConfig(t)
	model := liveModel(t, cfg, "deepseek", "deepseek-v4-pro")
	srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
		"live-m": {{Provider: "deepseek", Model: model, Protocol: "openai"}},
	}, "deepseek")
	defer srv.Close()

	turn1 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
		`"tools":[{"type":"custom","name":"apply_patch","description":"Apply a code patch to a file"}],` +
		`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Create hello.txt containing just the word hi. You MUST use the apply_patch tool."}]}]}`
	raw1 := liveResponsesTurn(t, srv, turn1)
	callID, callName, callInput := liveExtractCustomToolCall(t, raw1)
	if callID == "" || callName == "" || callInput == "" {
		t.Fatalf("live: incomplete custom call id/name/input = %q/%q/%q:\n%s",
			callID, callName, callInput, liveExcerpt(raw1))
	}
	if callName != "apply_patch" {
		t.Fatalf("live: custom tool name = %q, want apply_patch", callName)
	}

	// Turn 2: custom_tool_call + custom_tool_call_output in history.
	turn2 := `{"model":"live-m","stream":true,"max_output_tokens":512,` +
		`"tools":[{"type":"custom","name":"apply_patch","description":"Apply a code patch to a file"}],` +
		`"input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Create hello.txt containing just the word hi. You MUST use the apply_patch tool."}]},` +
		`{"type":"custom_tool_call","call_id":"` + callID + `","name":"` + callName + `","input":` + mustJSONStr(t, callInput) + `},` +
		`{"type":"custom_tool_call_output","call_id":"` + callID + `","output":"patch applied"}]}`
	liveResponsesTurn(t, srv, turn2)
}

// TestLive_AnthropicToChat_MediaVisionGate: a tool_result image in history
// must not 400 a text-only chat model (deepseek 400d "unknown variant
// image_url" before the vision gate) — the placeholder text rides instead.
func TestLive_AnthropicToChat_MediaVisionGate(t *testing.T) {
	cfg := liveConfig(t)
	model := liveModel(t, cfg, "deepseek", "deepseek-v4-pro")
	srv, p := liveProxy(t, cfg, map[string][]RouteTarget{
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
	// Parse the anthropic response and require a NON-EMPTY text block — a raw
	// `"text"` substring also matches error envelopes and media_type fields.
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("live: response is not an anthropic message JSON: %v\n%s", err, liveExcerpt(raw))
	}
	textLen := 0
	for _, c := range msg.Content {
		if c.Type == "text" {
			textLen += len(c.Text)
		}
	}
	if textLen == 0 {
		t.Fatalf("live: no non-empty text content block in response:\n%s", liveExcerpt(raw))
	}
}

// TestLive_AnthropicPassthrough: sanity control — anthropic client →
// anthropic backend (zhipu anthropic_base_url), byte-level passthrough works.
func TestLive_AnthropicPassthrough(t *testing.T) {
	cfg := liveConfig(t)
	prov, ok := cfg.Providers["zhipu"]
	if !ok || prov.AnthropicBaseURL == "" {
		t.Skip("live: zhipu has no anthropic_base_url")
	}
	model := liveModel(t, cfg, "zhipu", "glm-4.7")
	srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
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

// TestLive_AliasResponseModelNormalization is the live counterpart of
// TestForward_AliasResponseModelNormalizationE2E and pins the original
// client-resume regression ("Could not restore model model-proxy/k3"): on
// the kimi-code alias shape (client calls kimi-k3, upstream serves k3) the
// request leaves as k3 — proven by target-model commit metrics — while
// every client-facing response path carries the called model kimi-k3.
func TestLive_AliasResponseModelNormalization(t *testing.T) {
	t.Run("OpenAITarget", func(t *testing.T) {
		cfg := liveConfig(t)
		liveRequireKimiK3(t, cfg)
		srv, p := liveProxy(t, cfg, map[string][]RouteTarget{
			"kimi-k3": {{Provider: "kimi-code", Model: "k3", Protocol: "openai"}},
		}, "kimi-code")
		defer srv.Close()

		// Buffered chat: the response model echoes the CALLED name…
		status, raw := livePost(t, srv, "/v1/chat/completions",
			`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
		liveStatusOK(t, status, raw)
		var chat struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(raw), &chat); err != nil {
			t.Fatalf("live: response is not a chat completion JSON: %v\n%s", err, liveExcerpt(raw))
		}
		if chat.Model != "kimi-k3" {
			t.Fatalf("live: buffered chat model = %q, want called model kimi-k3:\n%s", chat.Model, liveExcerpt(raw))
		}
		// …while the upstream really was called as k3 (target-model commit
		// metrics). Without this, the assertions above would also pass if
		// kimi one day echoes kimi-k3 itself and would prove nothing about
		// normalization.
		awaitCommitMetrics(t, p, counters.PMKey{Provider: "kimi-code", Model: "k3"})

		// Streamed chat: every chunk's model is the called name. The check
		// is structural — content text may legitimately contain "k3".
		status, raw = livePost(t, srv, "/v1/chat/completions",
			`{"model":"kimi-k3","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
		liveStatusOK(t, status, raw)
		chunks := 0
		for _, ev := range parseSSE(raw) {
			if ev.data == "[DONE]" {
				continue
			}
			m := unmarshalMap(t, []byte(ev.data))
			if model := strOpt(m["model"]); model != "" {
				chunks++
				if model != "kimi-k3" {
					t.Fatalf("live: stream chunk model = %q, want kimi-k3:\n%s", model, liveExcerpt(raw))
				}
			}
		}
		if chunks == 0 {
			t.Fatalf("live: no chat chunk carried a model field:\n%s", liveExcerpt(raw))
		}

		// Anthropic ingress against the same openai target exercises the
		// conversion path: message_start's nested message.model.
		status, raw = livePost(t, srv, "/v1/messages",
			`{"model":"kimi-k3","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
		liveStatusOK(t, status, raw)
		liveRequireAnthropicStreamModel(t, raw, "kimi-k3")
	})

	t.Run("AnthropicPassthrough", func(t *testing.T) {
		cfg := liveConfig(t)
		liveRequireKimiK3(t, cfg)
		if cfg.Providers["kimi-code"].AnthropicBaseURL == "" {
			t.Skip("live: kimi-code has no anthropic_base_url")
		}
		srv, _ := liveProxy(t, cfg, map[string][]RouteTarget{
			"kimi-k3": {{Provider: "kimi-code", Model: "k3", Protocol: "anthropic"}},
		}, "kimi-code")
		defer srv.Close()

		// Buffered: top-level message model on the zero-copy path.
		status, raw := livePost(t, srv, "/v1/messages",
			`{"model":"kimi-k3","max_tokens":32,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
		liveStatusOK(t, status, raw)
		var msg struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("live: response is not an anthropic message JSON: %v\n%s", err, liveExcerpt(raw))
		}
		if msg.Model != "kimi-k3" {
			t.Fatalf("live: anthropic buffered model = %q, want kimi-k3:\n%s", msg.Model, liveExcerpt(raw))
		}

		// Streamed: the nested message_start message.model splice.
		status, raw = livePost(t, srv, "/v1/messages",
			`{"model":"kimi-k3","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"reply with exactly: pong"}]}`)
		liveStatusOK(t, status, raw)
		liveRequireAnthropicStreamModel(t, raw, "kimi-k3")
	})
}

// liveRequireKimiK3 skips unless the kimi-code provider still declares k3 —
// the alias scenario this suite pins no longer exists without it.
func liveRequireKimiK3(t *testing.T, cfg *Config) {
	t.Helper()
	prov, ok := cfg.Providers["kimi-code"]
	if !ok {
		t.Skip(`live: provider "kimi-code" not in config`)
	}
	for _, m := range prov.Models {
		if m == "k3" {
			return
		}
	}
	t.Skip("live: kimi-code no longer declares k3 — alias scenario gone")
}

// liveRequireAnthropicStreamModel requires exactly one message_start event
// whose nested message.model is want, and no event exposing any other model.
func liveRequireAnthropicStreamModel(t *testing.T, raw, want string) {
	t.Helper()
	starts := 0
	for _, ev := range parseSSE(raw) {
		m := unmarshalMap(t, []byte(ev.data))
		if model := strOpt(m["model"]); model != "" && model != want {
			t.Fatalf("live: anthropic event model = %q, want %s:\n%s", model, want, liveExcerpt(raw))
		}
		if sseEventType(ev) != "message_start" {
			continue
		}
		starts++
		message := asMap(m["message"])
		if message == nil {
			t.Fatalf("live: message_start without message object:\n%s", liveExcerpt(raw))
		}
		if model := strOpt(message["model"]); model != want {
			t.Fatalf("live: message_start message.model = %q, want %s:\n%s", model, want, liveExcerpt(raw))
		}
	}
	if starts != 1 {
		t.Fatalf("live: message_start events = %d, want 1:\n%s", starts, liveExcerpt(raw))
	}
}
