package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
)

// TestParsePinTTL: both --ttl DUR and --ttl=DUR forms parse; absent → 0.
func TestParsePinTTL(t *testing.T) {
	if d := ParsePinTTL([]string{"--ttl", "90m"}); d != 90*time.Minute {
		t.Errorf("--ttl 90m = %v want 90m", d)
	}
	if d := ParsePinTTL([]string{"--ttl=2h"}); d != 2*time.Hour {
		t.Errorf("--ttl=2h = %v want 2h", d)
	}
	if d := ParsePinTTL([]string{"glm", "zhipu"}); d != 0 {
		t.Errorf("absent --ttl = %v want 0", d)
	}
}

// TestIsDaemonUnreachable: connection-refused errors match, others don't.
func TestIsDaemonUnreachable(t *testing.T) {
	if !IsDaemonUnreachable(fmt.Errorf("dial tcp 127.0.0.1:8080: connect: connection refused")) {
		t.Error("connection refused should match")
	}
	if IsDaemonUnreachable(fmt.Errorf("some other error")) {
		t.Error("non-refused error should not match")
	}
}

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
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": "2030-01-01T00:00:00Z", "status": "pinned"})
	}))
	defer srv.Close()

	out, err := DoPin(srv.URL, "glm", "zhipu", time.Hour)
	if err != nil {
		t.Fatalf("doPin: %v", err)
	}
	if !strings.Contains(out, "pinned glm → zhipu") || !strings.Contains(out, "expires 2030") {
		t.Errorf("doPin out=%q", out)
	}
	// no-expiry suffix when expires_at empty.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": ""})
	}))
	defer srv2.Close()
	out2, _ := DoPin(srv2.URL, "glm", "z", 0)
	if !strings.Contains(out2, "no expiry") {
		t.Errorf("no-expiry out=%q", out2)
	}
	// 400 surfaces the daemon's message.
	if _, err := DoPin(srv.URL, "glm", "bad", 0); err == nil || !strings.Contains(err.Error(), "cannot pin") {
		t.Errorf("bad-provider err=%v want cannot pin", err)
	}
	// Unreachable daemon → error.
	if _, err := DoPin("http://127.0.0.1:1", "glm", "z", 0); err == nil {
		t.Error("unreachable doPin should error")
	}
}

// TestDoUnpin covers DELETE /api/pin: removed vs not-present lines.
func TestDoUnpin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("route") == "absent" {
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"removed": false})
			return
		}
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"removed": true})
	}))
	defer srv.Close()
	if out, _ := DoUnpin(srv.URL, "glm"); !strings.Contains(out, "unpinned glm") {
		t.Errorf("removed out=%q", out)
	}
	if out, _ := DoUnpin(srv.URL, "absent"); !strings.Contains(out, "no pin on absent") {
		t.Errorf("absent out=%q", out)
	}
}

// TestDoListPins covers GET /api/pin rendering (table + empty).
func TestDoListPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("empty") == "1" {
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"pins": []any{}})
			return
		}
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"pins": []map[string]any{
			{"route": "glm", "provider": "zhipu", "expires_at": ""},
			{"route": "codex", "provider": "codex", "expires_at": "2030-01-01T00:00:00Z"},
		}})
	}))
	defer srv.Close()
	out, _ := DoListPins(srv.URL)
	if !strings.Contains(out, "ROUTE") || !strings.Contains(out, "glm") || !strings.Contains(out, "never") || !strings.Contains(out, "2030") {
		t.Errorf("list out=%q", out)
	}
	// empty query param path
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clitest.WriteJSON(w, http.StatusOK, map[string]any{"pins": []any{}})
	}))
	defer srv2.Close()
	if out, _ := DoListPins(srv2.URL); !strings.Contains(out, "(no active pins)") {
		t.Errorf("empty list out=%q", out)
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
				clitest.WriteJSON(w, 200, map[string]any{"expires_at": "2030-01-01T00:00:00Z"})
			} else if r.Method == http.MethodDelete {
				clitest.WriteJSON(w, 200, map[string]any{"removed": true})
			}
		}
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := clitest.WriteTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes:\n  glm: [{provider: zhipu, model: glm-4}]\n")

	out := clitest.GrabStdout(t, func() { RunPin([]string{"--config", cfgPath, "glm", "zhipu", "--ttl", "1h"}) })
	if !strings.Contains(out, "pinned glm → zhipu") {
		t.Errorf("cmdPin out=%q", out)
	}
	out = clitest.GrabStdout(t, func() { RunUnpin([]string{"--config", cfgPath, "glm"}) })
	if !strings.Contains(out, "unpinned glm") {
		t.Errorf("cmdUnpin out=%q", out)
	}
}
