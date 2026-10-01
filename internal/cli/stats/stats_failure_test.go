package stats_test

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	stats "model-proxy/internal/cli/stats"
)

// TestCmdStatsFailureUX pins the CLI display contract for the `stats` failure
// family (CLI.md §10 “失败”): unreachable daemon, 404 (web.enabled off), any
// other non-200, and a malformed 200 body each print `✗ <ERR>` to stderr and
// exit 1. CmdStats is the process wrapper (RunStats os.Exits on its return
// value), so driving it in-process with buffers covers the exact bytes the
// subprocess would print.
func TestCmdStatsFailureUX(t *testing.T) {
	// Unreachable: bind an ephemeral port, then close it — guaranteed refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := ln.Addr().String()
	ln.Close()

	cases := []struct {
		name   string
		listen string
		serve  func(w http.ResponseWriter, r *http.Request)
		want   string
	}{
		{
			name:   "daemon unreachable",
			listen: deadAddr,
			want:   "cannot reach daemon at " + deadAddr + ": ",
		},
		{
			name:   "404 web disabled",
			listen: "", // filled from the httptest server below
			serve: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
			want: "web UI endpoints not available - is web.enabled true on the daemon?",
		},
		{
			name: "non-200 surfaces status and body",
			serve: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			want: "daemon returned HTTP 500: boom",
		},
		{
			name: "malformed 200 body",
			serve: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte("not-json{"))
			},
			want: "parse stats response: ",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listen := tc.listen
			if tc.serve != nil {
				up := httptest.NewServer(http.HandlerFunc(tc.serve))
				defer up.Close()
				listen = strings.TrimPrefix(up.URL, "http://")
			}
			var out, errb bytes.Buffer
			code := stats.CmdStats(nil, listen, &out, &errb)
			if code != 1 {
				t.Fatalf("exit = %d, want 1", code)
			}
			if out.String() != "" {
				t.Errorf("stdout = %q, want empty on failure", out.String())
			}
			if !strings.HasPrefix(errb.String(), "✗ ") {
				t.Errorf("stderr must lead with the ✗ marker: %q", errb.String())
			}
			if !strings.Contains(errb.String(), tc.want) {
				t.Errorf("stderr %q missing %q", errb.String(), tc.want)
			}
		})
	}

	// The unreachable hint also names the remedy line (CLI.md quotes it as a
	// second line of the same error).
	t.Run("daemon unreachable names the remedy", func(t *testing.T) {
		var out, errb bytes.Buffer
		if code := stats.CmdStats(nil, deadAddr, &out, &errb); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(errb.String(), "is `model-proxy serve` running?") {
			t.Errorf("stderr missing serve hint: %q", errb.String())
		}
	})
}
