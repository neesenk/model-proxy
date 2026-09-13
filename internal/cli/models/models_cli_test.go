package models

import (
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/providerbuild"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// --- models refresh: no provider → usage + available providers ---

func TestCLI_ModelsRefreshNoProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	stdout, _, code := clitest.RunCLI(t, "models", cfg, "refresh")
	if code != 0 {
		t.Errorf("models refresh (no provider): exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "aqp") {
		t.Errorf("models refresh (no provider) missing usage/providers:\n%s", stdout)
	}
}

// --- models refresh <unknown> → non-zero ---

func TestCLI_ModelsRefreshUnknownProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	_, stderr, code := clitest.RunCLI(t, "models", cfg, "refresh", "nope")
	if code == 0 {
		t.Error("models refresh nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models refresh nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// --- models refresh <zhipu> happy path: mock /models + cred file ---

func TestCLI_ModelsRefreshZhipuMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("models refresh auth=%q want Bearer test-key", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"},{"id":"glm-4.5","object":"model"}]}`))
	}))
	defer srv.Close()

	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	stdout, _, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
	if code != 0 {
		t.Fatalf("models refresh zhipu: exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") || !strings.Contains(stdout, "glm-4.5") {
		t.Errorf("models refresh zhipu missing models:\n%s", stdout)
	}
}

// --- models refresh: persists newly-discovered models into config.yaml ---

func TestCLI_ModelsRefresh_PersistsNewModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"},{"id":"glm-new-model","object":"model"}]}`))
	}))
	defer srv.Close()

	// Config lists only glm-5.2; glm-new-model is new.
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	_, stderr, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
	if code != 0 {
		t.Fatalf("models refresh exit=%d", code)
	}
	if !strings.Contains(stderr, "glm-new-model") || !strings.Contains(stderr, "added") {
		t.Errorf("refresh should report the new model:\n%s", stderr)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	if !strings.Contains(cfg, "- glm-new-model") {
		t.Errorf("config should now list glm-new-model:\n%s", cfg)
	}
	// existing model + routes + the comment-free structure preserved
	if !strings.Contains(cfg, "- glm-5.2") || !strings.Contains(cfg, "routes:") {
		t.Errorf("config lost existing model or routes:\n%s", cfg)
	}
	// The fresh 3-protocol matrix was persisted to model_caps.json (sibling of
	// quota_state.json under the isolated HOME), fingerprinted with the
	// provider's current protocol config.
	loaded, err := runtimewire.LoadModelCapsFile(filepath.Join(home, ".model-proxy", "model_caps.json"))
	if err != nil {
		t.Fatalf("LoadModelCapsFile: %v", err)
	}
	entry, ok := loaded["zhipu"]
	if !ok {
		t.Fatalf("model_caps.json missing zhipu entry: %v", loaded)
	}
	wantFP := providerbuild.ProtocolConfigFingerprint(configdomain.Provider{Provider: "zhipu", OpenAIBaseURL: srv.URL})
	if entry.Fingerprint != wantFP {
		t.Errorf("caps fingerprint=%q want %q", entry.Fingerprint, wantFP)
	}
	for _, id := range []string{"glm-5.2", "glm-new-model"} {
		mp, ok := entry.Models[id]
		if !ok {
			t.Errorf("caps models missing %s: %v", id, entry.Models)
			continue
		}
		// The mock answers 200 on every path: chat + responses Yes, anthropic
		// No (no anthropic_base_url configured).
		if mp.Chat != runtimewire.Yes || mp.Responses != runtimewire.Yes || mp.Anthropic != runtimewire.No {
			t.Errorf("caps %s = chat:%s ant:%s resp:%s, want yes/no/yes", id, mp.Chat, mp.Anthropic, mp.Responses)
		}
	}
}

// --- models refresh: FetchModels unavailable -> route-probe fallback ---
//
// When the provider has no /models endpoint (FetchModels 404s), refresh falls
// back to probing route-configured models + existing config models with the
// 3-protocol matrix. Models callable on ANY leg are written to models:;
// non-callable ones are dropped. Mirrors the FetchModels path's write semantics.

