package diag

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// replayDaemon serves the request-log fetch and the re-send endpoint for the
// `replay` subprocess tests, plus the shadow-report endpoint for `shadow
// report`.
func replayDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/requests/good":
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"records": []map[string]any{
				{"request_id": "good", "path": "/v1/responses", "request_body": `{"model":"glm","input":[]}`},
			}})
		case "/v1/responses":
			if fp := r.Header.Get("x-mp-force-provider"); fp != "zhipu" {
				t.Errorf("force-provider header=%q want zhipu", fp)
			}
			io.WriteString(w, `{"replayed":true}`)
		case "/api/shadow-report":
			clitest.WriteJSON(w, http.StatusOK, map[string]any{"enabled": true, "entries": []map[string]any{
				{"route": "glm", "primary_provider": "zhipu", "shadow_provider": "codex", "samples": 3, "status_match_rate": 1.0},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func replayDaemonConfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	listen := strings.TrimPrefix(srv.URL, "http://")
	return clitest.WriteTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes: {}\n")
}

// TestCmdReplay_Subprocess: the happy path writes the new backend's body to
// stdout (exit 0); a missing --to and an unknown record id are exit-1 errors
// with the message on stderr.
func TestCmdReplay_Subprocess(t *testing.T) {
	srv := replayDaemon(t)
	cfg := replayDaemonConfig(t, srv)

	stdout, _, code := clitest.RunCLI(t, "replay", cfg, "good", "--to", "zhipu")
	if code != 0 {
		t.Fatalf("replay exit=%d want 0", code)
	}
	if !strings.Contains(stdout, `"replayed":true`) {
		t.Errorf("replay stdout missing replayed body:\n%s", stdout)
	}

	_, stderr, code := clitest.RunCLI(t, "replay", cfg, "good")
	if code != 1 || !strings.Contains(stderr, "--to is required") {
		t.Errorf("replay without --to: exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "replay", cfg, "ghost", "--to", "zhipu")
	if code != 1 || !strings.Contains(stderr, "no request log") {
		t.Errorf("replay ghost: exit=%d stderr=%q", code, stderr)
	}
}

// TestCmdShadow_Subprocess: `shadow report` renders the aggregation table
// (exit 0); no subcommand and an unknown subcommand are exit-1 usage errors.
func TestCmdShadow_Subprocess(t *testing.T) {
	srv := replayDaemon(t)
	cfg := replayDaemonConfig(t, srv)

	stdout, _, code := clitest.RunCLI(t, "shadow", cfg, "report")
	if code != 0 {
		t.Fatalf("shadow report exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "ROUTE") || !strings.Contains(stdout, "zhipu") {
		t.Errorf("shadow report stdout missing table:\n%s", stdout)
	}

	// No subcommand at all (not even --config — that would land in the
	// unknown-subcommand branch): config comes from ./config.yaml in the CWD.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("listen: 127.0.0.1:1\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := clitest.RunCLIInCWD(t, dir, "shadow")
	if code != 1 || !strings.Contains(stderr, "usage: model-proxy shadow report") {
		t.Errorf("shadow (no subcommand): exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "shadow", cfg, "bogus")
	if code != 1 || !strings.Contains(stderr, "unknown shadow subcommand") {
		t.Errorf("shadow bogus: exit=%d stderr=%q", code, stderr)
	}
}
