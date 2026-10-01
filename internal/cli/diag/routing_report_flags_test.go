package diag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
)

// routingCfg builds an in-process config whose request log lives in dir.
func routingCfg(dir string) *configdomain.Config {
	return &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
}

// businessRecord builds one business request record on route at ts.
func businessRecord(ts time.Time, route string) requestlog.Record {
	return requestlog.Record{
		Ts: ts.Format(time.RFC3339), RequestID: "req-" + route + ts.Format("150405"),
		SessionID: "s-" + route, TurnKey: "t", Exposed: route,
		Provider: "p", UpstreamModel: "m", Status: 200, ResponseSize: 10,
		Routing: &configdomain.RoutingDecision{Source: "band", Grade: "g"},
	}
}

// TestCmdRoutingReport_RouteFilter pins the --route flag: the query narrows to
// one route's records, so the other route's rows disappear from BOTH the weak
// label and cost sections (queryRoutingRows' `AND exposed = ?`).
func TestCmdRoutingReport_RouteFilter(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	writeRoutingFixture(t, dir, []requestlog.Record{
		businessRecord(now.Add(-10*time.Minute), "alpha"),
		businessRecord(now.Add(-11*time.Minute), "beta"),
	})
	cfg := routingCfg(dir)
	since := now.Add(-time.Hour).Format(time.RFC3339)

	// Unfiltered: both routes appear.
	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", since}, cfg) })
	if !strings.Contains(out, "alpha") || !strings.Contains(out, "beta") {
		t.Fatalf("unfiltered report must show both routes:\n%s", out)
	}

	// --route alpha: only alpha remains; beta must vanish entirely.
	out = clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", since, "--route", "alpha"}, cfg) })
	if !strings.Contains(out, "alpha") {
		t.Fatalf("--route alpha lost its own route:\n%s", out)
	}
	if strings.Contains(out, "beta") {
		t.Errorf("--route alpha must exclude beta's rows:\n%s", out)
	}
}

// TestCmdRoutingReport_SinceDurationForm pins the --since Go-duration form
// (CLI.md §22: `--since DUR|TIME`): "1h" is now-1h, so a fresh record is
// inside the window and a 3h-old record falls out of it.
func TestCmdRoutingReport_SinceDurationForm(t *testing.T) {
	now := time.Now().UTC()

	dir := t.TempDir()
	writeRoutingFixture(t, dir, []requestlog.Record{businessRecord(now.Add(-30*time.Minute), "fresh")})
	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", "1h"}, routingCfg(dir)) })
	if !strings.Contains(out, "fresh") {
		t.Errorf("--since 1h must include a 30m-old record:\n%s", out)
	}

	dir = t.TempDir()
	writeRoutingFixture(t, dir, []requestlog.Record{businessRecord(now.Add(-3*time.Hour), "stale")})
	out = clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", "1h"}, routingCfg(dir)) })
	if !strings.Contains(out, "no routing business requests in range") {
		t.Errorf("--since 1h must exclude a 3h-old record:\n%s", out)
	}
}

// routingReportConfigPath writes a loadable config pointing request_log.dir at
// dir, for the subprocess (os.Exit) tests. A provider entry is required —
// config load fails before the command runs without one.
func routingReportConfigPath(t *testing.T, dir string) string {
	t.Helper()
	return clitest.WriteTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  aqp: {provider_id: aqp, openai_base_url: https://example.invalid/compass-api/v1}\nrequest_log:\n  dir: "+dir+"\n")
}

// TestCLI_RoutingReportBadIndex pins the documented failure path (CLI.md §22):
// a request log dir whose index.db is not a SQLite database must exit 1 with
// `✗ cannot open request log index: <ERR>` instead of rendering an empty
// report.
func TestCLI_RoutingReportBadIndex(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.db"), []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := routingReportConfigPath(t, dir)

	_, stderr, code := clitest.RunCLI(t, "routing", cfgPath, "report")
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(stderr, "cannot open request log index") {
		t.Errorf("stderr = %q, want the cannot-open-index error", stderr)
	}
}

// TestCLI_RoutingReportInvalidFlags pins the usage errors: an unparsable
// --since or --retry-window exits 1 with the exact guidance line.
func TestCLI_RoutingReportInvalidFlags(t *testing.T) {
	cfgPath := routingReportConfigPath(t, t.TempDir())

	_, stderr, code := clitest.RunCLI(t, "routing", cfgPath, "report", "--since", "bogus")
	if code != 1 || !strings.Contains(stderr, `invalid --since "bogus" (use RFC3339 or a Go duration like 7d, 24h)`) {
		t.Fatalf("--since bogus: exit=%d stderr=%q", code, stderr)
	}

	_, stderr, code = clitest.RunCLI(t, "routing", cfgPath, "report", "--retry-window", "bogus")
	if code != 1 || !strings.Contains(stderr, `invalid --retry-window "bogus" (use a Go duration like 10m)`) {
		t.Fatalf("--retry-window bogus: exit=%d stderr=%q", code, stderr)
	}
}