func TestCLI_ModelsRefreshFallback_RouteProbe(t *testing.T) {
	callable := map[string]bool{"keep-a": true, "route-c": true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			// No /models endpoint -> fetchModelsBearer errors -> fallback triggers.
			w.WriteHeader(404)
			w.Write([]byte(`{"error":{"code":"NotFound","message":"no models endpoint"}}`))
			return
		}
		// Probe legs (/chat/completions, /responses): 2xx for callable models,
		// 404 (+ error body) otherwise.
		model := protocol.ExtractModel(readAll(r.Body))
		if callable[model] {
			w.WriteHeader(200)
			w.Write([]byte(`{}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":{"code":"UnsupportedModel","message":"model not served"}}`))
	}))
	defer srv.Close()

	// Config lists keep-a + drop-b; a route adds route-c (route-only candidate).
	// Candidates probed = {keep-a, drop-b} (config) ∪ {route-c} (route).
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - keep-a\n      - drop-b\nroutes:\n  keep-a:\n    - {provider: zhipu, model: keep-a}\n  route-c:\n    - {provider: zhipu, model: route-c}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)
	// Fresh empty models.dev cache so hydrateModels doesn't hit the network.
	os.WriteFile(filepath.Join(credDir, "models_cache.json"),
		[]byte(`{"fetched_at":"`+time.Now().Format(time.RFC3339)+`","etag":"","by_name":{},"by_endpoint":{}}`), 0o600)
	// Safety net: if the cache is somehow bypassed, the endpoint MUST fail
	// (proves no network dependency) rather than silently succeed.
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer mdSrv.Close()
	t.Setenv("MP_MODELSDEV_URL", mdSrv.URL)

	_, stderr, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
	if code != 0 {
		t.Fatalf("models refresh fallback: exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "models endpoint unavailable for zhipu") || !strings.Contains(stderr, "probing route-configured models") {
		t.Errorf("stderr should announce the route-probe fallback:\n%s", stderr)
	}
	// The diff lines prove the write happened with the right delta.
	if !strings.Contains(stderr, "config: added 1 -> [route-c]") {
		t.Errorf("stderr should report route-c added:\n%s", stderr)
	}
	if !strings.Contains(stderr, "config: removed 1 -> [drop-b]") {
		t.Errorf("stderr should report drop-b removed:\n%s", stderr)
	}
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(data)
	if !strings.Contains(cfg, "- keep-a") || !strings.Contains(cfg, "- route-c") {
		t.Errorf("config should list the callable subset (keep-a, route-c):\n%s", cfg)
	}
	if strings.Contains(cfg, "- drop-b") {
		t.Errorf("config should have dropped non-callable drop-b:\n%s", cfg)
	}
	if !strings.Contains(cfg, "routes:") {
		t.Errorf("config should preserve routes + structure:\n%s", cfg)
	}
}

// --- models refresh: idempotent when nothing new (no write) ---

