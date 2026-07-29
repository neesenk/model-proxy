package web

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

// permissiveAssetFS deliberately accepts every Open path. It makes the
// traversal test prove serveUI rejects before relying on an FS implementation.
type permissiveAssetFS struct{ opened []string }

func (assets *permissiveAssetFS) Open(name string) (fs.File, error) {
	assets.opened = append(assets.opened, name)
	return fstest.MapFS{"asset": {Data: []byte("SECRET")}}.Open("asset")
}

type testReadAPI struct{}

func (testReadAPI) Dashboard(time.Time) Dashboard                                    { return Dashboard{} }
func (testReadAPI) LogFile() string                                                  { return "" }
func (testReadAPI) RequestLogDirectory() string                                      { return "" }
func (testReadAPI) Accounts() []ProviderAccounts                                     { return nil }
func (testReadAPI) Tokens() []TokenUsage                                             { return nil }
func (testReadAPI) Stats(StatsQuery) ([]observestats.Bucket, error)                  { return nil, nil }
func (testReadAPI) AgentStats(AgentStatsQuery) ([]observestats.AgentBucket, error)   { return nil, nil }
func (testReadAPI) Analytics(AnalyticsQuery) ([]observestats.AnalyticsBucket, error) { return nil, nil }
func (testReadAPI) Pricing() PricingSnapshot                                         { return PricingSnapshot{Catalog: &pricing.Catalog{}} }
func (testReadAPI) Fusion(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return nil, nil
}
func (testReadAPI) Pins() []Pin                             { return nil }
func (testReadAPI) ConfigDocument() (ConfigDocument, error) { return ConfigDocument{}, nil }

type testCommandAPI struct {
	begin func(context.Context, string) (LoginStart, error)
}

func (testCommandAPI) ResetStats() error                                { return nil }
func (testCommandAPI) RefreshQuota(string) bool                         { return true }
func (testCommandAPI) ResetHealth(string) ([]string, int, error)        { return nil, 0, nil }
func (testCommandAPI) SetPin(string, string, time.Duration) (Pin, bool) { return Pin{}, true }
func (testCommandAPI) ClearPin(string) bool                             { return true }
func (testCommandAPI) SaveConfig([]byte) error                          { return nil }
func (testCommandAPI) EditConfig(EditRequest) error                     { return nil }
func (testCommandAPI) AddAccount(context.Context, string, AccountInput) (MutationResult, error) {
	return MutationResult{}, nil
}
func (testCommandAPI) ProbeAccount(context.Context, string, string) (ProbeResult, error) {
	return ProbeResult{}, nil
}
func (testCommandAPI) RemoveAccount(string, string) (MutationResult, error) {
	return MutationResult{}, nil
}
func (api testCommandAPI) BeginLogin(ctx context.Context, name string) (LoginStart, error) {
	if api.begin == nil {
		return LoginStart{}, errors.New("not implemented")
	}
	return api.begin(ctx, name)
}

type completedLogin struct{}

func (completedLogin) Run(context.Context) LoginUpdate {
	return LoginUpdate{State: "done", Result: "account"}
}

func TestServeUITraversalGuard(t *testing.T) {
	assets := &permissiveAssetFS{}
	s, err := New(Options{Reads: testReadAPI{}, Commands: testCommandAPI{begin: func(context.Context, string) (LoginStart, error) {
		return LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: completedLogin{}}, nil
	}}, Assets: assets})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad := httptest.NewRecorder()
	s.ServeHTTP(bad, httptest.NewRequest(http.MethodGet, "/ui/../secret", nil))
	if bad.Code != http.StatusNotFound {
		t.Fatalf("traversal status=%d want 404", bad.Code)
	}
	if len(assets.opened) != 0 {
		t.Fatalf("traversal request reached asset filesystem: %q", assets.opened)
	}
	good := httptest.NewRecorder()
	s.ServeHTTP(good, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))
	if good.Code != http.StatusOK || good.Body.String() != "SECRET" {
		t.Fatalf("normal asset status=%d body=%q", good.Code, good.Body.String())
	}
	if got, want := assets.opened, []string{"assets/app.js"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("normal asset Open paths=%q want %q", got, want)
	}
}

func TestServerLoginTransport(t *testing.T) {
	s, err := New(Options{
		Reads: testReadAPI{},
		Commands: testCommandAPI{begin: func(context.Context, string) (LoginStart, error) {
			return LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: completedLogin{}}, nil
		}},
		Assets: fstest.MapFS{"assets/index.html": {Data: []byte("ok")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := httptest.NewRecorder()
	s.ServeHTTP(start, httptest.NewRequest(http.MethodPost, "/api/login/aqp/start", nil))
	if start.Code != http.StatusOK {
		t.Fatalf("login start status=%d body=%s", start.Code, start.Body.String())
	}
}
