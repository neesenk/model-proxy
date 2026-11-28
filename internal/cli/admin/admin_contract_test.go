package admin

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
	"model-proxy/internal/daemonctl"
	"model-proxy/internal/display"
)

// plainText pins the status symbols to their plain form. The commands render
// ✓ / • through display, whose color decision was made at process start
// against the real stdout — an interactive `go test -v` would else wrap them
// in ANSI and break every exact-line assertion here.
func plainText(t *testing.T) {
	t.Helper()
	previous := display.ColorEnabled
	display.SetColorEnabled(false)
	t.Cleanup(func() { display.SetColorEnabled(previous) })
}

// adminCalls are the five admin-surface requests this package's commands
// make, in the order the tests below issue them.
type adminCall struct {
	method, path string
	call         func(base string) (string, error)
}

var adminCalls = []adminCall{
	{"POST", "/api/pin", func(base string) (string, error) { return DoPin(base, "glm", "zhipu", 0) }},
	{"DELETE", "/api/pin", func(base string) (string, error) { return DoUnpin(base, "glm") }},
	{"GET", "/api/pin", DoListPins},
	{"POST", "/api/health/reset", func(base string) (string, error) { return DoUnfreeze(base, "zhipu") }},
	{"POST", "/api/health/freeze", func(base string) (string, error) { return DoFreeze(base, "zhipu") }},
}

