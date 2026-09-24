package web

import (
	"context"
	"encoding/json"
	"errors"
	"model-proxy/internal/appapi"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type commandFake struct {
	mu sync.Mutex

	resetStats     func() error
	refresh        func(string) bool
	resetHealth    func(string) ([]string, int, error)
	freezeHealth   func(string) ([]string, error)
	setPin         func(string, string, time.Duration) (appapi.Pin, bool)
	unblock        func(string) error
	clearPin       func(string) bool
	save           func([]byte) error
	validate       func([]byte) []appapi.ValidationIssue
	edit           func(appapi.EditRequest) error
	add            func(context.Context, string, appapi.AccountInput) (appapi.MutationResult, error)
	probe          func(context.Context, string, string) (appapi.ProbeResult, error)
	remove         func(string, string) (appapi.MutationResult, error)
	begin          func(context.Context, string) (appapi.LoginStart, error)
	refreshMdls    func(context.Context, string) (appapi.ModelsRefreshResult, error)
	setModelDisabl func(string, string, bool) error
	probeMCP       func(context.Context, string) (appapi.MCPProbeResult, error)
	takeoverRun    func(appapi.TakeoverRunRequest) (appapi.TakeoverRunResult, error)
	takeoverRest   func(string) (appapi.TakeoverRestoreResult, error)
	templateSave   func(string, []byte) error
	templateDel    func(string) error
	replay         func(context.Context, string, string) (appapi.ReplayResult, error)
	routeTest      func(context.Context, string) (appapi.RouteTestResult, error)
	catalogPull    func(context.Context) (appapi.ModelsCatalogPull, error)
}

func (fake *commandFake) ResetStats() error {
	if fake.resetStats != nil {
		return fake.resetStats()
	}
	return nil
}

func (fake *commandFake) RefreshQuota(provider string) bool {
	if fake.refresh != nil {
		return fake.refresh(provider)
	}
	return true
}

func (fake *commandFake) ResetHealth(provider string) ([]string, int, error) {
	if fake.resetHealth != nil {
		return fake.resetHealth(provider)
	}
	return nil, 0, nil
}

func (fake *commandFake) FreezeHealth(provider string) ([]string, error) {
	if fake.freezeHealth != nil {
		return fake.freezeHealth(provider)
	}
	return nil, nil
}

func (fake *commandFake) SetPin(route, provider string, ttl time.Duration) (appapi.Pin, bool) {
	if fake.setPin != nil {
		return fake.setPin(route, provider, ttl)
	}
	return appapi.Pin{}, true
}

func (fake *commandFake) SecurityAllowed() []appapi.SecurityAllowed { return nil }
func (fake *commandFake) SecurityDisallow(hash string) error        { return nil }
func (fake *commandFake) SecurityUnblock(sessionID string) error {
	if fake.unblock != nil {
		return fake.unblock(sessionID)
	}
	return nil
}
func (fake *commandFake) ClearPin(route string) bool {
	if fake.clearPin != nil {
		return fake.clearPin(route)
	}
	return true
}

func (fake *commandFake) SaveConfig(contents []byte) error {
	if fake.save != nil {
		return fake.save(contents)
	}
	return nil
}

func (fake *commandFake) ValidateConfig(contents []byte) []appapi.ValidationIssue {
	if fake.validate != nil {
		return fake.validate(contents)
	}
	return nil
}

func (fake *commandFake) EditConfig(req appapi.EditRequest) error {
	if fake.edit != nil {
		return fake.edit(req)
	}
	return nil
}

func (fake *commandFake) AddAccount(ctx context.Context, provider string, input appapi.AccountInput) (appapi.MutationResult, error) {
	if fake.add != nil {
		return fake.add(ctx, provider, input)
	}
	return appapi.MutationResult{}, nil
}

func (fake *commandFake) ProbeAccount(ctx context.Context, provider, id string) (appapi.ProbeResult, error) {
	if fake.probe != nil {
		return fake.probe(ctx, provider, id)
	}
	return appapi.ProbeResult{}, nil
}

func (fake *commandFake) RemoveAccount(provider, id string) (appapi.MutationResult, error) {
	if fake.remove != nil {
		return fake.remove(provider, id)
	}
	return appapi.MutationResult{}, nil
}

func (fake *commandFake) BeginLogin(ctx context.Context, provider string) (appapi.LoginStart, error) {
	if fake.begin != nil {
		return fake.begin(ctx, provider)
	}
	return appapi.LoginStart{}, nil
}

func (fake *commandFake) RunTakeover(req appapi.TakeoverRunRequest) (appapi.TakeoverRunResult, error) {
	if fake.takeoverRun != nil {
		return fake.takeoverRun(req)
	}
	return appapi.TakeoverRunResult{}, nil
}

func (fake *commandFake) RestoreTakeover(client string) (appapi.TakeoverRestoreResult, error) {
	if fake.takeoverRest != nil {
		return fake.takeoverRest(client)
	}
	return appapi.TakeoverRestoreResult{}, nil
}

func (fake *commandFake) SaveTakeoverTemplate(name string, yaml []byte) error {
	if fake.templateSave != nil {
		return fake.templateSave(name, yaml)
	}
	return nil
}

func (fake *commandFake) DeleteTakeoverTemplate(name string) error {
	if fake.templateDel != nil {
		return fake.templateDel(name)
	}
	return nil
}

func (fake *commandFake) Replay(ctx context.Context, id, provider string) (appapi.ReplayResult, error) {
	if fake.replay != nil {
		return fake.replay(ctx, id, provider)
	}
	return appapi.ReplayResult{}, nil
}

func (fake *commandFake) TestRoute(ctx context.Context, model string) (appapi.RouteTestResult, error) {
	if fake.routeTest != nil {
		return fake.routeTest(ctx, model)
	}
	return appapi.RouteTestResult{}, nil
}

func (fake *commandFake) PullModelsCatalog(ctx context.Context) (appapi.ModelsCatalogPull, error) {
	if fake.catalogPull != nil {
		return fake.catalogPull(ctx)
	}
	return appapi.ModelsCatalogPull{}, nil
}

func newCommandTestServer(t *testing.T, commands *commandFake) *Server {
	t.Helper()
	server, err := New(Options{Reads: testReadAPI{}, Commands: commands})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	return server
}

func commandRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	// serveAPI is the handler Register installs at /api/; call it directly so
	// malformed paths ("///", "//start") reach serveAPI's own path parsing —
	// a ServeMux would clean-redirect them before dispatch.
	server.serveAPI(recorder, request)
	return recorder
}

func commandJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if got := recorder.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type=%q want application/json", got)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode JSON %q: %v", recorder.Body.String(), err)
	}
	return body
}

func requireCommandResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, want map[string]any) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status=%d want %d body=%s", recorder.Code, status, recorder.Body.String())
	}
	got := commandJSON(t, recorder)
	if len(got) != len(want) {
		t.Fatalf("body=%v want exactly %v", got, want)
	}
	for key, value := range want {
		if !reflect.DeepEqual(got[key], value) {
			t.Fatalf("body[%q]=%#v want %#v; body=%v", key, got[key], value, got)
		}
	}
}

func TestCommandTokensResetContract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		calls := 0
		server := newCommandTestServer(t, &commandFake{resetStats: func() error {
			calls++
			return nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/tokens/reset", ""), http.StatusOK, map[string]any{"status": "reset"})
		if calls != 1 {
			t.Fatalf("ResetStats calls=%d want 1", calls)
		}
	})
	t.Run("error", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{resetStats: func() error { return errors.New("sqlite unavailable") }})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/tokens/reset", ""), http.StatusInternalServerError, map[string]any{"error": "sqlite unavailable"})
	})
}

func TestCommandModelsRefreshContract(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		var gotProvider string
		server := newCommandTestServer(t, &commandFake{refreshMdls: func(_ context.Context, provider string) (appapi.ModelsRefreshResult, error) {
			gotProvider = provider
			return appapi.ModelsRefreshResult{
				Provider:      provider,
				Kept:          []string{"glm"},
				Added:         []string{"glm"},
				Removed:       []string{},
				PolicyDropped: []string{},
				ProbeDropped:  []appapi.ModelsRefreshDrop{},
				ConfigUpdated: true,
			}, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/refresh", `{"provider":"zhipu"}`), http.StatusOK, map[string]any{
			"provider":       "zhipu",
			"kept":           []any{"glm"},
			"added":          []any{"glm"},
			"removed":        []any{},
			"policy_dropped": []any{},
			"probe_dropped":  []any{},
			"config_updated": true,
		})
		if gotProvider != "zhipu" {
			t.Fatalf("RefreshModels provider=%q want zhipu", gotProvider)
		}
	})
	t.Run("missing provider field", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/refresh", `{}`), http.StatusBadRequest, map[string]any{"error": "provider is required"})
	})
	t.Run("malformed body", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{})
		rec := commandRequest(server, http.MethodPost, "/api/models/refresh", "not-json")
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("malformed body status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
	t.Run("command error maps to port status", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{refreshMdls: func(context.Context, string) (appapi.ModelsRefreshResult, error) {
			return appapi.ModelsRefreshResult{}, appapi.NewHTTPError(http.StatusNotFound, "unknown provider: ghost")
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/refresh", `{"provider":"ghost"}`), http.StatusNotFound, map[string]any{"error": "unknown provider: ghost"})
	})
}

func TestCommandQuotaRefreshContract(t *testing.T) {
	var providers []string
	server := newCommandTestServer(t, &commandFake{refresh: func(provider string) bool {
		providers = append(providers, provider)
		return provider != "missing"
	}})
	requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/quota/refresh", ""), http.StatusOK, map[string]any{"status": "refreshed"})
	requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/quota/refresh", `{"provider":"aqp#one"}`), http.StatusOK, map[string]any{"status": "refreshed", "provider": "aqp#one"})
	requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/quota/refresh", `{"provider":"missing"}`), http.StatusNotFound, map[string]any{"error": "unknown provider: missing"})
	// A malformed body is rejected without triggering any refresh — it must not
	// silently degrade into a full-network poll.
	malformed := commandRequest(server, http.MethodPost, "/api/quota/refresh", "not-json")
	if malformed.Code != http.StatusBadRequest {
		t.Fatalf("malformed body status=%d body=%s", malformed.Code, malformed.Body.String())
	}
	if errorText := commandJSON(t, malformed)["error"]; !strings.HasPrefix(errorText.(string), "malformed JSON body: ") {
		t.Fatalf("error=%q missing malformed prefix", errorText)
	}
	if got, want := strings.Join(providers, ","), ",aqp#one,missing"; got != want {
		t.Fatalf("RefreshQuota providers=%q want %q", got, want)
	}
	t.Run("pay-as-you-go provider is not quota-tracked", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{refresh: func(provider string) bool {
			return provider != "shopee"
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/quota/refresh", `{}`), http.StatusOK, map[string]any{"status": "refreshed"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/quota/refresh", `{"provider":"shopee"}`), http.StatusNotFound, map[string]any{"error": "unknown provider: shopee"})
	})
}

