package web

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"model-proxy/internal/appapi"
)

// handlers_takeover_test.go — transport contract for the /api/takeover
// subtree: decode discipline, error classification, name validation. The
// takeover semantics themselves are covered in internal/admin and
// internal/takeover.

func TestTakeoverSurfaceHandler(t *testing.T) {
	var gotMode string
	reads := &readAPIStub{takeover: func(mode string) (appapi.TakeoverSurface, error) {
		gotMode = mode
		return appapi.TakeoverSurface{
			Clients: []appapi.TakeoverClient{{
				Name: "claude", Family: "claude", Format: "json", Source: "preset",
				File: "/home/u/.claude/settings.json", Installed: true, TakenOver: true, DriftOK: true,
			}},
			TemplatesDir: "/home/u/.model-proxy/takeover-templates",
			BackupDir:    "/cfg/.model-proxy",
		}, nil
	}}
	s := newReadServer(t, reads)
	rec := commandRequest(s, http.MethodGet, "/api/takeover", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/takeover = %d (%s)", rec.Code, rec.Body)
	}
	body := commandJSON(t, rec)
	clients, ok := body["clients"].([]any)
	if !ok || len(clients) != 1 {
		t.Fatalf("clients = %v", body["clients"])
	}
	first := clients[0].(map[string]any)
	if first["name"] != "claude" || first["taken_over"] != true || first["drift_ok"] != true {
		t.Errorf("client row = %v", first)
	}
	if body["templates_dir"] == "" || body["backup_dir"] == "" {
		t.Errorf("dirs missing: %v", body)
	}
	if gotMode != "" {
		t.Errorf("mode = %q, want empty for a bare GET", gotMode)
	}
	// ?mode= is passed through to the read port (the mode select's preview).
	rec = commandRequest(s, http.MethodGet, "/api/takeover?mode=split", "")
	if rec.Code != http.StatusOK || gotMode != "split" {
		t.Errorf("?mode=split → %d, port got %q", rec.Code, gotMode)
	}

	// A template-load failure surfaces as a 500 with the backend message.
	reads2 := &readAPIStub{takeover: func(string) (appapi.TakeoverSurface, error) {
		return appapi.TakeoverSurface{}, errors.New("template pi: family broken")
	}}
	s2 := newReadServer(t, reads2)
	rec2 := commandRequest(s2, http.MethodGet, "/api/takeover", "")
	if rec2.Code != http.StatusInternalServerError || !strings.Contains(rec2.Body.String(), "family broken") {
		t.Errorf("load failure = %d %s, want 500 with message", rec2.Code, rec2.Body)
	}
}