// TestAdminRequestsCarryTheConfiguredAdminToken: every admin-surface request
// carries the configured admin token (CLI.md「daemon 鉴权（S2，非回环部署）」),
// and a loopback deployment without a token file attaches nothing. A call
// site that falls back to a bare client loses its credential and answers 401
// on every non-loopback deployment.
func TestAdminRequestsCarryTheConfiguredAdminToken(t *testing.T) {
	const token = "test-admin-token-0123456789"
	tokenFile := filepath.Join(t.TempDir(), "admin_token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plainText(t)
	cases := []struct {
		name      string
		tokenFile string
		wantAuth  string
	}{
		{"admin token configured", tokenFile, "Bearer " + token},
		{"loopback deployment without a token file", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemonctl.SetAuthFiles(tc.tokenFile, "")
			t.Cleanup(func() { daemonctl.SetAuthFiles("", "") })
			type seen struct{ method, path, auth string }
			var got []seen
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = append(got, seen{r.Method, r.URL.Path, r.Header.Get("Authorization")})
				switch r.URL.Path {
				case "/api/pin":
					if r.Method == http.MethodDelete {
						clitest.WriteJSON(w, http.StatusOK, map[string]any{"removed": true})
					} else if r.Method == http.MethodPost {
						clitest.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": ""})
					} else {
						clitest.WriteJSON(w, http.StatusOK, map[string]any{"pins": []any{}})
					}
				case "/api/health/reset":
					clitest.WriteJSON(w, http.StatusOK, map[string]any{"cleared": []string{"zhipu"}})
				case "/api/health/freeze":
					clitest.WriteJSON(w, http.StatusOK, map[string]any{"frozen": []string{"zhipu"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			for _, call := range adminCalls {
				if _, err := call.call(srv.URL); err != nil {
					t.Fatalf("%s %s: %v", call.method, call.path, err)
				}
			}
			want := make([]seen, 0, len(adminCalls))
			for _, call := range adminCalls {
				want = append(want, seen{call.method, call.path, tc.wantAuth})
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("requests seen by the daemon:\ngot:  %+v\nwant: %+v", got, want)
			}
		})
	}
}

// TestDoPin_SendsRouteProviderAndTTLSeconds: --ttl reaches the daemon as
// whole seconds and an omitted --ttl as zero — a pin whose ttl never leaves
// the CLI would be written without expiry while the operator asked for 90m.
func TestDoPin_SendsRouteProviderAndTTLSeconds(t *testing.T) {
	plainText(t)
	type pinBody struct {
		Route      string `json:"route"`
		Provider   string `json:"provider"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	cases := []struct {
		name string
		ttl  time.Duration
		want pinBody
	}{
		{"--ttl 90m", 90 * time.Minute, pinBody{"glm", "zhipu", 5400}},
		{"no --ttl", 0, pinBody{"glm", "zhipu", 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got pinBody
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewDecoder(r.Body).Decode(&got)
				clitest.WriteJSON(w, http.StatusOK, map[string]any{"expires_at": ""})
			}))
			defer srv.Close()
			out, err := DoPin(srv.URL, "glm", "zhipu", tc.ttl)
			if err != nil {
				t.Fatalf("DoPin: %v", err)
			}
			if out != "✓ pinned glm → zhipu (no expiry (until unpin or restart))\n" {
				t.Errorf("DoPin line = %q", out)
			}
			if got != tc.want {
				t.Fatalf("POST /api/pin body = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDoCommandsRejectMalformedSuccessBody: a 200 whose body is not the
// expected JSON is an error, never a success line — a proxy in front of the
// daemon answers "200" with an empty or foreign body.
func TestDoCommandsRejectMalformedSuccessBody(t *testing.T) {
	plainText(t)
	cases := []struct {
		adminCall
		wantErr string
	}{
		{adminCall{"POST", "/api/pin", func(base string) (string, error) { return DoPin(base, "glm", "zhipu", 0) }}, "parse pin response"},
		{adminCall{"DELETE", "/api/pin", func(base string) (string, error) { return DoUnpin(base, "glm") }}, "parse unpin response"},
		{adminCall{"GET", "/api/pin", DoListPins}, "parse pins response"},
		{adminCall{"POST", "/api/health/reset", func(base string) (string, error) { return DoUnfreeze(base, "zhipu") }}, "parse daemon response"},
		{adminCall{"POST", "/api/health/freeze", func(base string) (string, error) { return DoFreeze(base, "zhipu") }}, "parse daemon response"},
	}
	for _, tc := range cases {
		for _, body := range []string{"not-json", ""} {
			t.Run(tc.method+" "+tc.path+" body="+fmt.Sprintf("%q", body), func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, body)
				}))
				defer srv.Close()
				out, err := tc.call(srv.URL)
				if out != "" || err == nil || !strings.HasPrefix(err.Error(), tc.wantErr+": ") {
					t.Fatalf("%s %s: out=%q err=%v, want no output and a %q error", tc.method, tc.path, out, err, tc.wantErr)
				}
			})
		}
	}
}

// TestDoCommandsSurfaceDaemonNon200: a non-200 is the command's error — the
// daemon's body, single-line and capped at 200 bytes — and never a rendered
// line; at the command level it is stderr ✗ + exit 1 with nothing on stdout.
// The pin family prefixes the status; the health operations surface the body.
func TestDoCommandsSurfaceDaemonNon200(t *testing.T) {
	plainText(t)
	cases := []struct {
		name    string
		status  int
		body    string
		call    adminCall
		wantErr string
	}{
		{"pin", http.StatusBadRequest, `{"error":"cannot pin: no target"}`, adminCalls[0], `{"error":"cannot pin: no target"}`},
		{"unpin", http.StatusInternalServerError, "boom", adminCalls[1], "daemon returned HTTP 500: boom"},
		{"list pins", http.StatusNotFound, "nope", adminCalls[2], "daemon returned HTTP 404: nope"},
		{"freeze", http.StatusBadRequest, `{"error":"provider is required"}`, adminCalls[4], `{"error":"provider is required"}`},
		{"long body is capped at 200 bytes", http.StatusInternalServerError, strings.Repeat("x", 300), adminCalls[3], strings.Repeat("x", 200) + "..."},
		{"multi-line body stays one line", http.StatusBadGateway, "<html>\r\n  <body>blocked</body>\n</html>", adminCalls[3], "<html>   <body>blocked</body> </html>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			out, err := tc.call.call(srv.URL)
			if out != "" || err == nil || err.Error() != tc.wantErr {
				t.Fatalf("out=%q err=%v, want no output and err=%q", out, err, tc.wantErr)
			}
		})
	}

	t.Run("command level", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			clitest.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "provider is required"})
		}))
		defer srv.Close()
		cfg := pinDaemonConfig(t, srv)
		stdout, stderr, code := clitest.RunCLI(t, "freeze", cfg, "zhipu")
		if code != 1 || stdout != "" || stderr != "✗ {\"error\":\"provider is required\"}\n" {
			t.Fatalf("freeze on a daemon 400: exit=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
	})
}
