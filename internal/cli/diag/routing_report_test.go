package diag

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/cli/clitest"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
)

func TestCmdRoutingReport_WeakLabelsAndCost(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{
		RequestLog: configdomain.RequestLogConfig{Dir: dir},
		Prices: map[string]configdomain.PriceConfig{
			"cheap":  {Input: 1.0, Output: 2.0},
			"strong": {Input: 5.0, Output: 10.0},
		},
	}
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		// Business request: cheap grade, 200, usage 100in/50out -> cost $0.0002
		{
			Ts: base.Format(time.RFC3339), RequestID: "req-1", SessionID: "s1",
			TurnKey: "t1", Exposed: "coding-auto", Provider: "p", UpstreamModel: "cheap",
			Status: 200, ResponseSize: 100,
			ResponseBody: `{"usage":{"prompt_tokens":100,"completion_tokens":50}}`,
			Routing: &configdomain.RoutingDecision{
				Source: "band", Grade: "cheap",
			},
		},
		// Retry of the same turn within window -> no_retry=false for req-1.
		{
			Ts: base.Add(2 * time.Minute).Format(time.RFC3339), RequestID: "req-2", SessionID: "s1",
			TurnKey: "t1", Exposed: "coding-auto", Provider: "p", UpstreamModel: "cheap",
			Status: 200, ResponseSize: 100,
			ResponseBody: `{"usage":{"prompt_tokens":100,"completion_tokens":50}}`,
			Routing:      &configdomain.RoutingDecision{Source: "band", Grade: "cheap"},
		},
		// Strong grade, 200 -> weak_ok.
		{
			Ts: base.Add(10 * time.Minute).Format(time.RFC3339), RequestID: "req-3", SessionID: "s2",
			TurnKey: "t2", Exposed: "coding-auto", Provider: "p", UpstreamModel: "strong",
			Status: 200, ResponseSize: 200,
			ResponseBody: `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			Routing: &configdomain.RoutingDecision{
				Source: "selector", Grade: "strong",
				Selector: &configdomain.SelectorChoice{Choice: "strong", Confidence: 0.8, Difficulty: 0.6, Enforced: true},
			},
		},
		// Later latch in same session -> no_escalation=false for req-3.
		{
			Ts: base.Add(20 * time.Minute).Format(time.RFC3339), RequestID: "req-4", SessionID: "s2",
			TurnKey: "t3", Exposed: "coding-auto", Provider: "p", UpstreamModel: "strong",
			Status: 200, ResponseSize: 50,
			ResponseBody: `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			Routing:      &configdomain.RoutingDecision{Source: "latch", Grade: "strong", Latch: "strong"},
		},
		// Shadow and decision records must be excluded from business counts.
		{Ts: base.Format(time.RFC3339), RequestID: "shadow-1", SessionID: "s1", Status: 200, ResponseSize: 10, Shadow: true},
		{Ts: base.Format(time.RFC3339), RequestID: "route-select-1", Status: 200, ResponseSize: 1, LatencyMs: 50,
			ResponseBody: `{"usage":{"prompt_tokens":3,"completion_tokens":2}}`},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() {
		CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339), "--retry-window", "5m"}, cfg)
	})

	if !strings.Contains(out, "Weak labels by route/grade") {
		t.Fatalf("missing weak labels header:\n%s", out)
	}
	// req-1: ok_basic=true, no_retry=false (req-2 within 5m), no_esc=true -> weak_ok=false
	// req-2: ok_basic=true, no_retry=true (no later retry), no_esc=true -> weak_ok=true
	// req-3: ok_basic=true, no_retry=true, no_esc=false (req-4 latch) -> weak_ok=false
	// req-4: ok_basic=true, no_retry=true, no_esc=true -> weak_ok=true
	if !strings.Contains(out, "coding-auto") {
		t.Errorf("missing route name:\n%s", out)
	}
	if !strings.Contains(out, "WEAK_OK") {
		t.Errorf("missing WEAK_OK column:\n%s", out)
	}
	// Cost: cheap records cost per 1M tokens: input $1, output $2.
	// req-1/req-2 each 100in/50out -> $0.0001 + $0.0001 = $0.0002 each -> $0.0004 for cheap.
	// req-3/req-4 strong 10in/5out -> $0.00005 + $0.00005 = $0.0001 each -> $0.0002 for strong.
	// Baseline for all four at strong max price (input $5, output $10 per 1M):
	// req-1/2: 100*5e-6 + 50*10e-6 = $0.001 each -> $0.002
	// req-3/4: 10*5e-6 + 5*10e-6 = $0.0001 each -> $0.0002
	// total baseline $0.0022, actual $0.0006 -> savings 72.7%.
	if !strings.Contains(out, "savings vs baseline: 72.7%") {
		t.Errorf("expected exact savings line:\n%s", out)
	}
	// The text renderer rounds to cents; pin the sub-cent figures through the
	// JSON projection of the same report.
	jsonOut := clitest.GrabStdout(t, func() {
		CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339), "--retry-window", "5m", "--json"}, cfg)
	})
	var report routingReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, jsonOut)
	}
	if math.Abs(report.Cost.ActualUSD-0.0006) > 1e-9 {
		t.Errorf("ActualUSD = %v, want 0.0006", report.Cost.ActualUSD)
	}
	if math.Abs(report.Cost.BaselineUSD-0.0022) > 1e-9 {
		t.Errorf("BaselineUSD = %v, want 0.0022", report.Cost.BaselineUSD)
	}
	if math.Abs(report.Cost.SavingsPct-72.72727272727273) > 1e-9 {
		t.Errorf("SavingsPct = %v, want 72.727...", report.Cost.SavingsPct)
	}
	if !strings.Contains(out, "Decision overhead") {
		t.Errorf("missing decision overhead section:\n%s", out)
	}
	if !strings.Contains(out, "decision requests: 1") {
		t.Errorf("expected 1 decision request:\n%s", out)
	}
	if !strings.Contains(out, "Note: weak labels are biased") {
		t.Errorf("missing disclaimer:\n%s", out)
	}
}

