package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

func TestResetStatsPassThrough(t *testing.T) {
	wantErr := errors.New("reset failed")
	calls := 0
	service := New(Ports{ResetStats: func() error { calls++; return nil }})
	if err := service.ResetStats(); err != nil || calls != 1 {
		t.Errorf("ResetStats = %v, calls = %d", err, calls)
	}
	service = New(Ports{ResetStats: func() error { return wantErr }})
	if err := service.ResetStats(); !errors.Is(err, wantErr) {
		t.Errorf("ResetStats err = %v, want %v", err, wantErr)
	}
}

func TestRefreshQuota(t *testing.T) {
	// Disabled tracker: always true, no polling.
	service := New(Ports{
		QuotaEnabled: func() bool { return false },
		QuotaPollOne: func(string) bool { t.Error("PollOne on disabled tracker"); return false },
		QuotaPollAll: func(time.Time) { t.Error("PollAll on disabled tracker") },
	})
	if !service.RefreshQuota("up") || !service.RefreshQuota("") {
		t.Error("disabled tracker must report true")
	}

	var polled string
	service = New(Ports{
		QuotaEnabled: func() bool { return true },
		QuotaPollOne: func(name string) bool {
			polled = name
			return name == "up"
		},
		QuotaPollAll: func(time.Time) { polled = "all" },
	})
	if !service.RefreshQuota("up") || polled != "up" {
		t.Errorf("named refresh = %v", polled)
	}
	if service.RefreshQuota("missing") {
		t.Error("unknown provider must report false")
	}
	if !service.RefreshQuota("") || polled != "all" {
		t.Errorf("empty name must poll all, polled = %q", polled)
	}
}

func TestResetHealth(t *testing.T) {
	persisted := 0
	service := New(Ports{
		ResetHealth: func(name string) ([]string, int) {
			if name != "up" {
				t.Errorf("ResetHealth name = %q", name)
			}
			return []string{"up"}, 2
		},
		QuotaEnabled: func() bool { return true },
		QuotaPersist: func() error { persisted++; return nil },
	})
	cleared, locks, err := service.ResetHealth("up")
	if err != nil || len(cleared) != 1 || cleared[0] != "up" || locks != 2 || persisted != 1 {
		t.Errorf("ResetHealth = %v %d %v, persisted = %d", cleared, locks, err, persisted)
	}

	// A persist failure propagates but keeps the cleared projection.
	wantErr := errors.New("persist failed")
	service = New(Ports{
		ResetHealth:  func(string) ([]string, int) { return []string{"up"}, 1 },
		QuotaEnabled: func() bool { return true },
		QuotaPersist: func() error { return wantErr },
	})
	cleared, locks, err = service.ResetHealth("up")
	if !errors.Is(err, wantErr) || len(cleared) != 1 || locks != 1 {
		t.Errorf("ResetHealth persist failure = %v %d %v", cleared, locks, err)
	}

	// Disabled tracker: no persist.
	service = New(Ports{
		ResetHealth:  func(string) ([]string, int) { return nil, 0 },
		QuotaEnabled: func() bool { return false },
		QuotaPersist: func() error { t.Error("Persist on disabled tracker"); return nil },
	})
	if _, _, err := service.ResetHealth(""); err != nil {
		t.Errorf("ResetHealth disabled tracker err = %v", err)
	}
}

func TestFreezeHealth(t *testing.T) {
	persisted := 0
	service := New(Ports{
		FreezeHealth: func(name string, known []string) []string {
			if name != "up" {
				t.Errorf("FreezeHealth name = %q", name)
			}
			if known != nil {
				t.Errorf("FreezeHealth known = %v, want nil (composition root derives it)", known)
			}
			return []string{"up"}
		},
		QuotaEnabled: func() bool { return true },
		QuotaPersist: func() error { persisted++; return nil },
	})
	frozen, err := service.FreezeHealth("up")
	if err != nil || len(frozen) != 1 || frozen[0] != "up" || persisted != 1 {
		t.Errorf("FreezeHealth = %v %v, persisted = %d", frozen, err, persisted)
	}

	// A persist failure propagates but keeps the frozen projection.
	wantErr := errors.New("persist failed")
	service = New(Ports{
		FreezeHealth: func(string, []string) []string { return []string{"up"} },
		QuotaEnabled: func() bool { return true },
		QuotaPersist: func() error { return wantErr },
	})
	frozen, err = service.FreezeHealth("up")
	if !errors.Is(err, wantErr) || len(frozen) != 1 {
		t.Errorf("FreezeHealth persist failure = %v %v", frozen, err)
	}

	// Disabled tracker: no persist.
	service = New(Ports{
		FreezeHealth: func(string, []string) []string { return nil },
		QuotaEnabled: func() bool { return false },
		QuotaPersist: func() error { t.Error("Persist on disabled tracker"); return nil },
	})
	if _, err := service.FreezeHealth("up"); err != nil {
		t.Errorf("FreezeHealth disabled tracker err = %v", err)
	}
}

