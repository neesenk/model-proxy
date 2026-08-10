package main

import (
	"io"
	"model-proxy/internal/protocol"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- cmdModels refresh: no provider → usage + available providers ---

func TestCLI_ModelsRefreshNoProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg, "refresh")
	if code != 0 {
		t.Errorf("models refresh (no provider): exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "aqp") {
		t.Errorf("models refresh (no provider) missing usage/providers:\n%s", stdout)
	}
}

// --- cmdModels refresh <unknown> → non-zero ---

func TestCLI_ModelsRefreshUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "models", cfg, "refresh", "nope")
	if code == 0 {
		t.Error("models refresh nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models refresh nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// --- cmdModels refresh <zhipu> happy path: mock /models + cred file ---

func TestCLI_ModelsRefreshZhipuMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("models refresh auth=%q want Bearer test-key", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"},{"id":"glm-4.5","object":"model"}]}`))
	}))
	defer srv.Close()

	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
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
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	_, stderr, code := runCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
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
}

// --- models refresh: FetchModels unavailable -> route-probe fallback ---
//
// When the provider has no /models endpoint (FetchModels 404s), refresh falls
// back to probing route-configured models + existing config models against the
// provider's own chat endpoint. Callable 2xx models are written to models:;
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
		// /chat/completions probe: 2xx for callable models, 404 (+ error body) otherwise.
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
	cfgPath := writeTempConfig(t, cfgBody)

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

	_, stderr, code := runCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
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
	cfgPath := writeTempConfig(t, cfgBody)
	before, _ := os.ReadFile(cfgPath)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	_, stderr, code := runCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
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

// --- models: implicit-route ambiguity warning (model in 2 logged-in providers, no route) ---

func TestCLI_ModelsImplicitRouteAmbiguityWarning(t *testing.T) {
	// deepseek + zhipu both list "shared-model"; no routes. Both logged in.
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  deepseek:\n    provider_id: deepseek\n    openai_base_url: http://x\n    models:\n      - shared-model\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: http://x\n    models:\n      - shared-model\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)
	os.WriteFile(filepath.Join(credDir, "deepseek_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)

	// models.dev endpoint that fails fast (display degrades to defaults; unrelated to the warning).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	_, stderr, code := runCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models exit=%d", code)
	}
	// deepseek < zhipu alphabetically → auto-route to deepseek; warning names both.
	if !strings.Contains(stderr, "shared-model") || !strings.Contains(stderr, "deepseek") || !strings.Contains(stderr, "zhipu") {
		t.Errorf("stderr should warn about shared-model ambiguity naming deepseek+zhipu:\n%s", stderr)
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

	cfgPath := writeTempConfig(t, minimalConfig)
	t.Setenv("MP_MODELSDEV_URL", srv.URL)
	home := t.TempDir()
	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath, "pull")
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
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// pre-seed a FRESH cache (within TTL) so no fetch happens
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"\"v1\"","by_name":{"glm-4.6":{"ctx":204800,"out":131072,"in":["text"],"out_mod":["text"]}},"by_endpoint":{"https://open.bigmodel.cn/api/paas/v4":["glm-4.6"]}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	// endpoint that FAILS if contacted (proves the fresh cache was used instead)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models display exit=%d", code)
	}
	if called {
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

// readAll is a tiny test helper.
func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}