func TestCommandHealthResetContract(t *testing.T) {
	t.Run("empty body targets all", func(t *testing.T) {
		var provider string
		server := newCommandTestServer(t, &commandFake{resetHealth: func(got string) ([]string, int, error) {
			provider = got
			return []string{"aqp", "codex"}, 2, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/health/reset", ""), http.StatusOK, map[string]any{"cleared": []any{"aqp", "codex"}, "model_locks_cleared": float64(2)})
		if provider != "" {
			t.Fatalf("ResetHealth provider=%q want all target empty string", provider)
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{})
		recorder := commandRequest(server, http.MethodPost, "/api/health/reset", "{")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if errorText := commandJSON(t, recorder)["error"]; !strings.HasPrefix(errorText.(string), "malformed JSON body: ") {
			t.Fatalf("error=%q missing malformed prefix", errorText)
		}
	})
	t.Run("persist error preserves the stable prefix", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{resetHealth: func(string) ([]string, int, error) {
			return nil, 0, errors.New("write state")
		}})
		recorder := commandRequest(server, http.MethodPost, "/api/health/reset", `{"provider":"aqp"}`)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got, want := commandJSON(t, recorder)["error"], "state cleared in memory but persist failed: write state"; got != want {
			t.Fatalf("error=%q want %q", got, want)
		}
	})
}

func TestCommandHealthFreezeContract(t *testing.T) {
	t.Run("named provider freezes and reports matches", func(t *testing.T) {
		var provider string
		server := newCommandTestServer(t, &commandFake{freezeHealth: func(got string) ([]string, error) {
			provider = got
			return []string{"aqp", "aqp#one"}, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/health/freeze", `{"provider":"aqp"}`), http.StatusOK, map[string]any{"frozen": []any{"aqp", "aqp#one"}})
		if provider != "aqp" {
			t.Fatalf("FreezeHealth provider=%q want aqp", provider)
		}
	})
	t.Run("empty provider is rejected — no freeze-all", func(t *testing.T) {
		called := false
		server := newCommandTestServer(t, &commandFake{freezeHealth: func(string) ([]string, error) {
			called = true
			return nil, nil
		}})
		for _, body := range []string{"", `{}`, `{"provider":""}`} {
			recorder := commandRequest(server, http.MethodPost, "/api/health/freeze", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("body %q: status=%d body=%s, want 400", body, recorder.Code, recorder.Body.String())
			}
			if got := commandJSON(t, recorder)["error"]; got != "provider is required" {
				t.Errorf("body %q: error=%q, want the provider-required message", body, got)
			}
		}
		if called {
			t.Error("empty provider reached FreezeHealth — freeze-all must be rejected at the transport")
		}
	})
	t.Run("malformed body", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{})
		recorder := commandRequest(server, http.MethodPost, "/api/health/freeze", "{")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if errorText := commandJSON(t, recorder)["error"]; !strings.HasPrefix(errorText.(string), "malformed JSON body: ") {
			t.Fatalf("error=%q missing malformed prefix", errorText)
		}
	})
	t.Run("persist error preserves the stable prefix", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{freezeHealth: func(string) ([]string, error) {
			return nil, errors.New("write state")
		}})
		recorder := commandRequest(server, http.MethodPost, "/api/health/freeze", `{"provider":"aqp"}`)
		if recorder.Code != http.StatusInternalServerError {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if got, want := commandJSON(t, recorder)["error"], "state frozen in memory but persist failed: write state"; got != want {
			t.Fatalf("error=%q want %q", got, want)
		}
	})
}

func TestCommandPinContract(t *testing.T) {
	t.Run("set applies TTL and returns normalized expiry", func(t *testing.T) {
		var route, provider string
		var ttl time.Duration
		expires := time.Date(2026, time.July, 29, 3, 4, 5, 0, time.FixedZone("test", -7*60*60))
		server := newCommandTestServer(t, &commandFake{setPin: func(gotRoute, gotProvider string, gotTTL time.Duration) (appapi.Pin, bool) {
			route, provider, ttl = gotRoute, gotProvider, gotTTL
			if gotRoute == "permanent" {
				return appapi.Pin{}, true
			}
			return appapi.Pin{ExpiresAt: expires}, true
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/pin", `{"route":"fast","provider":"aqp","ttl_seconds":90}`), http.StatusOK, map[string]any{"route": "fast", "provider": "aqp", "expires_at": "2026-07-29T10:04:05Z", "status": "pinned"})
		if route != "fast" || provider != "aqp" || ttl != 90*time.Second {
			t.Fatalf("SetPin args=(%q,%q,%s) want (fast,aqp,1m30s)", route, provider, ttl)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/pin", `{"route":"permanent","provider":"aqp"}`), http.StatusOK, map[string]any{"route": "permanent", "provider": "aqp", "expires_at": "", "status": "pinned"})
		if ttl != 0 {
			t.Fatalf("SetPin default TTL=%s want 0", ttl)
		}
	})
	t.Run("validation and unavailable target", func(t *testing.T) {
		calls := 0
		server := newCommandTestServer(t, &commandFake{setPin: func(string, string, time.Duration) (appapi.Pin, bool) {
			calls++
			return appapi.Pin{}, false
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/pin", "{"), http.StatusBadRequest, map[string]any{"error": "parse pin body: unexpected EOF"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/pin", `{"route":""}`), http.StatusBadRequest, map[string]any{"error": "route and provider are required"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/pin", `{"route":"fast","provider":"missing","ttl_seconds":-1}`), http.StatusBadRequest, map[string]any{"error": "cannot pin \"fast\" to \"missing\": no such route, or the route has no target for that provider"})
		if calls != 1 {
			t.Fatalf("SetPin calls=%d want 1", calls)
		}
	})
	t.Run("clear requires route and returns exact removal result", func(t *testing.T) {
		var routes []string
		server := newCommandTestServer(t, &commandFake{clearPin: func(route string) bool {
			routes = append(routes, route)
			return route == "fast"
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/pin", ""), http.StatusBadRequest, map[string]any{"error": "route query param is required"})
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/pin?route=fast", ""), http.StatusOK, map[string]any{"route": "fast", "removed": true})
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/pin?route=gone", ""), http.StatusOK, map[string]any{"route": "gone", "removed": false})
		if got, want := strings.Join(routes, ","), "fast,gone"; got != want {
			t.Fatalf("ClearPin routes=%q want %q", got, want)
		}
	})
}

func TestCommandConfigContract(t *testing.T) {
	t.Run("put success parse and application errors", func(t *testing.T) {
		var saved []byte
		server := newCommandTestServer(t, &commandFake{save: func(contents []byte) error {
			saved = append([]byte(nil), contents...)
			if string(contents) == "bad: [" {
				return appapi.NewHTTPError(http.StatusUnprocessableEntity, "invalid YAML")
			}
			return nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config", `{"yaml":"routes: []\n"}`), http.StatusOK, map[string]any{"status": "reloaded"})
		if got, want := string(saved), "routes: []\n"; got != want {
			t.Fatalf("SaveConfig body=%q want %q", got, want)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config", "{"), http.StatusBadRequest, map[string]any{"error": "invalid JSON body: unexpected EOF"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config", `{"yaml":"bad: ["}`), http.StatusUnprocessableEntity, map[string]any{"error": "invalid YAML"})
	})
	t.Run("edit validates kind and sends exact structured request", func(t *testing.T) {
		var got appapi.EditRequest
		server := newCommandTestServer(t, &commandFake{edit: func(request appapi.EditRequest) error {
			got = request
			if request.Name == "broken" {
				return errors.New("reload failed")
			}
			return nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/edit", "{"), http.StatusBadRequest, map[string]any{"error": "unexpected EOF"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/edit", `{"kind":"nope"}`), http.StatusBadRequest, map[string]any{"error": "unknown edit kind: nope"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/edit", `{"kind":"route","name":"fast","data":{"model":"gpt"}}`), http.StatusOK, map[string]any{"status": "reloaded"})
		if got.Kind != "route" || got.Name != "fast" || len(got.Data) != 1 || got.Data["model"] != "gpt" {
			t.Fatalf("EditConfig request=%#v", got)
		}
		for _, kind := range []string{"general", "scheduling", "request_log", "stats", "cache", "provider", "route"} {
			requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/edit", `{"kind":"`+kind+`"}`), http.StatusOK, map[string]any{"status": "reloaded"})
			if got.Kind != kind {
				t.Fatalf("EditConfig kind=%q want %q", got.Kind, kind)
			}
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/edit", `{"kind":"general","name":"broken"}`), http.StatusBadRequest, map[string]any{"error": "reload failed"})
	})
	t.Run("validate lints without saving", func(t *testing.T) {
		var got []byte
		server := newCommandTestServer(t, &commandFake{
			validate: func(contents []byte) []appapi.ValidationIssue {
				got = append([]byte(nil), contents...)
				if strings.Contains(string(contents), "bad") {
					return []appapi.ValidationIssue{{Line: 3, Message: "boom"}}
				}
				return nil
			},
			save: func([]byte) error {
				t.Error("validate must not persist")
				return nil
			},
		})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/validate", `{"yaml":"routes: []\n"}`), http.StatusOK, map[string]any{"ok": true, "errors": []any{}})
		if string(got) != "routes: []\n" {
			t.Fatalf("ValidateConfig body=%q", got)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/validate", `{"yaml":"bad: ["}`), http.StatusOK, map[string]any{
			"ok":     false,
			"errors": []any{map[string]any{"line": float64(3), "message": "boom"}},
		})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/config/validate", "{"), http.StatusBadRequest, map[string]any{"error": "invalid JSON body: unexpected EOF"})
	})
}

func TestCommandAccountContract(t *testing.T) {
	t.Run("add routing input context and errors", func(t *testing.T) {
		var provider string
		var input appapi.AccountInput
		var contextValue any
		calls := 0
		server := newCommandTestServer(t, &commandFake{add: func(ctx context.Context, gotProvider string, gotInput appapi.AccountInput) (appapi.MutationResult, error) {
			calls++
			provider, input, contextValue = gotProvider, gotInput, ctx.Value("request")
			if gotProvider == "fail" {
				return appapi.MutationResult{}, appapi.NewHTTPError(http.StatusConflict, "already exists")
			}
			return appapi.MutationResult{ID: "acct-1", Warning: "reload deferred"}, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/", `{}`), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp/extra", `{}`), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp/", `{}`), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp///", `{}`), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>"})
		if calls != 0 {
			t.Fatalf("AddAccount calls=%d want no malformed-path mutation", calls)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp", "{"), http.StatusBadRequest, map[string]any{"error": "unexpected EOF"})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/accounts/aqp", strings.NewReader(`{"api_key":"secret","label":"one","replace":true}`)).WithContext(context.WithValue(context.Background(), "request", "add-context"))
		serveWebRequest(server, recorder, request)
		requireCommandResponse(t, recorder, http.StatusOK, map[string]any{"id": "acct-1", "status": "added", "warning": "reload deferred"})
		if provider != "aqp" || input.APIKey != "secret" || input.Label != "one" || !input.Replace || contextValue != "add-context" {
			t.Fatalf("AddAccount args provider=%q input=%#v context=%v", provider, input, contextValue)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/fail", `{}`), http.StatusConflict, map[string]any{"error": "already exists"})
	})
	t.Run("test routing presentation context and errors", func(t *testing.T) {
		var provider, id string
		var contextValue any
		server := newCommandTestServer(t, &commandFake{probe: func(ctx context.Context, gotProvider, gotID string) (appapi.ProbeResult, error) {
			provider, id, contextValue = gotProvider, gotID, ctx.Value("request")
			if gotID == "missing" {
				return appapi.ProbeResult{}, appapi.NewHTTPError(http.StatusGone, "deleted")
			}
			if gotID == "failed" {
				return appapi.ProbeResult{OK: false, HTTPStatus: 429, Reason: "rate limited", Provider: gotProvider, AccountID: gotID, Model: "m", Latency: 1750 * time.Millisecond}, nil
			}
			return appapi.ProbeResult{OK: true, HTTPStatus: 200, Provider: gotProvider, AccountID: gotID, Model: "m", Latency: 3 * time.Millisecond}, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp/test", ""), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>/<id>/test"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp/failed/test", ""), http.StatusOK, map[string]any{"status": "failed", "reason": "rate limited", "http_status": float64(429), "latency_ms": float64(1750), "provider": "aqp", "account_id": "failed", "model": "m"})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/accounts/aqp/one/test", nil).WithContext(context.WithValue(context.Background(), "request", "probe-context"))
		serveWebRequest(server, recorder, request)
		requireCommandResponse(t, recorder, http.StatusOK, map[string]any{"status": "ok", "http_status": float64(200), "latency_ms": float64(3), "provider": "aqp", "account_id": "one", "model": "m"})
		if provider != "aqp" || id != "one" || contextValue != "probe-context" {
			t.Fatalf("ProbeAccount args=(%q,%q,%v)", provider, id, contextValue)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/accounts/aqp/missing/test", ""), http.StatusGone, map[string]any{"error": "deleted"})
	})
	t.Run("remove routing and errors", func(t *testing.T) {
		var provider, id string
		calls := 0
		server := newCommandTestServer(t, &commandFake{remove: func(gotProvider, gotID string) (appapi.MutationResult, error) {
			calls++
			provider, id = gotProvider, gotID
			if gotID == "fail" {
				return appapi.MutationResult{}, errors.New("cannot remove")
			}
			return appapi.MutationResult{Warning: "reload deferred"}, nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/accounts/aqp", ""), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>/<id>"})
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/accounts/aqp/", ""), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>/<id>"})
		// A slash inside the id segment must not be folded into the id: the id is
		// a single path segment (mirrors handleAccountAdd's strict check).
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/accounts/aqp/a/b", ""), http.StatusBadRequest, map[string]any{"error": "expected /api/accounts/<provider>/<id>"})
		if calls != 0 {
			t.Fatalf("RemoveAccount calls=%d want no malformed-path mutation", calls)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/accounts/aqp/one", ""), http.StatusOK, map[string]any{"status": "removed", "warning": "reload deferred"})
		if provider != "aqp" || id != "one" {
			t.Fatalf("RemoveAccount args=(%q,%q)", provider, id)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodDelete, "/api/accounts/aqp/fail", ""), http.StatusBadRequest, map[string]any{"error": "cannot remove"})
	})
}

type loginJobFunc func(context.Context) appapi.LoginUpdate

func (job loginJobFunc) Run(ctx context.Context) appapi.LoginUpdate { return job(ctx) }

func TestCommandLoginContract(t *testing.T) {
	t.Run("start session then poll pending and complete", func(t *testing.T) {
		started := make(chan struct{})
		release := make(chan struct{})
		finished := make(chan struct{})
		server := newCommandTestServer(t, &commandFake{begin: func(ctx context.Context, provider string) (appapi.LoginStart, error) {
			if provider != "aqp" || ctx == nil {
				t.Fatalf("BeginLogin provider=%q context=%v", provider, ctx)
			}
			return appapi.LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: loginJobFunc(func(context.Context) appapi.LoginUpdate {
				close(started)
				<-release
				update := appapi.LoginUpdate{State: "done", Detail: "saved", Result: "acct-1", Warning: "reload deferred"}
				close(finished)
				return update
			})}, nil
		}})
		start := commandRequest(server, http.MethodPost, "/api/login/aqp/start", "")
		if start.Code != http.StatusOK {
			t.Fatalf("start status=%d body=%s", start.Code, start.Body.String())
		}
		body := commandJSON(t, start)
		if len(body) != 2 || body["login_url"] != "https://login.example" {
			t.Fatalf("start body=%v", body)
		}
		id, ok := body["session_id"].(string)
		if !ok || id == "" {
			t.Fatalf("session_id=%#v", body["session_id"])
		}
		<-started
		requireCommandResponse(t, commandRequest(server, http.MethodGet, "/api/login/"+id+"/poll", ""), http.StatusOK, map[string]any{"state": "pending", "detail": "https://login.example", "result": "", "warning": ""})
		close(release)
		<-finished
		server.Close()
		requireCommandResponse(t, commandRequest(server, http.MethodGet, "/api/login/"+id+"/poll", ""), http.StatusOK, map[string]any{"state": "done", "detail": "saved", "result": "acct-1", "warning": "reload deferred"})
	})
	t.Run("device flow, rejected start, nil job, unknown poll", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{begin: func(_ context.Context, provider string) (appapi.LoginStart, error) {
			switch provider {
			case "codex":
				return appapi.LoginStart{Provider: "codex", VerifyURL: "https://verify.example", UserCode: "ABCD", Job: loginJobFunc(func(context.Context) appapi.LoginUpdate { return appapi.LoginUpdate{State: "done"} })}, nil
			case "bad":
				return appapi.LoginStart{}, appapi.NewHTTPError(http.StatusUnauthorized, "login unavailable")
			case "nil":
				return appapi.LoginStart{Provider: "nil"}, nil
			default:
				t.Fatalf("unexpected provider %q", provider)
				return appapi.LoginStart{}, nil
			}
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/login//start", ""), http.StatusBadRequest, map[string]any{"error": "expected /api/login/<provider>/start"})
		start := commandRequest(server, http.MethodPost, "/api/login/codex/start", "")
		if start.Code != http.StatusOK {
			t.Fatalf("device start status=%d body=%s", start.Code, start.Body.String())
		}
		body := commandJSON(t, start)
		if len(body) != 3 || body["verify_url"] != "https://verify.example" || body["user_code"] != "ABCD" || body["session_id"] == "" {
			t.Fatalf("device start body=%v", body)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/login/bad/start", ""), http.StatusUnauthorized, map[string]any{"error": "login unavailable"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/login/nil/start", ""), http.StatusInternalServerError, map[string]any{"error": "login flow returned no job"})
		requireCommandResponse(t, commandRequest(server, http.MethodGet, "/api/login/unknown/poll", ""), http.StatusNotFound, map[string]any{"error": "unknown or expired session"})
	})
	t.Run("shutdown rejects a new job and records terminal error", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{begin: func(context.Context, string) (appapi.LoginStart, error) {
			return appapi.LoginStart{Provider: "aqp", LoginURL: "https://login.example", Job: completedLogin{}}, nil
		}})
		server.Close()
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/login/aqp/start", ""), http.StatusServiceUnavailable, map[string]any{"error": "server is shutting down"})
	})
}

func (*commandFake) AddPreset(string) ([]string, string, error) { return nil, "", nil }

func (fake *commandFake) RefreshModels(ctx context.Context, provider string) (appapi.ModelsRefreshResult, error) {
	if fake.refreshMdls != nil {
		return fake.refreshMdls(ctx, provider)
	}
	return appapi.ModelsRefreshResult{Provider: provider}, nil
}

func (fake *commandFake) SetModelDisabled(provider, model string, disabled bool) error {
	if fake.setModelDisabl != nil {
		return fake.setModelDisabl(provider, model, disabled)
	}
	return nil
}

func (fake *commandFake) ProbeMCP(ctx context.Context, name string) (appapi.MCPProbeResult, error) {
	if fake.probeMCP != nil {
		return fake.probeMCP(ctx, name)
	}
	return appapi.MCPProbeResult{OK: true, ServerName: "fake", Tools: []string{"search"}}, nil
}

func TestModelsRefreshReceivesRequestCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	called := false
	server := newCommandTestServer(t, &commandFake{refreshMdls: func(got context.Context, name string) (appapi.ModelsRefreshResult, error) {
		called = true
		if name != "up" || got != ctx {
			t.Error("refresh did not receive exact request context and provider")
		}
		cancel()
		if !errors.Is(got.Err(), context.Canceled) {
			t.Error("request cancellation did not propagate")
		}
		return appapi.ModelsRefreshResult{}, got.Err()
	}})
	r := httptest.NewRequest(http.MethodPost, "/api/models/refresh", strings.NewReader(`{"provider":"up"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	server.handleModelsRefresh(w, r)
	if !called || w.Code != http.StatusBadRequest {
		t.Fatalf("refresh called=%v status=%d", called, w.Code)
	}
}

// TestModelsDisableHandler pins POST /api/models/disable's transport
// contract: the provider/model/disabled triple is required (missing fields,
// non-boolean disabled and malformed JSON are 400s), a port error maps to 400
// with the backend message, and the happy path echoes the resulting state.
func TestModelsDisableHandler(t *testing.T) {
	t.Run("validates the request shape", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{`),
			http.StatusBadRequest, map[string]any{"error": "malformed JSON body: unexpected EOF"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{}`),
			http.StatusBadRequest, map[string]any{"error": "provider and model are required"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{"provider":"up"}`),
			http.StatusBadRequest, map[string]any{"error": "provider and model are required"})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{"provider":"up","model":"m1"}`),
			http.StatusBadRequest, map[string]any{"error": "disabled (boolean) is required"})
	})
	t.Run("surfaces the port error verbatim", func(t *testing.T) {
		server := newCommandTestServer(t, &commandFake{setModelDisabl: func(provider, model string, disabled bool) error {
			return errors.New("unknown provider \"up\"")
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{"provider":"up","model":"m1","disabled":true}`),
			http.StatusBadRequest, map[string]any{"error": "unknown provider \"up\""})
	})
	t.Run("echoes the resulting state", func(t *testing.T) {
		var gotProvider, gotModel string
		var gotDisabled bool
		server := newCommandTestServer(t, &commandFake{setModelDisabl: func(provider, model string, disabled bool) error {
			gotProvider, gotModel, gotDisabled = provider, model, disabled
			return nil
		}})
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{"provider":"zhipu","model":"glm-4.7","disabled":true}`),
			http.StatusOK, map[string]any{"provider": "zhipu", "model": "glm-4.7", "disabled": true, "status": "disabled"})
		if gotProvider != "zhipu" || gotModel != "glm-4.7" || !gotDisabled {
			t.Fatalf("port saw provider=%q model=%q disabled=%v", gotProvider, gotModel, gotDisabled)
		}
		requireCommandResponse(t, commandRequest(server, http.MethodPost, "/api/models/disable", `{"provider":"zhipu","model":"glm-4.7","disabled":false}`),
			http.StatusOK, map[string]any{"provider": "zhipu", "model": "glm-4.7", "disabled": false, "status": "enabled"})
	})
}