func TestSetAndClearPin(t *testing.T) {
	expires := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	service := New(Ports{
		SetPin: func(route, provider string, ttl time.Duration) (time.Time, bool) {
			if provider != "up" || ttl != time.Minute {
				t.Errorf("SetPin args = %q %q %v", route, provider, ttl)
			}
			return expires, route == "m"
		},
		ClearPin: func(route string) bool { return route == "m" },
	})
	pin, ok := service.SetPin("m", "up", time.Minute)
	if !ok || pin.Route != "m" || pin.Provider != "up" || !pin.ExpiresAt.Equal(expires) {
		t.Errorf("SetPin = %+v %v", pin, ok)
	}
	if _, ok := service.SetPin("other", "up", time.Minute); ok {
		t.Error("unknown route must report false")
	}
	if !service.ClearPin("m") || service.ClearPin("other") {
		t.Error("ClearPin result mismatch")
	}
}

func TestValidateConfig(t *testing.T) {
	service := New(Ports{})
	if issues := service.ValidateConfig([]byte("listen: 127.0.0.1:0\nproviders:\n  up: {provider_id: zhipu, openai_base_url: https://x}\n")); len(issues) != 0 {
		t.Errorf("valid config issues = %v", issues)
	}
	issues := service.ValidateConfig([]byte("providers: ["))
	if len(issues) == 0 || issues[0].Message == "" {
		t.Errorf("invalid config issues = %v", issues)
	}
}

func TestAddAccountUnknownProvider(t *testing.T) {
	service := New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) { return configdomain.Provider{}, false },
	})
	_, err := service.AddAccount(context.Background(), "nope", appapi.AccountInput{APIKey: "k"})
	if httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("status = %d, want 404", httpErrorStatus(t, err))
	}
}

func TestAddAccountOAuthProviderRejected(t *testing.T) {
	service := New(Ports{
		ProviderConfig: func(name string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "aqp"}, true
		},
	})
	_, err := service.AddAccount(context.Background(), "aqp", appapi.AccountInput{APIKey: "k"})
	if httpErrorStatus(t, err) != http.StatusBadRequest ||
		!strings.Contains(err.Error(), "POST /api/login/aqp/start") {
		t.Errorf("err = %v", err)
	}
}

func TestAddAccountApikeySuccess(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{}
	service := New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "zhipu"}, true
		},
		Config:     func() *configdomain.Config { return &configdomain.Config{} },
		ConfigFile: func() string { return "config.yaml" },
		Reload:     spy.fn(),
	})
	result, err := service.AddAccount(context.Background(), "zhipu", appapi.AccountInput{
		APIKey: "test-key",
		Label:  "first",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == "" || result.Warning != "" {
		t.Errorf("result = %+v", result)
	}
	if len(spy.calls) != 1 || spy.calls[0] != "config.yaml" {
		t.Errorf("reload calls = %v", spy.calls)
	}
	pool, err := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if err != nil || len(pool.Accounts) != 1 || pool.Accounts[0].ID != result.ID ||
		pool.Accounts[0].Label != "first" {
		t.Errorf("persisted pool = %+v, %v", pool, err)
	}
}

func TestAddAccountValidationFailure(t *testing.T) {
	setTestHome(t)
	service := New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "zhipu"}, true
		},
		Config: func() *configdomain.Config { return &configdomain.Config{} },
		Reload: func(string) error {
			t.Error("failed add must not reload")
			return nil
		},
	})
	_, err := service.AddAccount(context.Background(), "zhipu", appapi.AccountInput{})
	if httpErrorStatus(t, err) != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", httpErrorStatus(t, err))
	}
}

