package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
)

// handlers_diag_test.go — transport contract for the diagnostics mutations
// (/api/replay, /api/routes/test, /api/models/catalog/refresh): decode
// discipline and error classification; the semantics live in internal/admin.

func TestReplayHandler(t *testing.T) {
	var gotID, gotProvider string
	commands := &commandFake{
		replay: func(_ context.Context, id, provider string) (appapi.ReplayResult, error) {
			gotID, gotProvider = id, provider
			return appapi.ReplayResult{Status: 200, Body: `{"ok":true}`, LatencyMs: 12}, nil
		},
	}
	s := newCommandTestServer(t, commands)

	rec := commandRequest(s, http.MethodPost, "/api/replay", `{"id":"r1","provider":"deepseek"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replay = %d (%s)", rec.Code, rec.Body)
	}
	if gotID != "r1" || gotProvider != "deepseek" {
		t.Errorf("service got id=%q provider=%q", gotID, gotProvider)
	}
	body := commandJSON(t, rec)
	if body["status"] != float64(200) || body["body"] != `{"ok":true}` {
		t.Errorf("body = %v", body)
	}

	// Malformed body → 400, never reaches the service.
	gotID = ""
	if rec := commandRequest(s, http.MethodPost, "/api/replay", `{`); rec.Code != http.StatusBadRequest || gotID != "" {
		t.Errorf("malformed = %d (service called with %q)", rec.Code, gotID)
	}
	// Backend guard (shadow record, unknown provider…) carries the message.
	commands.replay = func(context.Context, string, string) (appapi.ReplayResult, error) {
		return appapi.ReplayResult{}, appapi.NewHTTPError(http.StatusBadRequest, "record r1 is a shadow evaluation record")
	}
	rec = commandRequest(s, http.MethodPost, "/api/replay", `{"id":"r1","provider":"p"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "shadow") {
		t.Errorf("guard = %d %s", rec.Code, rec.Body)
	}
}

func TestRouteTestHandler(t *testing.T) {
	var gotModel string
	commands := &commandFake{
		routeTest: func(_ context.Context, model string) (appapi.RouteTestResult, error) {
			gotModel = model
			return appapi.RouteTestResult{Model: model, Results: []appapi.RouteTestTarget{
				{Provider: "p1", Model: "m1", OK: true, HTTPStatus: 200, LatencyMs: 120},
				{Provider: "p2", Model: "m1", OK: false, HTTPStatus: 429, Reason: "rate limited", LatencyMs: 30},
			}}, nil
		},
	}
	s := newCommandTestServer(t, commands)

	rec := commandRequest(s, http.MethodPost, "/api/routes/test", `{"model":"glm-5.2"}`)
	if rec.Code != http.StatusOK || gotModel != "glm-5.2" {
		t.Fatalf("routes/test = %d (model %q)", rec.Code, gotModel)
	}
	body := commandJSON(t, rec)
	results := body["results"].([]any)
	if len(results) != 2 || results[0].(map[string]any)["ok"] != true || results[1].(map[string]any)["reason"] != "rate limited" {
		t.Errorf("results = %v", body["results"])
	}

	// Unknown model → 404 with the route list.
	commands.routeTest = func(context.Context, string) (appapi.RouteTestResult, error) {
		return appapi.RouteTestResult{}, appapi.NewHTTPError(http.StatusNotFound, `no route for model "ghost"`)
	}
	rec = commandRequest(s, http.MethodPost, "/api/routes/test", `{"model":"ghost"}`)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "no route") {
		t.Errorf("unknown model = %d %s", rec.Code, rec.Body)
	}
}

func TestModelsCatalogRefreshHandler(t *testing.T) {
	called := false
	commands := &commandFake{
		catalogPull: func(context.Context) (appapi.ModelsCatalogPull, error) {
			called = true
			return appapi.ModelsCatalogPull{Status: "refreshed", Count: 12345, ETag: "abc"}, nil
		},
	}
	s := newCommandTestServer(t, commands)
	rec := commandRequest(s, http.MethodPost, "/api/models/catalog/refresh", "")
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("catalog refresh = %d (called %v)", rec.Code, called)
	}
	body := commandJSON(t, rec)
	if body["status"] != "refreshed" || body["count"] != float64(12345) {
		t.Errorf("body = %v", body)
	}

	commands.catalogPull = func(context.Context) (appapi.ModelsCatalogPull, error) {
		return appapi.ModelsCatalogPull{}, errors.New("models.dev unreachable")
	}
	rec = commandRequest(s, http.MethodPost, "/api/models/catalog/refresh", "")
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "unreachable") {
		t.Errorf("fetch failure = %d %s, want 502 with message", rec.Code, rec.Body)
	}
}