func TestTakeoverRunHandler(t *testing.T) {
	var gotClient, gotMode string
	commands := &commandFake{
		takeoverRun: func(client, mode string) (appapi.TakeoverRunResult, error) {
			gotClient, gotMode = client, mode
			return appapi.TakeoverRunResult{
				Status:   "ok",
				Applied:  []appapi.TakeoverApplied{{Name: "pi", Note: "native coverage"}},
				Skipped:  []string{"gemini-cli"},
				Warnings: []string{},
			}, nil
		},
	}
	s := newCommandTestServer(t, commands)

	rec := commandRequest(s, http.MethodPost, "/api/takeover", `{"client":"pi","mode":"split"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/takeover = %d (%s)", rec.Code, rec.Body)
	}
	if gotClient != "pi" || gotMode != "split" {
		t.Errorf("service got client=%q mode=%q, want pi/split", gotClient, gotMode)
	}
	body := commandJSON(t, rec)
	if body["status"] != "ok" {
		t.Errorf("status = %v", body["status"])
	}
	applied := body["applied"].([]any)
	if len(applied) != 1 || applied[0].(map[string]any)["name"] != "pi" || applied[0].(map[string]any)["note"] != "native coverage" {
		t.Errorf("applied = %v", body["applied"])
	}

	// Malformed body → 400, never reaches the service.
	gotClient, gotMode = "", ""
	rec = commandRequest(s, http.MethodPost, "/api/takeover", `{`)
	if rec.Code != http.StatusBadRequest || gotClient != "" {
		t.Errorf("malformed = %d (service called with %q)", rec.Code, gotClient)
	}

	// Backend failure (unknown client, bad mode) → 400 with the message.
	commands.takeoverRun = func(string, string) (appapi.TakeoverRunResult, error) {
		return appapi.TakeoverRunResult{}, errors.New(`unknown takeover client "nope"`)
	}
	rec = commandRequest(s, http.MethodPost, "/api/takeover", `{"client":"nope"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unknown takeover client") {
		t.Errorf("backend error = %d %s", rec.Code, rec.Body)
	}
}

func TestTakeoverRestoreHandler(t *testing.T) {
	var gotClient string
	commands := &commandFake{
		takeoverRest: func(client string) (appapi.TakeoverRestoreResult, error) {
			gotClient = client
			return appapi.TakeoverRestoreResult{Status: "restored", Restored: []string{"pi"}, Skipped: []string{}}, nil
		},
	}
	s := newCommandTestServer(t, commands)
	rec := commandRequest(s, http.MethodPost, "/api/takeover/restore", `{}`)
	if rec.Code != http.StatusOK || gotClient != "" {
		t.Fatalf("restore = %d (client %q)", rec.Code, gotClient)
	}
	body := commandJSON(t, rec)
	if body["status"] != "restored" || body["restored"].([]any)[0] != "pi" {
		t.Errorf("body = %v", body)
	}
}

func TestTakeoverTemplateHandlers(t *testing.T) {
	reads := &readAPIStub{takeoverTemplate: func(name string) (appapi.TakeoverTemplateDoc, error) {
		if name == "claude" {
			return appapi.TakeoverTemplateDoc{Name: "claude", Source: "preset", YAML: "format: json\n"}, nil
		}
		return appapi.TakeoverTemplateDoc{}, appapi.NewHTTPError(http.StatusNotFound, "unknown takeover template: "+name)
	}}
	var savedName, savedYAML, deletedName string
	commands := &commandFake{
		templateSave: func(name string, yaml []byte) error {
			savedName, savedYAML = name, string(yaml)
			return nil
		},
		templateDel: func(name string) error {
			deletedName = name
			if name == "claude" {
				return appapi.NewHTTPError(http.StatusBadRequest, "template \"claude\" is a built-in preset with no user override — nothing to delete")
			}
			return nil
		},
	}
	s := newReadServerWithCommands(t, reads, commands)

	// GET: preset round-trip and 404 passthrough.
	rec := commandRequest(s, http.MethodGet, "/api/takeover/templates/claude", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET template = %d", rec.Code)
	}
	if got := commandJSON(t, rec)["source"]; got != "preset" {
		t.Errorf("source = %v", got)
	}
	rec = commandRequest(s, http.MethodGet, "/api/takeover/templates/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown template = %d, want 404", rec.Code)
	}
	// Nested/empty names are rejected at the transport before the service.
	for _, p := range []string{"/api/takeover/templates/", "/api/takeover/templates/a/b"} {
		if rec := commandRequest(s, http.MethodGet, p, ""); rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", p, rec.Code)
		}
	}

	// PUT: body validation + pass-through.
	rec = commandRequest(s, http.MethodPut, "/api/takeover/templates/mine", `{"yaml":"format: json\n"}`)
	if rec.Code != http.StatusOK || savedName != "mine" || savedYAML != "format: json\n" {
		t.Fatalf("PUT = %d (saved %q %q)", rec.Code, savedName, savedYAML)
	}
	if rec := commandRequest(s, http.MethodPut, "/api/takeover/templates/mine", `{"yaml":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty yaml = %d, want 400", rec.Code)
	}
	if rec := commandRequest(s, http.MethodPut, "/api/takeover/templates/mine", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed yaml body = %d, want 400", rec.Code)
	}

	// DELETE: user template ok; preset rejection carries the backend message.
	rec = commandRequest(s, http.MethodDelete, "/api/takeover/templates/mine", "")
	if rec.Code != http.StatusOK || deletedName != "mine" {
		t.Fatalf("DELETE mine = %d (deleted %q)", rec.Code, deletedName)
	}
	rec = commandRequest(s, http.MethodDelete, "/api/takeover/templates/claude", "")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "built-in preset") {
		t.Errorf("DELETE preset = %d %s, want 400 with reason", rec.Code, rec.Body)
	}
}
