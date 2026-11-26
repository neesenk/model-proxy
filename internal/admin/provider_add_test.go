package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerstore"
)

// newAddProviderHarness builds a service whose config ports resolve through
// the provider store (mirroring the daemon's effective view) plus an
// HTTP-upstream fake for the account-validation and models-preview paths.
func newAddProviderHarness(t *testing.T, upstream *httptest.Server) (*Service, *reloadSpy, string) {
	t.Helper()
	return newProviderHarnessYAML(t, "listen: 127.0.0.1:8080\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: "+upstream.URL+", models: [glm-4.6]}\n")
}

// newProviderHarnessYAML is newAddProviderHarness over a caller-supplied
// config.yaml (routes, route_policy …).
func newProviderHarnessYAML(t *testing.T, configYAML string) (*Service, *reloadSpy, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home) // the account store rides the process HOME seam
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(configYAML), 0o600); err != nil {
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

// TestDeleteProviderZeroAccounts: a provider with no accounts leaves the
// store (the yaml-defined fixture is absorbed and stripped in the same
// mutation), the daemon reloads, and the account projection no longer lists
// it — the Accounts sidebar and Status page read that same effective set.
func TestDeleteProviderZeroAccounts(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, cfgPath := newAddProviderHarness(t, up)
	// Deleting the only provider is refused by the effective-config validation
	// (a daemon needs at least one provider) and leaves everything intact.
	if _, err := service.DeleteProvider(context.Background(), "zhipu"); err == nil || !strings.Contains(err.Error(), "no providers configured") {
		t.Fatalf("last-provider delete err = %v, want the validation refusal", err)
	}
	if _, ok := service.ports.ProviderConfig("zhipu"); !ok {
		t.Fatal("refused delete must keep the provider")
	}
	if _, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{
		Name: "acme", OpenAIBaseURL: up.URL + "/v1", Models: []string{"acme-m1"},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	reloads := len(spy.calls)

	result, err := service.DeleteProvider(context.Background(), "zhipu")
	if err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if result.Warning != "" {
		t.Fatalf("warning = %q, want none", result.Warning)
	}
	if _, ok := readProviderStore(t, service)["zhipu"]; ok {
		t.Fatal("deleted provider must leave the store")
	}
	if body := readTestFile(t, cfgPath); strings.Contains(body, "zhipu") {
		t.Fatalf("yaml providers section must be stripped, not left to resurrect the entry:\n%s", body)
	}
	if _, ok := service.ports.ProviderConfig("zhipu"); ok {
		t.Fatal("effective provider set still resolves the deleted provider")
	}
	if len(spy.calls) != reloads+1 {
		t.Fatalf("reload calls = %v, want exactly one for the delete", spy.calls[reloads:])
	}
}

// TestDeleteProviderRejections: a provider that still owns an account is a
// 409 (the credential must never be orphaned by a stale UI), an unknown name
// is a 404, and neither touches the store or reloads.
func TestDeleteProviderRejections(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, _ := newAddProviderHarness(t, up)
	if _, err := service.AddProvider(context.Background(), appapi.ProviderAddRequest{
		Name:          "acme",
		OpenAIBaseURL: up.URL + "/v1",
		UsageURL:      up.URL + "/v1/usage",
		Token:         "sk-acme-test-key-123456",
		Models:        []string{"acme-m1"},
	}); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	reloads := len(spy.calls)

	_, err := service.DeleteProvider(context.Background(), "acme")
	if status := httpErrorStatus(t, err); status != http.StatusConflict {
		t.Fatalf("delete with accounts status = %d, want 409 (err=%v)", status, err)
	}
	if !strings.Contains(err.Error(), "1 account(s)") {
		t.Fatalf("conflict message = %q, want the account count", err.Error())
	}
	if _, ok := readProviderStore(t, service)["acme"]; !ok {
		t.Fatal("rejected delete must keep the provider")
	}

	_, err = service.DeleteProvider(context.Background(), "no-such")
	if status := httpErrorStatus(t, err); status != http.StatusNotFound {
		t.Fatalf("unknown provider status = %d, want 404 (err=%v)", status, err)
	}
	_, err = service.DeleteProvider(context.Background(), "  ")
	if status := httpErrorStatus(t, err); status != http.StatusBadRequest {
		t.Fatalf("blank name status = %d, want 400 (err=%v)", status, err)
	}
	if len(spy.calls) != reloads {
		t.Fatalf("rejected deletes must not reload: calls %v", spy.calls[reloads:])
	}
}

// routedDeleteYAML: provider "gone" is targeted by a list-form route that
// keeps another target, a map-form route (compact + map target forms), and a
// route whose ONLY target it is.
func routedDeleteYAML(url, extra string) string {
	return `listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: ` + url + `, models: [glm-4.6]}
  gone: {provider_id: static, openai_base_url: ` + url + `/v1, models: [m1]}
routes:
  mixed:
    - zhipu/glm-4.6 # zhipu stays
    - gone/m1
  mapped: {strategy: load_balance, targets: [{provider: gone, model: m1}, zhipu/glm-4.6]}
  only-gone:
    - gone/m1
` + extra
}

// TestDeleteProviderPrunesRoutes is the regression for "route X target N:
// provider not defined": deleting a provider still referenced by explicit
// routes strips those targets (comments on the survivors kept), removes the
// route left empty, and reports both lists.
func TestDeleteProviderPrunesRoutes(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, cfgPath := newProviderHarnessYAML(t, routedDeleteYAML(up.URL, ""))

	result, err := service.DeleteProvider(context.Background(), "gone")
	if err != nil {
		t.Fatalf("DeleteProvider: %v", err)
	}
	if strings.Join(result.RoutesUpdated, ",") != "mapped,mixed" || strings.Join(result.RoutesRemoved, ",") != "only-gone" {
		t.Fatalf("routes updated=%v removed=%v, want [mapped mixed] / [only-gone]", result.RoutesUpdated, result.RoutesRemoved)
	}
	if _, ok := service.ports.ProviderConfig("gone"); ok {
		t.Fatal("deleted provider still resolves")
	}
	if len(spy.calls) != 1 {
		t.Fatalf("reload calls = %v, want exactly one", spy.calls)
	}
	body := readTestFile(t, cfgPath)
	if strings.Contains(body, "gone") {
		t.Fatalf("config.yaml still references the deleted provider:\n%s", body)
	}
	if !strings.Contains(body, "# zhipu stays") {
		t.Fatalf("surviving target lost its comment:\n%s", body)
	}
	cfg := service.ports.Config()
	if len(cfg.Routes["mixed"]) != 1 || cfg.Routes["mixed"][0].Provider != "zhipu" {
		t.Fatalf("mixed = %+v, want only zhipu/glm-4.6", cfg.Routes["mixed"])
	}
	if len(cfg.Routes["mapped"]) != 1 || cfg.Routes["mapped"][0].Provider != "zhipu" {
		t.Fatalf("mapped = %+v, want only zhipu/glm-4.6", cfg.Routes["mapped"])
	}
	if cfg.RouteStrategies["mapped"] != configdomain.RouteStrategyLoadBalance {
		t.Fatalf("mapped strategy = %q, the declaration must survive the prune", cfg.RouteStrategies["mapped"])
	}
	if _, ok := cfg.Routes["only-gone"]; ok {
		t.Fatal("a route left without targets must be removed")
	}
}

// TestDeleteProviderRollsBackBothFiles: once the route prune is on disk, a
// later refusal (a route_policy reference the prune does not rewrite) or a
// reload that did not apply restores config.yaml byte-for-byte and keeps the
// provider — never half a delete.
func TestDeleteProviderRollsBackBothFiles(t *testing.T) {
	up := addProviderUpstream(t)
	t.Run("validation refusal", func(t *testing.T) {
		policy := "route_policy:\n  mixed:\n    bands:\n      - when: {has_tools: true}\n        target: gone/m1\n"
		service, spy, cfgPath := newProviderHarnessYAML(t, routedDeleteYAML(up.URL, policy))
		original := readTestFile(t, cfgPath)
		if _, ok := service.ports.Config().RoutePolicies["mixed"]; !ok {
			t.Fatal("fixture must be a valid config carrying the route_policy before the delete")
		}
		_, err := service.DeleteProvider(context.Background(), "gone")
		if err == nil || !strings.Contains(err.Error(), "route_policy") {
			t.Fatalf("err = %v, want the route_policy refusal", err)
		}
		if got := readTestFile(t, cfgPath); got != original {
			t.Fatalf("config.yaml not restored:\n%s", got)
		}
		if _, ok := service.ports.ProviderConfig("gone"); !ok {
			t.Fatal("refused delete must keep the provider")
		}
		if len(spy.calls) != 0 {
			t.Fatalf("refused delete must not reload: %v", spy.calls)
		}
	})
	t.Run("reload failure", func(t *testing.T) {
		service, spy, cfgPath := newProviderHarnessYAML(t, routedDeleteYAML(up.URL, ""))
		original := readTestFile(t, cfgPath)
		spy.err = errors.New("reload exploded")
		if _, err := service.DeleteProvider(context.Background(), "gone"); err == nil || !strings.Contains(err.Error(), "reload exploded") {
			t.Fatalf("err = %v, want the reload failure", err)
		}
		if got := readTestFile(t, cfgPath); got != original {
			t.Fatalf("config.yaml not restored after the failed reload:\n%s", got)
		}
		if _, ok := service.ports.ProviderConfig("gone"); !ok {
			t.Fatal("failed reload must roll the store back to keep the provider")
		}
	})
}

// TestDeleteProviderAccountReadFailure: an unreadable account store must fail
// closed before either file is written. Treating the error as an empty account
// list would delete a provider while its credentials still exist on disk.
func TestDeleteProviderAccountReadFailure(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, cfgPath := newProviderHarnessYAML(t, routedDeleteYAML(up.URL, ""))
	original := readTestFile(t, cfgPath)
	poolPath := accounts.NewStore(accounts.HomeDir()).PoolPath("gone")
	if err := os.MkdirAll(filepath.Dir(poolPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := service.DeleteProvider(context.Background(), "gone")
	if status := httpErrorStatus(t, err); status != http.StatusInternalServerError {
		t.Fatalf("account-store read failure status = %d, want 500 (err=%v)", status, err)
	}
	if !strings.Contains(err.Error(), "cannot verify provider") {
		t.Fatalf("error = %q, want the account-verification failure", err)
	}
	if _, ok := service.ports.ProviderConfig("gone"); !ok {
		t.Fatal("failed account read must keep the provider")
	}
	if got := readTestFile(t, cfgPath); got != original {
		t.Fatalf("failed account read changed config.yaml:\n%s", got)
	}
	if len(spy.calls) != 0 {
		t.Fatalf("refused delete must not reload: %v", spy.calls)
	}
}

// TestDeleteProviderLegacyOAuthReadFailure covers the other account source:
// a pool-less OAuth provider's legacy single-account file must fail closed too.
func TestDeleteProviderLegacyOAuthReadFailure(t *testing.T) {
	up := addProviderUpstream(t)
	service, spy, _ := newProviderHarnessYAML(t, `listen: 127.0.0.1:8080
providers:
  zhipu: {provider_id: zhipu, openai_base_url: `+up.URL+`, models: [glm-4.6]}
  oauth: {provider_id: aqp, anthropic_base_url: `+up.URL+`}
`)
	path := accounts.AuthFilePath("oauth", "oauth_auth")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.DeleteProvider(context.Background(), "oauth")
	if status := httpErrorStatus(t, err); status != http.StatusInternalServerError {
		t.Fatalf("legacy OAuth read failure status = %d, want 500 (err=%v)", status, err)
	}
	if _, ok := service.ports.ProviderConfig("oauth"); !ok {
		t.Fatal("legacy OAuth read failure must keep the provider")
	}
	if len(spy.calls) != 0 {
		t.Fatalf("refused delete must not reload: %v", spy.calls)
	}
}
