package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// pinDaemon stands in for the daemon's pin/health endpoints.
func pinDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/pin" && r.Method == http.MethodGet:
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"pins": []map[string]any{
				{"route": "glm", "provider": "zhipu", "expires_at": "2030-01-01T00:00:00Z"},
			}})
		case r.URL.Path == "/api/pin" && r.Method == http.MethodPost:
			clitest.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "cannot pin: no target"})
		case r.URL.Path == "/api/health/reset" && r.Method == http.MethodPost:
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"cleared": []string{"zhipu"}, "model_locks_cleared": 1})
		case r.URL.Path == "/api/health/freeze" && r.Method == http.MethodPost:
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"frozen": []string{"zhipu"}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func pinDaemonConfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	listen := strings.TrimPrefix(srv.URL, "http://")
	return clitest.WriteTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes:\n  glm: [{provider: zhipu, model: glm-4}]\n")
}

// TestCmdPin_ListInProcess: bare `pin` lists the active pins (no os.Exit on
// the success path).
func TestCmdPin_ListInProcess(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)
	out := clitest.GrabStdout(t, func() { RunPin([]string{"--config", cfg}) })
	if !strings.Contains(out, "glm") || !strings.Contains(out, "zhipu") {
		t.Errorf("pin list out=%q", out)
	}
}

// TestCmdUnfreeze_InProcess: `unfreeze <provider>` posts the reset and prints
// the cleared line (no os.Exit on the success path).
func TestCmdUnfreeze_InProcess(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)
	out := clitest.GrabStdout(t, func() { RunUnfreeze([]string{"zhipu", "--config", cfg}) })
	if !strings.Contains(out, "zhipu") {
		t.Errorf("unfreeze out=%q", out)
	}
}

// TestCmdFreeze_InProcess: `freeze <provider>` posts the freeze and prints the
// frozen line (no os.Exit on the success path).
func TestCmdFreeze_InProcess(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)
	out := clitest.GrabStdout(t, func() { RunFreeze([]string{"zhipu", "--config", cfg}) })
	if !strings.Contains(out, "froze zhipu") {
		t.Errorf("freeze out=%q", out)
	}
}

// TestCmdFreeze_RequiresProvider: bare `freeze` is a usage error (exit 1) —
// unlike `unfreeze`, freeze has no no-arg = all form.
func TestCmdFreeze_RequiresProvider(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)
	_, stderr, code := clitest.RunCLI(t, "freeze", cfg)
	if code != 1 || !strings.Contains(stderr, "usage: model-proxy freeze <provider>") {
		t.Errorf("freeze (no provider): exit=%d stderr=%q", code, stderr)
	}
}

// TestCmdPin_UsageErrors: one positional (missing provider) and an invalid
// --ttl are exit-1 usage errors; unpin with no route is too.
func TestCmdPin_UsageErrors(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)

	_, stderr, code := clitest.RunCLI(t, "pin", cfg, "glm")
	if code != 1 || !strings.Contains(stderr, "usage: model-proxy pin") {
		t.Errorf("pin (missing provider): exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "pin", cfg, "glm", "zhipu", "--ttl", "bogus")
	if code != 1 || !strings.Contains(stderr, `invalid --ttl "bogus"`) {
		t.Errorf("pin --ttl bogus: exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "unpin", cfg)
	if code != 1 || !strings.Contains(stderr, "usage: model-proxy unpin") {
		t.Errorf("unpin (no route): exit=%d stderr=%q", code, stderr)
	}
}

// TestCmdPin_DaemonErrors: a daemon 400 surfaces its message (exit 1); an
// unreachable daemon prints the "cannot reach daemon" guidance (exit 1).
func TestCmdPin_DaemonErrors(t *testing.T) {
	srv := pinDaemon(t)
	cfg := pinDaemonConfig(t, srv)

	_, stderr, code := clitest.RunCLI(t, "pin", cfg, "glm", "zhipu")
	if code != 1 || !strings.Contains(stderr, "cannot pin") {
		t.Errorf("pin daemon-400: exit=%d stderr=%q", code, stderr)
	}

	dead := clitest.WriteTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes:\n  glm: [{provider: zhipu, model: glm-4}]\n")
	_, stderr, code = clitest.RunCLI(t, "pin", dead, "glm", "zhipu")
	if code != 1 || !strings.Contains(stderr, "cannot reach daemon") {
		t.Errorf("pin unreachable: exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "unfreeze", dead, "zhipu")
	if code != 1 || !strings.Contains(stderr, "cannot reach daemon") {
		t.Errorf("unfreeze unreachable: exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "freeze", dead, "zhipu")
	if code != 1 || !strings.Contains(stderr, "cannot reach daemon") {
		t.Errorf("freeze unreachable: exit=%d stderr=%q", code, stderr)
	}
}
