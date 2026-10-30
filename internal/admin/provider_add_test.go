package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// newAddProviderHarness builds a service whose config ports resolve through
// the provider store (mirroring the daemon's effective view) plus an
// HTTP-upstream fake for the account-validation and models-preview paths.
func newAddProviderHarness(t *testing.T, upstream *httptest.Server) (*Service, *reloadSpy, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // the account store rides the process HOME seam
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("listen: 127.0.0.1:8080\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: "+upstream.URL+", models: [glm-4.6]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spy := &reloadSpy{}
	service := New(Ports{
		ConfigFile: func() string { return cfgPath },
		HomeDir:    func() string { return home },
		Reload:     spy.fn(),
		ProviderConfig: func(name string) (configdomain.Provider, bool) {
			providers, err := providerstore.Resolve(cfgPath, providerstore.PathUnder(home))
			if err != nil {
				return configdomain.Provider{}, false
			}
			p, ok := providers[name]
			return p, ok
		},
		Config: func() *configdomain.Config {
			data, err := os.ReadFile(cfgPath)
			if err != nil {
				return &configdomain.Config{}
			}
			providers, perr := providerstore.Resolve(cfgPath, providerstore.PathUnder(home))
			if perr != nil {
				return &configdomain.Config{}
			}
			cfg, cerr := configdomain.LoadConfigEffective(cfgPath, data, providers)
			if cerr != nil {
				return &configdomain.Config{}
			}
			return cfg
		},
	})
	return service, spy, cfgPath
}

func addProviderUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /models (account validation + preview) and probe legs all succeed.
		w.Write([]byte(`{"data":[{"id":"acme-m1"},{"id":"acme-m2"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestAddProviderStaticWithToken: a static definition with a token lands in
// the store, the yaml section is absorbed, and the token routes into the
// account store (never provider.json).
func TestAddProviderStaticWithToken(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, cfgPath := newAddProviderHarness(t, up)

	result, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{
		Name:          "acme",
		OpenAIBaseURL: up.URL + "/v1",
		UsageURL:      up.URL + "/v1/usage",
		Billing:       "pay-as-you-go",
		Token:         "sk-acme-test-key-123456",
		Models:        []string{"acme-m1"},
	})
	if err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	if result.Status != "added" || result.Name != "acme" {
		t.Fatalf("result = %+v", result)
	}
	if result.AccountID == "" {
		t.Fatalf("token must create an account: %+v", result)
	}
	if result.AccountWarning != "" || result.ReloadWarning != "" {
		t.Fatalf("warnings = %+v", result)
	}

	store := readProviderStore(t, service)
	entry, ok := store["acme"]
	if !ok || entry.Provider != "static" || entry.Billing != "pay-as-you-go" {
		t.Fatalf("store entry = %+v", store["acme"])
	}
	if !strings.Contains(string(mustRead(t, storeJSONPath(t, service))), "acme-m1") {
		// models persisted
	}
	if strings.Contains(string(mustRead(t, storeJSONPath(t, service))), "sk-acme-test-key") {
		t.Fatal("TOKEN LEAKED INTO provider.json — red line 3 violation")
	}
	// The credential lives in the account pool file instead.
	pool := mustRead(t, filepath.Join(service.homeDir(), ".model-proxy", "acme_apikeys.json"))
	if !strings.Contains(pool, "sk-acme-test-key-123456") {
		t.Fatalf("account pool missing the token:\n%s", maskSecrets(pool))
	}
	// The yaml fixture's zhipu block was absorbed and stripped in the same
	// mutation; the reload chain fired.
	body := readTestFile(t, cfgPath)
	if strings.Contains(body, "providers:") {
		t.Fatalf("yaml providers section must be stripped:\n%s", body)
	}
	if len(spy.calls) < 2 {
		t.Errorf("reload calls = %v, want at least the provider and account reloads", spy.calls)
	}
}

// TestAddProviderPresetBase: preset mode layers explicit fields over the
// template entry; the template's base URL and default models survive.
func TestAddProviderPresetBase(t *testing.T) {
	up := addProviderUpstream(t)
	service, _, _ := newAddProviderHarness(t, up)

	if _, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{
		Name:    "my-zhipu",
		Preset:  "zhipu",
		Billing: "pay-as-you-go",
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	store := readProviderStore(t, service)
	entry, ok := store["my-zhipu"]
	if !ok || entry.Provider != "zhipu" || entry.OpenAIBaseURL == "" {
		t.Fatalf("preset-based entry = %+v", entry)
	}
	if entry.Billing != "pay-as-you-go" {
		t.Errorf("explicit billing must override the template: %q", entry.Billing)
	}
	if len(entry.Models) == 0 {
		t.Error("preset default models must carry over")
	}
}

func TestAddProviderDuplicateIsConflict(t *testing.T) {
	up := addProviderUpstream(t)
	service, _, _ := newAddProviderHarness(t, up)
	_, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{Name: "zhipu", Preset: "zhipu"})
	if status := httpErrorStatus(t, err); status != http.StatusConflict {
		t.Fatalf("duplicate add status = %d, want 409 (err=%v)", status, err)
	}
}

func TestAddProviderRejectsInvalidEntry(t *testing.T) {
	up := addProviderUpstream(t)
	service, _, _ := newAddProviderHarness(t, up)
	_, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{Name: "broken"})
	if err == nil || !strings.Contains(err.Error(), "base_url") {
		t.Fatalf("err = %v, want a base-URL validation error", err)
	}
	if _, serr := os.Stat(storeJSONPath(t, service)); !os.IsNotExist(serr) {
		t.Fatal("rejected add must not create the store")
	}
	_, err = service.AddProvider(context.Background(), appapi.ProviderAddRequest{Name: "x", Preset: "no-such"})
	if status := httpErrorStatus(t, err); status != http.StatusBadRequest {
		t.Fatalf("unknown preset status = %d, want 400", status)
	}
}

// TestPreviewProviderModelsFetchOnly: the preview fetches through the
// transient fields + token and persists NOTHING.
func TestPreviewProviderModelsFetchOnly(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"data":[{"id":"zz-2"},{"id":"zz-1"}]}`))
	}))
	t.Cleanup(srv.Close)
	service, _, _ := newAddProviderHarness(t, srv)

	models, err := service.PreviewProviderModels(context.Background(), appapi.ProviderModelsPreviewRequest{
		OpenAIBaseURL: srv.URL + "/v1",
		Token:         "sk-preview-token-123456",
	})
	if err != nil {
		t.Fatalf("PreviewProviderModels: %v", err)
	}
	if len(models) != 2 || models[0] != "zz-1" || models[1] != "zz-2" {
		t.Fatalf("models = %v, want sorted [zz-1 zz-2]", models)
	}
	if gotAuth != "Bearer sk-preview-token-123456" {
		t.Fatalf("preview auth = %q", gotAuth)
	}
	if _, serr := os.Stat(storeJSONPath(t, service)); !os.IsNotExist(serr) {
		t.Fatal("preview must not create the store")
	}
}