func TestAddAccountReloadWarningSurfaces(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{err: errors.New("config rejected")}
	service := New(Ports{
		ProviderConfig: func(string) (configdomain.Provider, bool) {
			return configdomain.Provider{Provider: "zhipu"}, true
		},
		Config:     func() *configdomain.Config { return &configdomain.Config{} },
		ConfigFile: func() string { return "config.yaml" },
		Reload:     spy.fn(),
	})
	result, err := service.AddAccount(context.Background(), "zhipu", appapi.AccountInput{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Warning != "config rejected" {
		t.Errorf("warning = %q, want the reload error message", result.Warning)
	}
}

func TestRemoveAccount(t *testing.T) {
	setTestHome(t)
	spy := &reloadSpy{}
	service := New(Ports{
		ProviderConfig: func(name string) (configdomain.Provider, bool) {
			switch name {
			case "zhipu":
				return configdomain.Provider{Provider: "zhipu"}, true
			case "aqp":
				return configdomain.Provider{Provider: "aqp"}, true
			case "codex":
				return configdomain.Provider{Provider: "codex"}, true
			default:
				return configdomain.Provider{}, false
			}
		},
		ConfigFile: func() string { return "config.yaml" },
		Reload:     spy.fn(),
	})

	if _, err := service.RemoveAccount("nope", "x"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("unknown provider status = %d", httpErrorStatus(t, err))
	}

	apiKeyID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "k"})
	if err := accounts.NewStore(accounts.HomeDir()).Save("zhipu", "zhipu", accounts.Pool{Accounts: []accounts.Account{
		{ID: apiKeyID, APIKey: "k"},
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := service.RemoveAccount("zhipu", apiKeyID)
	if err != nil || result.Warning != "" {
		t.Fatalf("RemoveAccount = %+v, %v", result, err)
	}
	pool, _ := accounts.NewStore(accounts.HomeDir()).Load("zhipu", "zhipu")
	if len(pool.Accounts) != 0 {
		t.Errorf("pool after remove = %+v", pool)
	}

	// aqp removal clears the OAuth auth file.
	if err := provider.SaveAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"), &provider.AqpAccountData{
		AccountID: "aqp-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemoveAccount("aqp", "aqp-id"); err != nil {
		t.Fatalf("RemoveAccount aqp: %v", err)
	}
	account, err := provider.LoadAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"))
	if err != nil || account != nil {
		t.Errorf("aqp account after remove = %+v, %v", account, err)
	}

	// codex removal clears its auth file too.
	codexAuth := &provider.CodexAuthFile{}
	codexAuth.Tokens.AccountID = "codex-id"
	if err := provider.WriteCodexAuthFile(accounts.AuthFilePath("codex", "oauth_auth"), codexAuth); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RemoveAccount("codex", "codex-id"); err != nil {
		t.Fatalf("RemoveAccount codex: %v", err)
	}
	codexAccount, err := provider.LoadCodexAccount(accounts.AuthFilePath("codex", "oauth_auth"))
	if err != nil || codexAccount != nil {
		t.Errorf("codex account after remove = %+v, %v", codexAccount, err)
	}

	if len(spy.calls) != 3 {
		t.Errorf("reload calls = %v, want one per successful removal", spy.calls)
	}
}

type fakeProbeImpl struct{}

func (fakeProbeImpl) AuthHeaders(*http.Request) error { return nil }
func (fakeProbeImpl) Refresh() error                  { return nil }
func (fakeProbeImpl) RewriteRequest(targetURL string, body []byte, _ string) (string, []byte) {
	return targetURL, body
}
func (fakeProbeImpl) Logout() error                           { return nil }
func (fakeProbeImpl) Usage() error                            { return nil }
func (fakeProbeImpl) FetchModels() ([]string, error)          { return nil, nil }
func (fakeProbeImpl) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (fakeProbeImpl) ExtraHeaders(*http.Request, string)      {}
func (fakeProbeImpl) FilterModelIDs(ids []string) ([]string, []string) {
	return ids, nil
}
func (fakeProbeImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Path: "/chat/completions", Body: []byte(`{"model":"` + modelID + `"}`)}
}

func probePorts(cfg *configdomain.Config, providers map[string]provider.Provider) Ports {
	return Ports{ProbeRuntime: func() (*configdomain.Config, map[string]provider.Provider) {
		return cfg, providers
	}}
}

func TestProbeAccountErrorClassification(t *testing.T) {
	setTestHome(t)

	// Nil config: unknown provider.
	service := New(probePorts(nil, nil))
	if _, err := service.ProbeAccount(context.Background(), "up", "id"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("nil cfg status = %d", httpErrorStatus(t, err))
	}

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"up":      {Provider: "zhipu", Models: []string{"m"}},
		"nomodel": {Provider: "zhipu"},
	}}
	accountID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "k"})
	for _, name := range []string{"up", "nomodel"} {
		if err := accounts.NewStore(accounts.HomeDir()).Save(name, "zhipu", accounts.Pool{Accounts: []accounts.Account{
			{ID: accountID, APIKey: "k"},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	service = New(probePorts(cfg, map[string]provider.Provider{}))

	if _, err := service.ProbeAccount(context.Background(), "missing", accountID); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("unknown provider status = %d", httpErrorStatus(t, err))
	}
	if _, err := service.ProbeAccount(context.Background(), "up", "unknown-id"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("unknown account status = %d", httpErrorStatus(t, err))
	}
	_, err := service.ProbeAccount(context.Background(), "nomodel", accountID)
	if httpErrorStatus(t, err) != http.StatusBadRequest {
		t.Errorf("missing model status = %d, want 400", httpErrorStatus(t, err))
	}
	if _, err := service.ProbeAccount(context.Background(), "up", accountID); httpErrorStatus(t, err) != http.StatusNotFound ||
		!strings.Contains(err.Error(), "not available") {
		t.Errorf("unavailable impl err = %v", err)
	}
}

func TestProbeAccountPoolVirtualKey(t *testing.T) {
	setTestHome(t)
	accountA := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "ka"})
	if err := accounts.NewStore(accounts.HomeDir()).Save("up", "zhipu", accounts.Pool{Accounts: []accounts.Account{
		{ID: accountA, APIKey: "ka"},
		{ID: accounts.AccountID("zhipu", accounts.Credentials{APIKey: "kb"}), APIKey: "kb"},
	}}); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"up": {Provider: "zhipu", Models: []string{"m"}},
	}}
	service := New(probePorts(cfg, map[string]provider.Provider{}))
	_, err := service.ProbeAccount(context.Background(), "up", accountA)
	if httpErrorStatus(t, err) != http.StatusNotFound ||
		!strings.Contains(err.Error(), "up#"+accountA+" not available") {
		t.Errorf("multi-account probe must resolve the virtual key: %v", err)
	}
}