func TestCLI_ModelsRefresh_IdempotentNothingNew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"}]}`))
	}))
	defer srv.Close()

	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)
	before, _ := os.ReadFile(cfgPath)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	_, stderr, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
	if code != 0 {
		t.Fatalf("models refresh exit=%d", code)
	}
	if strings.Contains(stderr, "added") {
		t.Errorf("should not report new models when nothing new:\n%s", stderr)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(before) != string(after) {
		t.Errorf("config should be unchanged when nothing new:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// --- models pull: force-refresh from a mocked models.dev endpoint ---

func TestCLI_ModelsPull_MockedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag := r.Header.Get("If-None-Match"); etag != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"zhipuai":{"api":"https://open.bigmodel.cn/api/paas/v4","models":{"glm-4.6":{"limit":{"context":204800,"output":131072},"modalities":{"input":["text"],"output":["text"]}}}}}`))
	}))
	defer srv.Close()

	cfgPath := clitest.WriteTempConfig(t, minimalConfig)
	t.Setenv("MP_MODELSDEV_URL", srv.URL)
	home := t.TempDir()
	stdout, _, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "pull")
	if code != 0 {
		t.Fatalf("models pull exit=%d", code)
	}
	if !strings.Contains(stdout, "models.dev catalog refreshed") || !strings.Contains(stdout, "1 unique models") {
		t.Errorf("models pull output unexpected:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "models_cache.json")); err != nil {
		t.Errorf("cache file not created: %v", err)
	}
}

// --- models display: hydrates from a fresh pre-seeded cache (no network) ---

func TestCLI_ModelsDisplay_HydratesFromCache(t *testing.T) {
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://open.bigmodel.cn/api/paas/v4\nroutes:\n  glm-4.6:\n    - {provider: zhipu, model: glm-4.6}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// pre-seed a FRESH cache (within TTL) so no fetch happens
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"\"v1\"","by_name":{"glm-4.6":{"ctx":204800,"out":131072,"in":["text"],"out_mod":["text"]}},"by_endpoint":{"https://open.bigmodel.cn/api/paas/v4":["glm-4.6"]}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	// endpoint that FAILS if contacted (proves the fresh cache was used instead).
	// The handler runs on the server goroutine while the CLI runs in a
	// subprocess — atomic so the read below is race-free either way.
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called.Store(true)
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	stdout, _, code := clitest.RunCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models display exit=%d", code)
	}
	if called.Load() {
		t.Error("fresh cache should NOT have fetched from endpoint")
	}
	if !strings.Contains(stdout, "glm-4.6") || !strings.Contains(stdout, "models.dev") {
		t.Errorf("display should show glm-4.6 with models.dev source:\n%s", stdout)
	}
	// ctx 204800 came from the cache, not config (config had no models:)
	if !strings.Contains(stdout, "204800") {
		t.Errorf("display should show cached context 204800:\n%s", stdout)
	}
}

// --- models display: PROTOCOLS column from a seeded model_caps.json ---
//
// The column is a read-only projection of model_caps.json: a provider's entry
// is used ONLY when its stored fingerprint matches the provider's current
// protocol config; stale entries degrade to "-".

func TestCLI_ModelsDisplay_ProtocolsColumn(t *testing.T) {
	cfgBody := "listen: 127.0.0.1:15721\n" +
		"providers:\n" +
		"  zhipu:\n    openai_base_url: https://zhipu.invalid/v1\n    provider_id: zhipu\n    models:\n      - glm-5.2\n" +
		"  aqp:\n    openai_base_url: https://aqp.invalid/v1\n    provider_id: aqp\n    models:\n      - m-stale\n" +
		"routes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// Fresh empty models.dev cache so hydrateModels doesn't hit the network.
	os.WriteFile(filepath.Join(credDir, "models_cache.json"),
		[]byte(`{"fetched_at":"`+time.Now().Format(time.RFC3339)+`","etag":"","by_name":{},"by_endpoint":{}}`), 0o600)
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer mdSrv.Close()
	t.Setenv("MP_MODELSDEV_URL", mdSrv.URL)

	// Seed model_caps.json: zhipu with the CURRENT fingerprint (projected),
	// aqp with a STALE one (different base url -> must be ignored).
	matrix := map[string]runtimewire.ModelProtocols{
		"glm-5.2": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes},
	}
	err := runtimewire.SaveModelCapsFile(filepath.Join(credDir, "model_caps.json"), map[string]runtimewire.ProviderModelCaps{
		"zhipu": {
			Fingerprint: providerbuild.ProtocolConfigFingerprint(configdomain.Provider{Provider: "zhipu", OpenAIBaseURL: "https://zhipu.invalid/v1"}),
			ProbedAt:    time.Now(),
			Models:      matrix,
		},
		"aqp": {
			Fingerprint: providerbuild.ProtocolConfigFingerprint(configdomain.Provider{Provider: "aqp", OpenAIBaseURL: "https://OLD.invalid/v1"}),
			ProbedAt:    time.Now(),
			Models:      map[string]runtimewire.ModelProtocols{"m-stale": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.Yes}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, code := clitest.RunCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models display exit=%d", code)
	}
	if !strings.Contains(stdout, "PROTOCOLS") {
		t.Errorf("display should show the PROTOCOLS column:\n%s", stdout)
	}
	if !strings.Contains(stdout, "chat/resp") {
		t.Errorf("glm-5.2 should render chat/resp from the seeded caps:\n%s", stdout)
	}
	// aqp's entry is fingerprint-stale -> its model must NOT render the seeded
	// verdicts (projection treats it as no data -> "-").
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "m-stale") {
			if strings.Contains(line, "chat") || strings.Contains(line, "ant") || strings.Contains(line, "resp") {
				t.Errorf("stale-fingerprint row should render '-' in PROTOCOLS, got: %q", line)
			}
		}
	}
}

// --- models refresh <kimi-code>: upstream display names are surfaced ---

// TestCLI_ModelsRefreshKimiCodeDisplayNames pins the id-stable/model-swapped
// signal end to end: Kimi Code serves "K2.8 Preview" under the unchanged id
// `kimi-for-coding`, so refresh's id diff stays empty — the upstream display
// name is the only visible cue. It must appear both as a stderr per-model line
// and in the kept table's NAME column.
func TestCLI_ModelsRefreshKimiCodeDisplayNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/models" {
			w.Write([]byte(`{"object":"list","data":[
				{"id":"kimi-for-coding","object":"model","display_name":"K2.8 Preview"},
				{"id":"k3","object":"model","display_name":"K3"}
			]}`))
			return
		}
		// Probe legs (chat/responses): 200 with an empty body classifies Yes.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  kimi-code:\n    openai_base_url: " + srv.URL + "\n    provider_id: kimi-code\n    models:\n      - kimi-for-coding\n      - k3\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "kimi-code_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	stdout, stderr, code := clitest.RunCLIWithHome(t, home, "models", cfgPath, "refresh", "kimi-code")
	if code != 0 {
		t.Fatalf("models refresh kimi-code: exit=%d want 0\nstderr:\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "upstream model name: kimi-for-coding -> K2.8 Preview") {
		t.Errorf("stderr missing the display-name line:\n%s", stderr)
	}
	if !strings.Contains(stdout, "K2.8 Preview") {
		t.Errorf("kept table NAME column missing K2.8 Preview:\n%s", stdout)
	}
	// Ids unchanged -> no config diff, no rewrite noise.
	if strings.Contains(stderr, "config: added") || strings.Contains(stderr, "config: removed") {
		t.Errorf("unexpected config diff for an unchanged id set:\n%s", stderr)
	}
}
