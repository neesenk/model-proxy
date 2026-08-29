package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
)

// serveShadowReport stands in for the daemon's /api/shadow-report endpoint and
// returns a config pointing CmdShadowReport at it.
func serveShadowReport(t *testing.T, enabled bool, entries []requestlog.ShadowReportEntry) *configdomain.Config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/shadow-report" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"enabled": enabled,
			"entries": entries,
		}); err != nil {
			t.Errorf("encode shadow report stub: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return &configdomain.Config{Listen: strings.TrimPrefix(srv.URL, "http://")}
}

// TestCmdShadowReport_RendersRealTable drives the production renderer (not a
// test-side re-implementation): header, every data column, compact-number
// formatting and the %-16.16s route truncation must all come out of
// CmdShadowReport itself.
func TestCmdShadowReport_RendersRealTable(t *testing.T) {
	cfg := serveShadowReport(t, true, []requestlog.ShadowReportEntry{
		{
			Route: "glm", PrimaryProvider: "zhipu", ShadowProvider: "codex",
			Samples: 5, StatusMatchRate: 0.8,
			PrimaryLatencyMs: 100, ShadowLatencyMs: 150,
			PrimarySizeAvg: 200, ShadowSizeAvg: 180,
		},
		{
			Route: "abcdefghijklmnopq", PrimaryProvider: "kimi", ShadowProvider: "deepseek",
			Samples: 1200, StatusMatchRate: 0.421,
			PrimaryLatencyMs: 9, ShadowLatencyMs: 1234,
			PrimarySizeAvg: 1500000, ShadowSizeAvg: 450000,
		},
	})
	out := grabStdout(t, func() { CmdShadowReport(nil, cfg) })
	for _, want := range []string{"ROUTE", "PRIMARY", "SHADOW", "SAMPLES", "MATCH", "P_LAT", "S_LAT", "P_BYTES", "S_BYTES"} {
		if !strings.Contains(out, want) {
			t.Errorf("shadow report header missing %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"glm", "zhipu", "codex", "   5", "80%", "100ms", "150ms", "200", "180"} {
		if !strings.Contains(out, want) {
			t.Errorf("shadow report first row missing %q:\n%s", want, out)
		}
	}
	for _, want := range []string{"kimi", "deepseek", "1200", "42%", "9ms", "1234ms", "1.5M", "450k"} {
		if !strings.Contains(out, want) {
			t.Errorf("shadow report second row missing %q:\n%s", want, out)
		}
	}
	// The 17-char route must render truncated to 16 chars (%-16.16s).
	if !strings.Contains(out, "abcdefghijklmnop ") || strings.Contains(out, "abcdefghijklmnopq") {
		t.Errorf("long route not truncated to 16 chars:\n%s", out)
	}
}

// TestCmdShadowReport_DisabledAndEmpty covers both early-return messages of
// the production renderer.
func TestCmdShadowReport_DisabledAndEmpty(t *testing.T) {
	cfg := serveShadowReport(t, false, nil)
	out := grabStdout(t, func() { CmdShadowReport(nil, cfg) })
	if !strings.Contains(out, "(request logging is off") {
		t.Errorf("disabled shadow report should print the request_log hint, got:\n%s", out)
	}

	cfg = serveShadowReport(t, true, nil)
	out = grabStdout(t, func() { CmdShadowReport(nil, cfg) })
	if !strings.Contains(out, "(no paired shadow samples in range)") {
		t.Errorf("empty shadow report should print the no-samples hint, got:\n%s", out)
	}
	if strings.Contains(out, "ROUTE") {
		t.Errorf("empty shadow report must not render a table:\n%s", out)
	}
}
