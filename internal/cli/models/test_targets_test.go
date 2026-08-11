package models

import (
	"testing"

	"model-proxy/internal/app"

	configdomain "model-proxy/internal/config"
)

// --- testTargetsFor (in-process): explicit sort, implicit fallback, miss ---

func TestTestTargetsFor(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	writeTestPool(t, "zhipu", "zhipu", "KEY-A")

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu", Models: []string{"glm-implicit"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm-explicit": {
				{Provider: "zhipu", Model: "glm-5.2", Priority: 2},
				{Provider: "zhipu", Model: "glm-5.1", Priority: 1},
			},
		},
	}

	// Explicit route: sorted by priority asc.
	got := testTargetsFor(cfg, "glm-explicit")
	if len(got) != 2 || got[0].Model != "glm-5.1" || got[1].Model != "glm-5.2" {
		t.Errorf("explicit targets=%+v want priority-sorted [glm-5.1 glm-5.2]", got)
	}

	// No explicit route: implicit fallback (logged-in provider serves the model).
	got = testTargetsFor(cfg, "glm-implicit")
	if len(got) != 1 || got[0] != (configdomain.RouteTarget{Provider: "zhipu", Model: "glm-implicit", Priority: 1}) {
		t.Errorf("implicit targets=%+v want single zhipu target", got)
	}

	// Neither explicit nor implicit: nil.
	if got := testTargetsFor(cfg, "nope"); got != nil {
		t.Errorf("unrouted model targets=%+v want nil", got)
	}
}

// writeTestPool writes a one-account plural pool for implicit-route tests.
func writeTestPool(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	pool := app.CredentialPool{Version: 1}
	for _, key := range keys {
		pool.Accounts = append(pool.Accounts, app.PoolAccount{
			ID:    app.AccountIDFor(providerID, app.AccountCred{APIKey: key}),
			Label: key, APIKey: key, AddedAt: "2026-07-08",
		})
	}
	if err := app.SavePool(name, providerID, pool); err != nil {
		t.Fatal(err)
	}
}