func TestProbeAccountOAuthIDMismatch(t *testing.T) {
	setTestHome(t)
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"aqp": {Provider: "aqp"},
	}}
	service := New(probePorts(cfg, nil))
	if _, err := service.ProbeAccount(context.Background(), "aqp", "nope"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("aqp mismatch status = %d", httpErrorStatus(t, err))
	}
	if err := provider.SaveAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"), &provider.AqpAccountData{
		AccountID: "aqp-id",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ProbeAccount(context.Background(), "aqp", "other"); httpErrorStatus(t, err) != http.StatusNotFound {
		t.Errorf("aqp stored-id mismatch status = %d", httpErrorStatus(t, err))
	}
}

func TestProbeAccountSuccess(t *testing.T) {
	setTestHome(t)
	accountID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "k"})
	if err := accounts.NewStore(accounts.HomeDir()).Save("up", "zhipu", accounts.Pool{Accounts: []accounts.Account{
		{ID: accountID, APIKey: "k"},
	}}); err != nil {
		t.Fatal(err)
	}
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		gotModel = r.Header.Get("Content-Type")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer upstream.Close()

	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"up": {Provider: "zhipu", OpenAIBaseURL: upstream.URL, Models: []string{"configured-model"}},
	}}
	service := New(probePorts(cfg, map[string]provider.Provider{"up": fakeProbeImpl{}}))
	result, err := service.ProbeAccount(context.Background(), "up", accountID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.HTTPStatus != http.StatusOK || result.Provider != "up" ||
		result.AccountID != accountID || result.Model != "configured-model" || result.Latency < 0 {
		t.Errorf("probe result = %+v", result)
	}
	if gotModel != "application/json" {
		t.Errorf("upstream content-type = %q", gotModel)
	}
}

func TestReloadAfterMutationWarningShapes(t *testing.T) {
	service := New(Ports{
		ConfigFile: func() string { return "config.yaml" },
		Reload:     func(string) error { return nil },
	})
	if warning := service.reloadAfterMutation(); warning != "" {
		t.Errorf("successful reload warning = %q", warning)
	}

	service = New(Ports{
		ConfigFile: func() string { return "config.yaml" },
		Reload: func(string) error {
			return &ReloadAppliedWarning{Err: errors.New("quota persist failed")}
		},
	})
	if warning := service.reloadAfterMutation(); warning != "reload applied with warning: quota persist failed" {
		t.Errorf("applied warning = %q", warning)
	}

	service = New(Ports{
		ConfigFile: func() string { return "config.yaml" },
		Reload:     func(string) error { return errors.New("config rejected") },
	})
	if warning := service.reloadAfterMutation(); warning != "config rejected" {
		t.Errorf("plain failure warning = %q", warning)
	}
}

func TestReloadAppliedWarningErrorShape(t *testing.T) {
	err := &ReloadAppliedWarning{Err: errors.New("inner")}
	if err.Error() != "reload applied with warning: inner" {
		t.Errorf("message = %q", err.Error())
	}
	if !errors.Is(err, err.Err) {
		t.Error("Unwrap must expose the inner error")
	}
}

