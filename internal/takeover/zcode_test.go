package takeover_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// zcodeFixture builds the config/meta/routes for one exposed reasoning model.
func zcodeFixture() (*configdomain.Config, map[string]map[string]catalog.Model, map[string][]configdomain.RouteTarget) {
	cfg := cfgWith(map[string]configdomain.Provider{
		"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
	}, nil)
	meta := map[string]map[string]catalog.Model{
		"zhipu": {"glm-5.3": {
			Reasoning:        true,
			ReasoningEfforts: []string{"low", "high", "max"},
			ToolCall:         true,
			Context:          1000000,
			Output:           128000,
			Modalities:       catalog.Modalities{Input: []string{"text", "image"}},
		}},
	}
	routes := map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
	return cfg, meta, routes
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

func providerRules(t *testing.T, v map[string]any) []any {
	t.Helper()
	rules, _ := v["config"].(map[string]any)["providerConfigRules"].(map[string]any)["providerRules"].([]any)
	return rules
}

func modelRules(t *testing.T, v map[string]any) []any {
	t.Helper()
	rules, _ := v["config"].(map[string]any)["modelConfigRules"].(map[string]any)["providerModelRules"].([]any)
	return rules
}

// TestZcodePresetRendering: the zcode preset renders a personal provider rule
// (api type derived from the variant protocol, bare baseUrl for anthropic),
// one model rule per exposed model carrying the models.dev metadata, and the
// default model selection pointing at the primary model — while preserving
// the user's own personal providers and manual model rules.
func TestZcodePresetRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".zcode", "v2", "provider_config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed with the user's own personal provider + a manual model rule.
	seed := `{"schemaVersion":1,"config":{"providerConfigRules":{"providerRules":[{"providerId":"mine","providerName":"mine","enabled":true,"config":{"group":"standard-personal","access":{"type":"api-key","apiKey":"x"},"api":{"type":"anthropic-messages","baseUrl":"https://mine.example"},"personalModelIds":["m1"],"modelOrder":["m1"],"visibility":"visible"}}]},"modelConfigRules":{"providerModelRules":[],"manualProviderModelRules":[{"providerId":"mine","modelId":"m1","config":{"enabled":false}}]}}}`
	if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("zcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := zcodeFixture()
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	v := readJSONFile(t, target)
	if v["schemaVersion"].(float64) != 1 {
		t.Fatalf("schemaVersion = %v", v["schemaVersion"])
	}

	rules := providerRules(t, v)
	if len(rules) != 2 {
		t.Fatalf("providerRules = %d, want user rule + ours: %v", len(rules), rules)
	}
	var ours map[string]any
	for _, r := range rules {
		m := r.(map[string]any)
		if m["providerId"] == "model-proxy" {
			ours = m
		}
	}
	if ours == nil {
		t.Fatalf("our provider rule missing: %v", rules)
	}
	oc := ours["config"].(map[string]any)
	api := oc["api"].(map[string]any)
	if api["type"] != "anthropic-messages" || api["baseUrl"] != "http://127.0.0.1:15721" {
		t.Fatalf("api = %v (anthropic variant: anthropic-messages + bare url)", api)
	}
	if oc["access"].(map[string]any)["apiKey"] != "PROXY_MANAGED" {
		t.Fatalf("access = %v", oc["access"])
	}
	ids, _ := oc["personalModelIds"].([]any)
	if len(ids) != 1 || ids[0] != "glm-5.3" {
		t.Fatalf("personalModelIds = %v", ids)
	}

	mrules := modelRules(t, v)
	if len(mrules) != 1 {
		t.Fatalf("providerModelRules = %v", mrules)
	}
	mr := mrules[0].(map[string]any)
	if mr["providerId"] != "model-proxy" || mr["modelId"] != "glm-5.3" {
		t.Fatalf("model rule = %v", mr)
	}
	mrc := mr["config"].(map[string]any)
	props := mrc["properties"].(map[string]any)
	if props["contextWindow"].(float64) != 1000000 || props["supportsToolCall"] != true {
		t.Fatalf("properties = %v", props)
	}
	if props["inputFormat"].(map[string]any)["supportsImage"] != true {
		t.Fatalf("inputFormat = %v", props["inputFormat"])
	}
	specs := mrc["optionSpecs"].(map[string]any)
	if specs["maxOutputTokens"].(map[string]any)["max"].(float64) != 128000 {
		t.Fatalf("optionSpecs = %v", specs)
	}
	values, _ := specs["reasoningLevel"].(map[string]any)["values"].([]any)
	if len(values) != 3 || values[2] != "max" {
		t.Fatalf("reasoningLevel.values = %v", values)
	}

	sel := v["config"].(map[string]any)["defaultModelSelection"].(map[string]any)
	if sel["providerId"] != "model-proxy" || sel["modelId"] != "glm-5.3" {
		t.Fatalf("defaultModelSelection = %v", sel)
	}
	if sel["options"].(map[string]any)["reasoningLevel"] != "max" {
		t.Fatalf("default reasoning level = %v (want highest effort)", sel["options"])
	}

	// The manual rule collection survived untouched.
	manual, _ := v["config"].(map[string]any)["modelConfigRules"].(map[string]any)["manualProviderModelRules"].([]any)
	if len(manual) != 1 {
		t.Fatalf("manualProviderModelRules dropped: %v", manual)
	}

	// Idempotent: re-render produces identical bytes (no duplicate entries).
	before, _ := os.ReadFile(target)
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(target)
	if string(before) != string(after) {
		t.Fatalf("re-render changed the file:\n%s\n---\n%s", before, after)
	}
}

// TestZcodeModelShrink: a re-takeover with fewer exposed models removes the
// stale model rules of OUR provider while leaving user rules alone.
func TestZcodeModelShrink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".zcode", "v2", "provider_config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("zcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := zcodeFixture()
	routes["glm-5.3-flash"] = []configdomain.RouteTarget{{Provider: "zhipu", Model: "glm-5.3", Priority: 1}}
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	if n := len(modelRules(t, readJSONFile(t, target))); n != 2 {
		t.Fatalf("initial render: %d model rules, want 2", n)
	}
	// Shrink back to one model: the dropped model's rule must disappear.
	if err := tmpl.Rewrite(cfg, meta, zcodeRoutes1()); err != nil {
		t.Fatal(err)
	}
	mrules := modelRules(t, readJSONFile(t, target))
	if len(mrules) != 1 || mrules[0].(map[string]any)["modelId"] != "glm-5.3" {
		t.Fatalf("stale model rule survived shrink: %v", mrules)
	}
	rules := providerRules(t, readJSONFile(t, target))
	oc := rules[0].(map[string]any)["config"].(map[string]any)
	if ids, _ := oc["personalModelIds"].([]any); len(ids) != 1 {
		t.Fatalf("personalModelIds not shrunk: %v", ids)
	}
}

