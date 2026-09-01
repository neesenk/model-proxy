package app

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestConfigPutValid(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)

	w, p := newTestWeb(t)
	w.configFile = cfgPath
	edited := []byte("listen: 127.0.0.1:17001\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	rec := httptest.NewRecorder()
	body := `{"yaml":"` + strings.ReplaceAll(strings.ReplaceAll(string(edited), "\n", "\\n"), `"`, `\"`) + `"}`
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, edited) {
		t.Errorf("config not written; got %q want %q", got, edited)
	}
	bak := readLatestBackup(t, cfgPath)
	if !bytes.Equal(bak, original) {
		t.Errorf("backup not original; got %q", bak)
	}
	// reload happened: p.cfg.Listen updated.
	if p.cfg.Listen != "127.0.0.1:17001" {
		t.Errorf("reload did not apply: listen=%q", p.cfg.Listen)
	}
}

func TestConfigPutInvalidNoWrite(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)

	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	// invalid: route references a missing provider
	bad := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\nroutes:\n  m: [{provider: ghost, model: m}]\n")
	rec := httptest.NewRecorder()
	body := `{"yaml":"` + strings.ReplaceAll(strings.ReplaceAll(string(bad), "\n", "\\n"), `"`, `\"`) + `"}`
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, original) {
		t.Errorf("invalid write should not touch config; got %q", got)
	}
	if entries, _ := os.ReadDir(filepath.Join(filepath.Dir(cfgPath), ".model-proxy", "back")); len(entries) != 0 {
		t.Errorf("invalid write should not create a backup; .model-proxy/back/ has %d entries", len(entries))
	}
}

// readLatestBackup returns the contents of the most recent backup in the
// `.model-proxy/back/` directory sibling of cfgPath (timestamped names sort
// lexically = chronologically). Fails the test if no backup exists.
func readLatestBackup(t *testing.T, cfgPath string) []byte {
	t.Helper()
	dir := filepath.Join(filepath.Dir(cfgPath), ".model-proxy", "back")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("no backup dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no backup in %s", dir)
	}
	sort.Strings(names)
	b, err := os.ReadFile(filepath.Join(dir, names[len(names)-1]))
	if err != nil {
		t.Fatalf("read backup %s: %v", names[len(names)-1], err)
	}
	return b
}

func TestConfigEditPreservesComments(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("# top comment\nlisten: 127.0.0.1:17000 # inline\n# scheduling block\nscheduling:\n  circuit_threshold: 3\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)
	w, p := newTestWeb(t)
	w.configFile = cfgPath

	rec := httptest.NewRecorder()
	body := `{"kind":"scheduling","data":{"circuit_threshold":5}}`
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	gs := string(got)
	if !strings.Contains(gs, "# top comment") {
		t.Errorf("top comment lost:\n%s", gs)
	}
	if !strings.Contains(gs, "# scheduling block") {
		t.Errorf("scheduling comment lost:\n%s", gs)
	}
	if !strings.Contains(gs, "circuit_threshold: 5") {
		t.Errorf("threshold not updated to 5:\n%s", gs)
	}
	if p.cfg.Scheduling.CircuitThreshold != 5 {
		t.Errorf("reload did not apply threshold 5: %d", p.cfg.Scheduling.CircuitThreshold)
	}
}

func TestConfigEditGeneral(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:17000\nlog_level: info\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(`{"kind":"general","data":{"listen":"127.0.0.1:18000"}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "listen: 127.0.0.1:18000") {
		t.Errorf("listen not updated:\n%s", got)
	}
}

// TestConfigEditUnknownKind asserts a bogus kind returns 400, not 500 or a
// panic. provider/route/claude_mapping are valid kinds as of Task 9
// (covered by TestConfigEditProviderBilling / TestConfigEditRouteCRUD), so this
// test now only covers the genuinely unknown case.
func TestConfigEditUnknownKind(t *testing.T) {
	w, _ := newTestWeb(t)
	for _, kind := range []string{"bogus"} {
		rec := httptest.NewRecorder()
		body := `{"kind":"` + kind + `","name":"x","data":{}}`
		serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(body)))
		if rec.Code != 400 {
			t.Errorf("kind=%s status=%d want 400 body=%s", kind, rec.Code, rec.Body.String())
		}
	}
}

func TestConfigEditProviderBilling(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("providers:\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: https://api.deepseek.com\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"deepseek","data":{"billing":"plan"}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "billing: plan") {
		t.Errorf("billing not set:\n%s", got)
	}
}

// TestConfigEditProviderModels asserts the provider form's models field writes a
// models: sequence (one entry per array element) under providers.<name>, and
// that an absent models key leaves an existing list untouched.
func TestConfigEditProviderModels(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    models:\n      - glm-4.5\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath

	// Set models to a new list -> the old entry is replaced, order preserved.
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"zhipu","data":{"models":["glm-4.6","glm-4.5-air"]}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	gs := string(got)
	// Both new models present, the dropped one gone.
	for _, want := range []string{"- glm-4.6", "- glm-4.5-air"} {
		if !strings.Contains(gs, want) {
			t.Errorf("models missing %q:\n%s", want, gs)
		}
	}
	if strings.Contains(gs, "- glm-4.5\n") {
		t.Errorf("old model glm-4.5 should have been replaced:\n%s", gs)
	}

	// An edit WITHOUT models must leave the list intact (non-destructive).
	rec2 := httptest.NewRecorder()
	serveWeb(w, rec2, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"zhipu","data":{"billing":"plan"}}`)))
	if rec2.Code != 200 {
		t.Fatalf("status=%d body=%s", rec2.Code, rec2.Body.String())
	}
	got2, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got2), "- glm-4.6") {
		t.Errorf("models dropped by a models-less edit:\n%s", got2)
	}
}

