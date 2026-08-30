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

	"model-proxy/internal/appapi"
	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/presets"
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

func (testReadAPI) Dashboard(time.Time) appapi.Dashboard                   { return appapi.Dashboard{} }
func (testReadAPI) LogFile() string                                        { return "" }
func (testReadAPI) RequestLogDirectory() string                            { return "" }
func (testReadAPI) Accounts() []appapi.ProviderAccounts                    { return nil }
func (testReadAPI) Tokens() []appapi.TokenUsage                            { return nil }
func (testReadAPI) Stats(appapi.StatsQuery) ([]observestats.Bucket, error) { return nil, nil }
func (testReadAPI) AgentStats(appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
	return nil, nil
}
func (testReadAPI) Analytics(appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	return nil, nil
}
func (testReadAPI) Pricing() appapi.PricingSnapshot {
	return appapi.PricingSnapshot{Catalog: &pricing.Catalog{}}
}
func (testReadAPI) Fusion(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return nil, nil
}
func (testReadAPI) Pins() []appapi.Pin { return nil }
func (testReadAPI) Security(appapi.SecurityQuery) (appapi.SecurityResult, error) {
	return appapi.SecurityResult{}, nil
}
func (testReadAPI) ConfigDocument() (appapi.ConfigDocument, error) {
	return appapi.ConfigDocument{}, nil
}

type testCommandAPI struct {
	begin func(context.Context, string) (appapi.LoginStart, error)
}

func (testCommandAPI) ResetStats() error                         { return nil }
func (testCommandAPI) RefreshQuota(string) bool                  { return true }
func (testCommandAPI) ResetHealth(string) ([]string, int, error) { return nil, 0, nil }
func (testCommandAPI) SetPin(string, string, time.Duration) (appapi.Pin, bool) {
	return appapi.Pin{}, true
}
func (testCommandAPI) ClearPin(string) bool    { return true }
func (testCommandAPI) SaveConfig([]byte) error { return nil }
func (testCommandAPI) ValidateConfig([]byte) []appapi.ValidationIssue {
	return nil
}
func (testCommandAPI) EditConfig(appapi.EditRequest) error { return nil }
func (testCommandAPI) AddAccount(context.Context, string, appapi.AccountInput) (appapi.MutationResult, error) {
	return appapi.MutationResult{}, nil
}
func (testCommandAPI) ProbeAccount(context.Context, string, string) (appapi.ProbeResult, error) {
	return appapi.ProbeResult{}, nil
}
func (testCommandAPI) RemoveAccount(string, string) (appapi.MutationResult, error) {
	return appapi.MutationResult{}, nil
}
func (api testCommandAPI) BeginLogin(ctx context.Context, name string) (appapi.LoginStart, error) {
	if api.begin == nil {
		return appapi.LoginStart{}, errors.New("not implemented")
	}
	return api.begin(ctx, name)
}

type completedLogin struct{}

func (completedLogin) Run(context.Context) appapi.LoginUpdate {
	return appapi.LoginUpdate{State: "done", Result: "account"}
}

func TestServeUITraversalGuard(t *testing.T) {
	assets := &permissiveAssetFS{}
	s, err := New(Options{Reads: testReadAPI{}, Commands: testCommandAPI{begin: func(context.Context, string) (appapi.LoginStart, error) {
		return appapi.LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: completedLogin{}}, nil
	}}, Assets: assets})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad := httptest.NewRecorder()
	// serveUI itself must reject the traversal before touching the FS — going
	// through the mux would let ServeMux's path cleaning redirect first, which
	// is a different layer than the one this test pins.
	s.serveUI(bad, httptest.NewRequest(http.MethodGet, "/ui/../secret", nil))
	if bad.Code != http.StatusNotFound {
		t.Fatalf("traversal status=%d want 404", bad.Code)
	}
	if len(assets.opened) != 0 {
		t.Fatalf("traversal request reached asset filesystem: %q", assets.opened)
	}
	good := httptest.NewRecorder()
	serveWebRequest(s, good, httptest.NewRequest(http.MethodGet, "/ui/app.js", nil))
	if good.Code != http.StatusOK || good.Body.String() != "SECRET" {
		t.Fatalf("normal asset status=%d body=%q", good.Code, good.Body.String())
	}
	if got, want := assets.opened, []string{"assets/app.js"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("normal asset Open paths=%q want %q", got, want)
	}
	if got := good.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("asset Cache-Control=%q want no-cache", got)
	}
	if got := good.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("asset X-Content-Type-Options=%q want nosniff", got)
	}
}

func TestServeUICacheHeadersOnIndex(t *testing.T) {
	s, err := New(Options{
		Reads: testReadAPI{},
		Commands: testCommandAPI{begin: func(context.Context, string) (appapi.LoginStart, error) {
			return appapi.LoginStart{}, errors.New("not implemented")
		}},
		Assets: fstest.MapFS{"assets/index.html": {Data: []byte("ok")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recorder := httptest.NewRecorder()
	serveWebRequest(s, recorder, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("index status=%d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("index Cache-Control=%q want no-cache", got)
	}
	if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("index X-Content-Type-Options=%q want nosniff", got)
	}
}

func TestServerLoginTransport(t *testing.T) {
	s, err := New(Options{
		Reads: testReadAPI{},
		Commands: testCommandAPI{begin: func(context.Context, string) (appapi.LoginStart, error) {
			return appapi.LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: completedLogin{}}, nil
		}},
		Assets: fstest.MapFS{"assets/index.html": {Data: []byte("ok")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := httptest.NewRecorder()
	serveWebRequest(s, start, httptest.NewRequest(http.MethodPost, "/api/login/aqp/start", nil))
	if start.Code != http.StatusOK {
		t.Fatalf("login start status=%d body=%s", start.Code, start.Body.String())
	}
}

func (testCommandAPI) AddPreset(string) ([]string, string, error) { return nil, "", nil }

func (testReadAPI) Presets() []presets.Preset { return nil }