func TestAddPreset(t *testing.T) {
	path := writeTestConfig(t)
	spy := &reloadSpy{}
	service := New(Ports{
		ConfigFile: func() string { return path },
		Reload:     spy.fn(),
	})
	warnings, reloadWarning, err := service.AddPreset("zhipu")
	if err != nil {
		t.Fatal(err)
	}
	if reloadWarning != "" {
		t.Errorf("reloadWarning = %q", reloadWarning)
	}
	if len(spy.calls) != 1 || spy.calls[0] != path {
		t.Errorf("reload calls = %v", spy.calls)
	}
	_ = warnings // ambiguity model names depend on the merged catalog shape
	merged, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(merged), "zhipu") {
		t.Errorf("merged config missing preset block: %v %s", err, merged)
	}

	if _, _, err := service.AddPreset("no-such-preset"); err == nil ||
		!strings.Contains(err.Error(), `unknown preset "no-such-preset"`) {
		t.Errorf("unknown preset err = %v", err)
	}
}

// TestSetModelDisabledValidation pins the fail-closed gate: the provider must
// exist in the current config and the model must belong to its served set
// (models: list ∪ explicit route targets — the probe matrix's own universe);
// only then does the port fire with the exact triple.
func TestSetModelDisabledValidation(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Models: []string{"glm-4.6", "glm-4.7"}},
			"kimi":  {},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"routed": {{Provider: "kimi", Model: "k3"}},
		},
	}
	var gotProvider, gotModel string
	var gotDisabled bool
	service := New(Ports{
		Config: func() *configdomain.Config { return cfg },
		SetModelDisabled: func(provider, model string, disabled bool) error {
			gotProvider, gotModel, gotDisabled = provider, model, disabled
			return nil
		},
	})

	for _, tc := range []struct {
		provider, model string
		disabled        bool
		wantErr         string
	}{
		{"", "glm-4.6", true, "provider and model are required"},
		{"zhipu", "", true, "provider and model are required"},
		{"missing", "glm-4.6", true, `unknown provider "missing"`},
		{"zhipu", "nope", true, `provider "zhipu" does not serve model "nope"`},
		{"kimi", "nope", false, `provider "kimi" does not serve model "nope"`},
	} {
		err := service.SetModelDisabled(tc.provider, tc.model, tc.disabled)
		if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("SetModelDisabled(%q,%q,%v) err = %v, want %q", tc.provider, tc.model, tc.disabled, err, tc.wantErr)
		}
	}
	if gotProvider != "" {
		t.Fatalf("port fired for an invalid pair (%s/%s)", gotProvider, gotModel)
	}

	// models: member and explicit route target both pass; nil port is refused.
	if err := service.SetModelDisabled("zhipu", "glm-4.7", true); err != nil {
		t.Fatalf("models: member rejected: %v", err)
	}
	if err := service.SetModelDisabled("kimi", "k3", false); err != nil {
		t.Fatalf("route target rejected: %v", err)
	}
	if gotProvider != "kimi" || gotModel != "k3" || gotDisabled {
		t.Fatalf("port saw provider=%q model=%q disabled=%v, want kimi/k3/false", gotProvider, gotModel, gotDisabled)
	}
	if err := (New(Ports{Config: func() *configdomain.Config { return cfg }})).SetModelDisabled("zhipu", "glm-4.6", true); err == nil {
		t.Fatal("nil SetModelDisabled port must be refused, not panic")
	}
}

// TestSetModelDisabledPersistError pins the transport classification of the
// "memory applied, persist failed" path: unlike the validation rejections
// (plain errors → 400), the persist failure is an appapi.HTTPError with 500
// and the stable "toggle applied in memory but persisting it failed: "
// prefix — the web transport and the Web UI's switch handling both branch
// on that pair.
func TestSetModelDisabledPersistError(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Models: []string{"glm-4.7"}},
		},
	}
	service := New(Ports{
		Config: func() *configdomain.Config { return cfg },
		SetModelDisabled: func(provider, model string, disabled bool) error {
			return errors.New("write state")
		},
	})
	err := service.SetModelDisabled("zhipu", "glm-4.7", true)
	if err == nil {
		t.Fatal("persist error must surface, not be swallowed")
	}
	var classified *appapi.HTTPError
	if !errors.As(err, &classified) || classified.Status != http.StatusInternalServerError {
		t.Fatalf("persist error must classify as HTTPError 500, got %T %v", err, err)
	}
	if want := "toggle applied in memory but persisting it failed: write state"; err.Error() != want {
		t.Fatalf("error=%q want %q", err.Error(), want)
	}
}