// TestConfigGetProviderModels asserts /api/config surfaces provider_models
// (name -> models list) so the Provider form can prefill.
func TestConfigGetProviderModels(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n    models:\n      - glm-4.5\n      - glm-4.6\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ProviderModels map[string][]string `json:"provider_models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	got := resp.ProviderModels["zhipu"]
	want := []string{"glm-4.5", "glm-4.6"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("provider_models[zhipu] = %v, want %v", got, want)
	}
}

// TestConfigGetRoutes asserts /api/config surfaces structured routes
// (exposed -> [{provider, model, priority}]) so the Routes form can prefill +
// edit each route's targets as provider/model/priority rows. A target that
// omits priority inherits its provider's priority (0 when the provider sets
// none either).
func TestConfigGetRoutes(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: https://y\nroutes:\n  glm-4.6:\n    - {provider: zhipu, model: glm-4.6, priority: 1}\n    - {provider: deepseek, model: deepseek-chat}\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Routes map[string][]struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Priority int    `json:"priority"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	got := resp.Routes["glm-4.6"]
	if len(got) != 2 {
		t.Fatalf("routes[glm-4.6] = %d targets, want 2", len(got))
	}
	// First target carries an explicit priority; second omits it -> 0.
	if got[0].Provider != "zhipu" || got[0].Model != "glm-4.6" || got[0].Priority != 1 {
		t.Errorf("target[0] = %+v, want zhipu/glm-4.6/priority 1", got[0])
	}
	if got[1].Provider != "deepseek" || got[1].Model != "deepseek-chat" || got[1].Priority != 0 {
		t.Errorf("target[1] = %+v, want deepseek/deepseek-chat/priority 0 (unset)", got[1])
	}
}

// TestConfigEditProviderAddWithProviderID asserts the "add provider" flow
// succeeds: a new provider block must carry provider_id (configuration validation
// rejects an empty one) and openai_base_url. Pre-fix editStructured never wrote
// provider_id, so adding a provider always 400'd with "provider_id is empty".
func TestConfigEditProviderAddWithProviderID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	// Minimal valid config: one existing provider + listen.
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:17000\nproviders:\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: https://api.deepseek.com\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"zhipu-work","data":{"provider_id":"zhipu","openai_base_url":"https://open.bigmodel.cn/api/paas/v4","billing":"plan"}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	gs := string(got)
	// The new provider block must set provider_id + openai_base_url (validates).
	for _, want := range []string{"zhipu-work:", "provider_id: zhipu", "openai_base_url: https://open.bigmodel.cn/api/paas/v4", "billing: plan"} {
		if !strings.Contains(gs, want) {
			t.Errorf("config missing %q:\n%s", want, gs)
		}
	}
	// The existing provider must survive.
	if !strings.Contains(gs, "deepseek:") {
		t.Errorf("existing provider dropped:\n%s", gs)
	}
}

// TestConfigEditProviderAddMissingProviderID asserts that adding a provider
// WITHOUT provider_id still fails validation (the guard the Web UI relies on),
// and that the on-disk config is unchanged.
func TestConfigEditProviderAddMissingProviderID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: https://api.deepseek.com\n")
	os.WriteFile(cfgPath, original, 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"provider","name":"ghost","data":{"openai_base_url":"https://x"}}`)))
	if rec.Code != 400 {
		t.Fatalf("status=%d want 400 (provider_id missing), body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "provider_id is empty") {
		t.Errorf("error should mention provider_id, got: %s", rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !bytes.Equal(got, original) {
		t.Errorf("config changed on a failed validate - should be unchanged:\n%s", got)
	}
}

// TestAccountsListMasked asserts /api/accounts NEVER serializes any secret
// (api_key / access_key / secret_key / SSO cookie) while still emitting the
// real account id (the UI needs it to remove accounts). The acct struct has no
// field for any secret — by construction they can't be serialized — and the
// test verifies the raw api_key string is absent from the body AND the real id
// is present.
func TestConfigEditRouteCRUD(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("providers:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\nroutes:\n  m: [{provider: zhipu, model: m}]\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit",
		strings.NewReader(`{"kind":"route","name":"m","data":{"targets":[{"provider":"zhipu","model":"m","priority":1}]}}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(got), "priority: 1") {
		t.Errorf("route target not updated:\n%s", got)
	}
}

