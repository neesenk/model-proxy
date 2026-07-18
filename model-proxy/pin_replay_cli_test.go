package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestDoPin covers the pin CLI core (POST /api/pin) against an httptest daemon:
// success line, non-200 error, and connection-refused error path.
func TestDoPin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/pin" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Route    string `json:"route"`
			Provider string `json:"provider"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.Provider == "bad" {
			http.Error(w, `cannot pin: no target`, http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"expires_at": "2030-01-01T00:00:00Z", "status": "pinned"})
	}))
	defer srv.Close()

	out, err := doPin(srv.URL, "glm", "zhipu", time.Hour)
	if err != nil {
		t.Fatalf("doPin: %v", err)
	}
	if !strings.Contains(out, "pinned glm → zhipu") || !strings.Contains(out, "expires 2030") {
		t.Errorf("doPin out=%q", out)
	}
	// no-expiry suffix when expires_at empty.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"expires_at": ""})
	}))
	defer srv2.Close()
	out2, _ := doPin(srv2.URL, "glm", "z", 0)
	if !strings.Contains(out2, "no expiry") {
		t.Errorf("no-expiry out=%q", out2)
	}
	// 400 surfaces the daemon's message.
	if _, err := doPin(srv.URL, "glm", "bad", 0); err == nil || !strings.Contains(err.Error(), "cannot pin") {
		t.Errorf("bad-provider err=%v want cannot pin", err)
	}
	// Unreachable daemon → error.
	if _, err := doPin("http://127.0.0.1:1", "glm", "z", 0); err == nil {
		t.Error("unreachable doPin should error")
	}
}

// TestDoUnpin covers DELETE /api/pin: removed vs not-present lines.
func TestDoUnpin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("route") == "absent" {
			writeJSON(w, http.StatusOK, map[string]any{"removed": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"removed": true})
	}))
	defer srv.Close()
	if out, _ := doUnpin(srv.URL, "glm"); !strings.Contains(out, "unpinned glm") {
		t.Errorf("removed out=%q", out)
	}
	if out, _ := doUnpin(srv.URL, "absent"); !strings.Contains(out, "no pin on absent") {
		t.Errorf("absent out=%q", out)
	}
}

// TestDoListPins covers GET /api/pin rendering (table + empty).
func TestDoListPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("empty") == "1" {
			writeJSON(w, http.StatusOK, map[string]any{"pins": []any{}})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"pins": []map[string]any{
			{"route": "glm", "provider": "zhipu", "expires_at": ""},
			{"route": "codex", "provider": "codex", "expires_at": "2030-01-01T00:00:00Z"},
		}})
	}))
	defer srv.Close()
	out, _ := doListPins(srv.URL)
	if !strings.Contains(out, "ROUTE") || !strings.Contains(out, "glm") || !strings.Contains(out, "never") || !strings.Contains(out, "2030") {
		t.Errorf("list out=%q", out)
	}
	// empty query param path
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"pins": []any{}})
	}))
	defer srv2.Close()
	if out, _ := doListPins(srv2.URL); !strings.Contains(out, "(no active pins)") {
		t.Errorf("empty list out=%q", out)
	}
}

// TestDoReplay covers the replay core: fetch the stored request, re-send with the
// force-provider header, return the new backend's body. Errors: missing id (404)
// and empty body.
func TestDoReplay(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/requests/ghost":
			http.Error(w, "no record", http.StatusNotFound)
		case "/api/requests/nobody":
			writeJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{{"path": "/v1/responses", "request_body": ""}}})
		case "/api/requests/good":
			writeJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{{"path": "/v1/responses", "request_body": `{"model":"glm","input":[]}`}}})
		case "/v1/responses":
			// The replay re-sends with the force-provider header.
			if fp := r.Header.Get("x-mp-force-provider"); fp != "zhipu" {
				t.Errorf("force-provider header=%q want zhipu", fp)
			}
			io.WriteString(w, `{"replayed":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	body, err := doReplay(srv.URL, "good", "zhipu")
	if err != nil {
		t.Fatalf("doReplay good: %v", err)
	}
	if !strings.Contains(string(body), `"replayed":true`) {
		t.Errorf("replay body=%q", string(body))
	}
	if _, err := doReplay(srv.URL, "ghost", "zhipu"); err == nil || !strings.Contains(err.Error(), "no request log") {
		t.Errorf("ghost err=%v want no request log", err)
	}
	if _, err := doReplay(srv.URL, "nobody", "zhipu"); err == nil || !strings.Contains(err.Error(), "no captured request body") {
		t.Errorf("nobody err=%v want no captured body", err)
	}
}

// TestDoReplay_TruncatedBody (F6b): a record whose captured request body was
// truncated at max_body_bytes must NOT be replayed verbatim — it would send an
// incomplete request → a misleading upstream 400. doReplay refuses with a clear
// message pointing at max_body_bytes.
func TestDoReplay_TruncatedBody(t *testing.T) {
	trunc := `{"model":"glm","input":"` + strings.Repeat("x", 50) + `"}` + truncMarker
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"records": []map[string]any{
			{"path": "/v1/responses", "request_body": trunc},
		}})
	}))
	defer srv.Close()
	_, err := doReplay(srv.URL, "trunc", "zhipu")
	if err == nil || !strings.Contains(err.Error(), "truncated") || !strings.Contains(err.Error(), "max_body_bytes") {
		t.Errorf("truncated-body replay err=%v, want a truncated/max_body_bytes refusal", err)
	}
}

// TestCmdPin_InProcess: the cmdPin/cmdUnpin success paths against an httptest
// daemon via a temp config whose listen points at it. Covers the os.Exit-free
// command wrappers (arg parse → do* → print).
func TestCmdPin_InProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/pin":
			if r.Method == http.MethodPost {
				writeJSON(w, 200, map[string]any{"expires_at": "2030-01-01T00:00:00Z"})
			} else if r.Method == http.MethodDelete {
				writeJSON(w, 200, map[string]any{"removed": true})
			}
		}
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes:\n  glm: [{provider: zhipu, model: glm-4}]\n")

	out := captureStdout(t, func() { cmdPin([]string{"--config", cfgPath, "glm", "zhipu", "--ttl", "1h"}) })
	if !strings.Contains(out, "pinned glm → zhipu") {
		t.Errorf("cmdPin out=%q", out)
	}
	out = captureStdout(t, func() { cmdUnpin([]string{"--config", cfgPath, "glm"}) })
	if !strings.Contains(out, "unpinned glm") {
		t.Errorf("cmdUnpin out=%q", out)
	}
}
