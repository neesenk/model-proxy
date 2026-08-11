package provider

import (
	"io"
	"os"
	"sort"
	"testing"
	"time"
)

// usage_display_test.go covers the shared display helpers (printQuotaSnapshot,
// printAFPWindow, aqpUsageLine, listConfigModels, printUsageFields) moved from
// main in Phase 3. In package provider, the unexported helpers are called
// directly. Color is forced off via SetColorEnabled.

func init() { SetColorEnabled(false) }

func TestPrintQuotaSnapshot(t *testing.T) {
	s := &QuotaSnapshot{
		Billing: BillingPlan,
		Account: "a@b.com",
		Plan:    "pro",
		Windows: []QuotaWindow{
			{Label: "5h tokens", Kind: "tokens", Used: 40, Total: 100, RemainingPct: 0.6,
				DetailLabel: "By model",
				Details:     []QuotaDetail{{Label: "glm-5.2", Used: 30000}},
				ResetsAt:    time.Now().Add(1 * time.Hour)},
			{Label: "Weekly tokens", Kind: "tokens", Used: 70, Total: 100, RemainingPct: 0.3},
		},
		Notes: []string{"Rate Limit: allowed"},
	}
	out := captureProv(t, func() { printQuotaSnapshot(s) })
	for _, want := range []string{"5h tokens", "Weekly tokens", "40 used / 100 total",
		"By model", "glm-5.2", "Rate Limit: allowed", "40.0% used"} {
		if !contains(out, want) {
			t.Errorf("printQuotaSnapshot missing %q:\n%s", want, out)
		}
	}
}

func TestPrintQuotaSnapshot_UnmeasuredWindow(t *testing.T) {
	s := &QuotaSnapshot{
		Billing: BillingPayG,
		Windows: []QuotaWindow{
			{Label: "Balance", Kind: "money", Total: 10.5, RemainingPct: -1},
		},
	}
	out := captureProv(t, func() { printQuotaSnapshot(s) })
	if contains(out, "200% used") {
		t.Errorf("unmeasured window rendered as '200%% used':\n%s", out)
	}
	if !contains(out, "unmeasured") {
		t.Errorf("unmeasured window missing 'unmeasured' label:\n%s", out)
	}
}

func TestPrintAFPWindow(t *testing.T) {
	w := AfpWindow{Quota: 100, Used: 30, ResetTime: time.Now().Add(2 * time.Hour).UnixMilli()}
	out := captureProv(t, func() { printAFPWindow("5h", w) })
	for _, want := range []string{"5h", "30.0% used", "30.0 used / 100.0 quota", "70.0 remaining"} {
		if !contains(out, want) {
			t.Errorf("printAFPWindow missing %q:\n%s", want, out)
		}
	}
	out2 := captureProv(t, func() { printAFPWindow("daily", AfpWindow{}) })
	if !contains(out2, "0.0% used") {
		t.Errorf("printAFPWindow zero-quota missing '0.0%% used':\n%s", out2)
	}
	if !contains(out2, "0.0 used / 0.0 quota, 0.0 remaining") {
		t.Errorf("printAFPWindow zero-quota missing zero line:\n%s", out2)
	}
}

func TestAqpUsageLine(t *testing.T) {
	mu := &MonthlyProjectUsage{SelectedYear: 2026, SelectedMonth: 7,
		TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	var line string
	captureProv(t, func() { line = aqpUsageLine(mu) })
	for _, want := range []string{"[", "]", "30.0% used", "$30.00 / $100.00",
		"balance $70.00", "CQP", "2026-07"} {
		if !contains(line, want) {
			t.Errorf("aqpUsageLine missing %q: %s", want, line)
		}
	}
	if !contains(line, "30.0% used · $30.00 / $100.00") {
		t.Errorf("aqpUsageLine order/separator wrong: %s", line)
	}
}

func TestListConfigModels(t *testing.T) {
	out := captureProv(t, func() { listConfigModels([]string{"doubao", "glm-5.2", "kimi"}) })
	if !contains(out, "3 models (from config)") {
		t.Errorf("missing count line:\n%s", out)
	}
	idxD := idx(out, "doubao")
	idxG := idx(out, "glm-5.2")
	idxK := idx(out, "kimi")
	if idxD < 0 || idxG < 0 || idxK < 0 {
		t.Fatalf("missing a model id:\n%s", out)
	}
	if !(idxD < idxG && idxG < idxK) {
		t.Errorf("models not sorted: doubao=%d glm-5.2=%d kimi=%d", idxD, idxG, idxK)
	}
}

func TestPrintUsageFields(t *testing.T) {
	m := map[string]any{
		"str":   "hello",
		"num":   float64(42),
		"flag":  true,
		"obj":   map[string]any{"x": float64(1)},
		"other": []any{1, 2},
	}
	out := captureProv(t, func() { printUsageFields(m, 0) })
	for _, want := range []string{"str", "hello", "num", "42", "flag", "true", "obj", "other"} {
		if !contains(out, want) {
			t.Errorf("printUsageFields missing %q:\n%s", want, out)
		}
	}
}

// keep sort referenced (listConfigModels uses it).
var _ = sort.Strings

// --- test helpers ---

// captureStdoutProvider runs fn with os.Stdout redirected to a pipe and returns
// what was written. Color is forced off (init sets ColorEnabled=false).
func captureStdoutProvider(fn func()) string {
	oldOut := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		panic(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	os.Stdout = oldOut
	return <-done
}

func captureProv(t *testing.T, fn func()) string {
	t.Helper()
	// Reuse the main-package captureStdout pattern: redirect os.Stdout via a
	// pipe. Color is already forced off in init().
	return captureStdoutProvider(fn)
}

func contains(s, sub string) bool { return idx(s, sub) >= 0 }
func idx(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