// TestAccountsAddRemove covers the POST (add) + DELETE (remove) account flows.
// apikey add must validate against usage_url, save to the pool, and trigger a
// best-effort reload (ignored on failure — the account was still persisted).
// aqp/codex add must 400 (they use the async login flow, not the apikey core)
// rather than silently no-op'ing or attempting the apikey path. remove empties
// the pool; an unknown provider add must 404 (route table intact).
// TestConfigEditSchedulingNewIntKey asserts that a structured edit which ADDS a
// brand-new int key (circuit_threshold) to a config with NO scheduling block
// produces a value node whose tag yaml.v3 infers as int (not an explicit !!str).
// Before the scalarNode fix, the new node was !!str "5", yaml.v3 emitted the
// explicit tag, and reload failed "cannot unmarshal !!str 5 into int" (400).
//
// RED-before evidence: with the old scalarNode (Tag: "!!str"), this test fails
// at the status check (got 400 with "cannot unmarshal !!str", want 200) and the
// threshold assertion never runs. After the fix (Tag: ""), yaml.v3 infers int
// from "5" and reload succeeds with CircuitThreshold == 5.
func TestConfigEditSchedulingNewIntKey(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	// Config with NO scheduling block — editScheduling must create both the
	// block and the circuit_threshold key from scratch.
	original := []byte("listen: 127.0.0.1:17000\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://x\n")
	os.WriteFile(cfgPath, original, 0o644)
	w, p := newTestWeb(t)
	w.configFile = cfgPath

	rec := httptest.NewRecorder()
	body := `{"kind":"scheduling","data":{"circuit_threshold":5}}`
	serveWeb(w, rec, httptest.NewRequest("POST", "/api/config/edit", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 body=%s", rec.Code, rec.Body.String())
	}
	// Reload must have decoded the new int key correctly.
	if p.cfg.Scheduling.CircuitThreshold != 5 {
		t.Errorf("reload did not apply threshold 5: %d", p.cfg.Scheduling.CircuitThreshold)
	}
	// The emitted YAML must NOT carry an explicit !!str tag on the value.
	got, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(got), "!!str") {
		t.Errorf("emitted YAML has explicit !!str tag (should infer int):\n%s", got)
	}
}

// TestConfigGetDerivedRoutesAndProviderMeta asserts /api/config surfaces the
// EFFECTIVE route table: models derived from provider model lists (aliases
// applied, priority inherited per provider) alongside explicit routes, plus
// provider_meta (priority + alias) for the UI.
func TestConfigGetDerivedRoutesAndProviderMeta(t *testing.T) {
	dir := t.TempDir()
	cfgPath := dir + "/config.yaml"
	os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:0\nproviders:\n  kimi-code:\n    provider_id: kimi-code\n    openai_base_url: https://x\n    priority: 1\n    alias: {k3: kimi-k3}\n    models: [k3]\n  aqp:\n    provider_id: aqp\n    openai_base_url: https://y\n    priority: 2\n    models: [kimi-k3]\n"), 0o644)
	w, _ := newTestWeb(t)
	w.configFile = cfgPath
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		ProviderMeta map[string]struct {
			Priority int               `json:"priority"`
			Alias    map[string]string `json:"alias"`
		} `json:"provider_meta"`
		Routes map[string][]struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Priority int    `json:"priority"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if pm := resp.ProviderMeta["kimi-code"]; pm.Priority != 1 || pm.Alias["k3"] != "kimi-k3" {
		t.Errorf("provider_meta[kimi-code] = %+v, want priority 1 + alias k3→kimi-k3", pm)
	}
	got := resp.Routes["kimi-k3"]
	if len(got) != 2 {
		t.Fatalf("routes[kimi-k3] = %d targets, want 2 (aggregated): %+v", len(got), got)
	}
	if got[0].Provider != "kimi-code" || got[0].Model != "k3" || got[0].Priority != 1 {
		t.Errorf("target[0] = %+v, want kimi-code/k3 with inherited priority 1", got[0])
	}
	if got[1].Provider != "aqp" || got[1].Model != "kimi-k3" || got[1].Priority != 2 {
		t.Errorf("target[1] = %+v, want aqp/kimi-k3 with provider priority 2", got[1])
	}
	if _, aliasedAway := resp.Routes["k3"]; aliasedAway {
		t.Errorf("aliased-away k3 must not appear as its own route: %+v", resp.Routes)
	}
}