func TestCmdRoutingReport_SelectorMatrix(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
	base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		{
			Ts: base.Format(time.RFC3339), RequestID: "r1", SessionID: "s1", TurnKey: "t1",
			Exposed: "auto", Provider: "p", UpstreamModel: "cheap", Status: 200, ResponseSize: 10,
			Routing: &configdomain.RoutingDecision{
				Source: "selector", Grade: "cheap",
				Selector: &configdomain.SelectorChoice{Choice: "cheap", Confidence: 0.9, Difficulty: 0.2, Enforced: true},
			},
		},
		{
			Ts: base.Add(time.Minute).Format(time.RFC3339), RequestID: "r2", SessionID: "s2", TurnKey: "t2",
			Exposed: "auto", Provider: "p", UpstreamModel: "strong", Status: 200, ResponseSize: 10,
			Routing: &configdomain.RoutingDecision{
				Source: "selector", Grade: "strong",
				Selector: &configdomain.SelectorChoice{Choice: "strong", Confidence: 0.5, Difficulty: 0.8, Enforced: true},
			},
		},
		{
			Ts: base.Add(2 * time.Minute).Format(time.RFC3339), RequestID: "r3", SessionID: "s3", TurnKey: "t3",
			Exposed: "auto", Provider: "p", UpstreamModel: "cheap", Status: 200, ResponseSize: 10,
			Routing: &configdomain.RoutingDecision{
				Source: "selector", Grade: "cheap",
				Selector: &configdomain.SelectorChoice{Choice: "cheap", Confidence: 0.4, Difficulty: 0.4, Enforced: false},
			},
		},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339)}, cfg) })
	if !strings.Contains(out, "Selector enforce matrix") {
		t.Fatalf("missing enforce matrix header:\n%s", out)
	}
	if !strings.Contains(out, "Selector shadow matrix") {
		t.Fatalf("missing shadow matrix header:\n%s", out)
	}
	if !strings.Contains(out, "low") || !strings.Contains(out, "high") {
		t.Errorf("missing difficulty buckets:\n%s", out)
	}
}

func TestCmdRoutingReport_EmptyAndDisabledPaths(t *testing.T) {
	// Missing request-log directory.
	missingDir := filepath.Join(t.TempDir(), "missing")
	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: missingDir}}
	out := clitest.GrabStdout(t, func() { CmdRoutingReport(nil, cfg) })
	if !strings.Contains(out, "no request log directory") {
		t.Errorf("expected no-log-dir hint, got:\n%s", out)
	}

	// Existing directory but no index yet.
	cfg2 := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: t.TempDir()}}
	out = clitest.GrabStdout(t, func() { CmdRoutingReport(nil, cfg2) })
	if !strings.Contains(out, "no request log index yet") {
		t.Errorf("expected no-index hint, got:\n%s", out)
	}

	// Existing directory and index but no business records.
	dir := t.TempDir()
	cfg3 := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
	writeRoutingFixture(t, dir, []requestlog.Record{
		{Ts: time.Now().UTC().Format(time.RFC3339), RequestID: "shadow-1", Shadow: true, Status: 200},
	})
	out = clitest.GrabStdout(t, func() { CmdRoutingReport(nil, cfg3) })
	if !strings.Contains(out, "no routing business requests in range") {
		t.Errorf("expected empty hint, got:\n%s", out)
	}
}