func zcodeRoutes1() map[string][]configdomain.RouteTarget {
	return map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
}

// TestZcodeVariantsShareFile: two protocol variants (split mode) upsert
// distinct provider ids into the same provider_config.json — each with its
// own api type and model rules, neither clobbering the other.
func TestZcodeVariantsShareFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".zcode", "v2", "provider_config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := zcodeFixture()
	anthropic, err := takeover.TemplateByName("zcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := anthropic.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	openai, err := takeover.TemplateByName("zcode-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := openai.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	v := readJSONFile(t, target)
	apis := map[string]string{}
	for _, r := range providerRules(t, v) {
		m := r.(map[string]any)
		apis[m["providerId"].(string)] = m["config"].(map[string]any)["api"].(map[string]any)["type"].(string)
	}
	if apis["model-proxy"] != "anthropic-messages" || apis["model-proxy-openai"] != "openai-chat-completions" {
		t.Fatalf("provider apis = %v", apis)
	}
	openaiRule := ""
	for _, r := range providerRules(t, v) {
		m := r.(map[string]any)
		if m["providerId"] == "model-proxy-openai" {
			openaiRule = m["config"].(map[string]any)["api"].(map[string]any)["baseUrl"].(string)
		}
	}
	if openaiRule != "http://127.0.0.1:15721/v1" {
		t.Fatalf("openai baseUrl = %q (want /v1)", openaiRule)
	}
	if n := len(modelRules(t, v)); n != 2 {
		t.Fatalf("model rules = %d, want one per provider", n)
	}
}

// TestZcodeDriftPointer: doctor's drift probe navigates the rule ARRAY by
// providerId (no positional index) and compares the live baseUrl against the
// value the template would write.
func TestZcodeDriftPointer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".zcode", "v2", "provider_config.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("zcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := zcodeFixture()
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	current, expected := tmpl.Pointer(cfg)
	if expected != "http://127.0.0.1:15721" {
		t.Fatalf("expected = %q", expected)
	}
	if current != expected {
		t.Fatalf("current = %q right after takeover, want %q (no drift)", current, expected)
	}
	// Simulate a client-side rewrite (user edited the endpoint in the UI).
	v := readJSONFile(t, target)
	for _, r := range providerRules(t, v) {
		m := r.(map[string]any)
		if m["providerId"] == "model-proxy" {
			m["config"].(map[string]any)["api"].(map[string]any)["baseUrl"] = "http://127.0.0.1:9999"
		}
	}
	out, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(target, out, 0o600); err != nil {
		t.Fatal(err)
	}
	current, _ = tmpl.Pointer(cfg)
	if current != "http://127.0.0.1:9999" {
		t.Fatalf("drifted pointer = %q", current)
	}
}

// TestZcodeShapeRequiresProtocol: shape zcode derives the provider api.type
// from the variant's protocol — a template without one fails closed at load.
func TestZcodeShapeRequiresProtocol(t *testing.T) {
	if _, err := takeover.ParseTemplate("bad", "test", []byte(`
file: /tmp/x.json
format: json
json:
  set: {schemaVersion: 1}
models:
  shape: zcode
`)); err == nil || !strings.Contains(err.Error(), "protocol") {
		t.Fatalf("shape zcode without protocol must fail: %v", err)
	}
}