func TestPreviewProviderModelsRejections(t *testing.T) {
	up := addProviderUpstream(t)
	service, _, _ := newAddProviderHarness(t, up)
	ctx := context.Background()

	_, err := service.PreviewProviderModels(ctx, appapi.ProviderModelsPreviewRequest{})
	if status := httpErrorStatus(t, err); status != http.StatusBadRequest {
		t.Fatalf("no base URL status = %d, want 400", status)
	}
	_, err = service.PreviewProviderModels(ctx, appapi.ProviderModelsPreviewRequest{ProviderID: "codex", OpenAIBaseURL: "https://x/v1"})
	if err == nil || !strings.Contains(err.Error(), "OAuth") {
		t.Fatalf("codex preview err = %v, want the OAuth hint", err)
	}
	_, err = service.PreviewProviderModels(ctx, appapi.ProviderModelsPreviewRequest{ProviderID: "volcengine", OpenAIBaseURL: "https://x/v1"})
	if err == nil || !strings.Contains(err.Error(), "volcengine") {
		t.Fatalf("volcengine preview err = %v, want the AK/SK hint", err)
	}
}

func TestPreviewProviderModelsUpstreamErrorIsBadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	service, _, _ := newAddProviderHarness(t, srv)
	_, err := service.PreviewProviderModels(context.Background(), appapi.ProviderModelsPreviewRequest{OpenAIBaseURL: srv.URL + "/v1"})
	if status := httpErrorStatus(t, err); status != http.StatusBadGateway {
		t.Fatalf("upstream 404 status = %d, want 502 (err=%v)", status, err)
	}
}

func storeJSONPath(t *testing.T, service *Service) string {
	t.Helper()
	return providerstore.PathUnder(service.homeDir())
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// maskSecrets keeps credential values out of test FAILURE output (red line 3
// applies to logs and dumps, not to in-memory comparisons).
func maskSecrets(s string) string {
	if len(s) > 40 {
		return s[:20] + "…" + "(masked)"
	}
	return "(masked)"
}