func TestCmdRoutingReport_JSON(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
	base := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		{
			Ts: base.Format(time.RFC3339), RequestID: "r1", SessionID: "s1", TurnKey: "t1",
			Exposed: "auto", Provider: "p", UpstreamModel: "m", Status: 200, ResponseSize: 10,
			ResponseBody: `{"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
			Routing:      &configdomain.RoutingDecision{Source: "band", Grade: "g1"},
		},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() {
		CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339), "--json"}, cfg)
	})
	var report routingReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, out)
	}
	if report.BusinessRequests != 1 {
		t.Errorf("BusinessRequests = %d, want 1", report.BusinessRequests)
	}
	if len(report.WeakLabelSummary.ByRoute) != 1 {
		t.Errorf("ByRoute = %d, want 1", len(report.WeakLabelSummary.ByRoute))
	}
}

func TestCmdRoutingReport_DiagnosticsErrorBreaksWeakOK(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
	base := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		{
			Ts: base.Format(time.RFC3339), RequestID: "r1", SessionID: "s1", TurnKey: "t1",
			Exposed: "auto", Provider: "p", UpstreamModel: "m", Status: 200, ResponseSize: 10,
			Routing:     &configdomain.RoutingDecision{Source: "band", Grade: "g1"},
			Diagnostics: []requestlog.ConversionDiagnostic{{Code: "upstream_error", Detail: "boom"}},
		},
		{
			Ts: base.Add(time.Minute).Format(time.RFC3339), RequestID: "r2", SessionID: "s2", TurnKey: "t2",
			Exposed: "auto", Provider: "p", UpstreamModel: "m", Status: 200, ResponseSize: 10,
			Routing:     &configdomain.RoutingDecision{Source: "band", Grade: "g1"},
			Diagnostics: []requestlog.ConversionDiagnostic{{Code: "stop_dropped", Detail: "dropping stop"}},
		},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339)}, cfg) })
	if !strings.Contains(out, "WEAK_OK") {
		t.Fatalf("missing WEAK_OK column:\n%s", out)
	}
	lines := strings.Split(out, "\n")
	var found bool
	for _, line := range lines {
		if strings.Contains(line, "auto") && strings.Contains(line, "g1") {
			found = true
			// Two samples; upstream_error row has weak_ok=0, stop_dropped row has weak_ok=1.
			if !strings.Contains(line, "2") {
				t.Errorf("expected 2 samples in route/grade row, got:\n%s", line)
			}
		}
	}
	if !found {
		t.Errorf("did not find aggregated route/grade row:\n%s", out)
	}
}

func TestCmdRoutingReport_GradeBaselineFromConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{
		RequestLog: configdomain.RequestLogConfig{Dir: dir},
		Prices: map[string]configdomain.PriceConfig{
			"cheap-model":  {Input: 1.0, Output: 2.0},
			"strong-model": {Input: 10.0, Output: 20.0},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"coding-auto": {
				Grades: map[string][]configdomain.RouteTarget{
					"cheap":  {{Provider: "p", Model: "cheap-model"}},
					"strong": {{Provider: "p", Model: "strong-model"}},
				},
			},
		},
	}
	base := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		{
			Ts: base.Format(time.RFC3339), RequestID: "r1", SessionID: "s1", TurnKey: "t1",
			Exposed: "coding-auto", Provider: "p", UpstreamModel: "cheap-model", Status: 200, ResponseSize: 10,
			ResponseBody: `{"usage":{"prompt_tokens":1000,"completion_tokens":500}}`,
			Routing:      &configdomain.RoutingDecision{Source: "band", Grade: "cheap"},
		},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339)}, cfg) })
	// Actual: 1000*1e-6*1 + 500*1e-6*2 = $0.002
	// Baseline (strong-model): 1000*1e-6*10 + 500*1e-6*20 = $0.02
	// savings = (0.02 - 0.002) / 0.02 = 90%.
	if !strings.Contains(out, "savings vs baseline: 90.0%") {
		t.Errorf("expected exact savings line:\n%s", out)
	}
	jsonOut := clitest.GrabStdout(t, func() {
		CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339), "--json"}, cfg)
	})
	var report routingReport
	if err := json.Unmarshal([]byte(jsonOut), &report); err != nil {
		t.Fatalf("JSON parse: %v\n%s", err, jsonOut)
	}
	if math.Abs(report.Cost.ActualUSD-0.002) > 1e-9 {
		t.Errorf("ActualUSD = %v, want 0.002", report.Cost.ActualUSD)
	}
	if math.Abs(report.Cost.BaselineUSD-0.02) > 1e-9 {
		t.Errorf("BaselineUSD = %v, want 0.02", report.Cost.BaselineUSD)
	}
	if math.Abs(report.Cost.SavingsPct-90.0) > 1e-9 {
		t.Errorf("SavingsPct = %v, want 90", report.Cost.SavingsPct)
	}
}

func writeRoutingFixture(t *testing.T, dir string, records []requestlog.Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	name := "requests-20260920.log"
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	for i := range records {
		if err := enc.Encode(&records[i]); err != nil {
			t.Fatalf("encode record %d: %v", i, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(buf.String()), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}

	// Build the SQLite index from the JSONL file.
	indexer, err := requestlog.NewIndexer(dir)
	if err != nil {
		t.Fatalf("NewIndexer: %v", err)
	}
	go indexer.Run()
	if err := indexer.WaitReconciled(t.Context()); err != nil {
		t.Fatalf("WaitReconciled: %v", err)
	}
	indexer.Shutdown()
}

// TestCmdRoutingReport_EvalVerdicts verifies that shadow records carrying
// eval_verdict diagnostics are aggregated into the L2 eval verdict table.
func TestCmdRoutingReport_EvalVerdicts(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{RequestLog: configdomain.RequestLogConfig{Dir: dir}}
	base := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	records := []requestlog.Record{
		// Two eval shadow samples for the same route/grade pair with different verdicts.
		{
			Ts: base.Format(time.RFC3339), RequestID: "shadow-req-1", SessionID: "s1",
			Exposed: "coding-auto", Provider: "p", UpstreamModel: "strong-model",
			Status: 200, ResponseSize: 10, Shadow: true,
			Diagnostics: []requestlog.ConversionDiagnostic{
				{Code: "eval_shadow_grade", Detail: "strong"},
				{Code: "eval_verdict", Detail: "shadow_better"},
			},
			Routing: &configdomain.RoutingDecision{Source: "band", Grade: "cheap"},
		},
		{
			Ts: base.Add(time.Minute).Format(time.RFC3339), RequestID: "shadow-req-2", SessionID: "s2",
			Exposed: "coding-auto", Provider: "p", UpstreamModel: "strong-model",
			Status: 200, ResponseSize: 10, Shadow: true,
			Diagnostics: []requestlog.ConversionDiagnostic{
				{Code: "eval_shadow_grade", Detail: "strong"},
				{Code: "eval_verdict", Detail: "tie"},
			},
			Routing: &configdomain.RoutingDecision{Source: "band", Grade: "cheap"},
		},
		// A regular shadow record without eval verdict is ignored.
		{
			Ts: base.Add(2 * time.Minute).Format(time.RFC3339), RequestID: "shadow-req-3", SessionID: "s3",
			Exposed: "coding-auto", Provider: "p", UpstreamModel: "strong-model",
			Status: 200, ResponseSize: 10, Shadow: true,
		},
	}
	writeRoutingFixture(t, dir, records)

	out := clitest.GrabStdout(t, func() { CmdRoutingReport([]string{"--since", base.Add(-time.Hour).Format(time.RFC3339)}, cfg) })

	if !strings.Contains(out, "L2 eval verdicts") {
		t.Fatalf("missing L2 eval verdicts header:\n%s", out)
	}
	// One shadow_better + one tie aggregated by route/primary/shadow/verdict.
	if !strings.Contains(out, "shadow_better") {
		t.Errorf("missing shadow_better verdict:\n%s", out)
	}
	if !strings.Contains(out, "tie") {
		t.Errorf("missing tie verdict:\n%s", out)
	}
}
