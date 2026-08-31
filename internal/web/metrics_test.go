package web

import (
	"strings"
	"testing"

	"model-proxy/internal/appapi"
)

// escapeLabel follows the Prometheus text-exposition rules: ONLY backslash,
// double-quote and newline are escaped. Go's %q would also emit \t, \u0007
// etc., which a Prometheus parser rejects — everything else passes raw.
func TestEscapeLabelPrometheusRules(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"a\tb", "a\tb"}, // tab passes through RAW (never \t)
		{"a\ab", "a\ab"}, // bell passes through raw (never \a or \u0007)
		{"a\"b", "a\\\"b"},
		{"a\\b", "a\\\\b"},
		{"a\nb", "a\\nb"},
		{"x\\\"\ny", "x\\\\\\\"\\ny"},
	}
	for _, tc := range cases {
		if got := escapeLabel(tc.in); got != tc.want {
			t.Errorf("escapeLabel(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A provider key containing a tab, quote and backslash must produce an exact,
// parseable exposition line — raw tab, escaped quote/backslash.
func TestRenderPrometheusEscapesProviderLabel(t *testing.T) {
	name := "evil\t\"\\provider"
	v := appapi.Dashboard{
		Counters: map[string]appapi.Metrics{
			name: {Requests: 7, LatencySum: 70, TTFTSum: 35},
		},
	}
	body := renderPrometheus(v)
	wantRequests := "model_proxy_requests_total{provider=\"evil\t\\\"\\\\provider\"} 7\n"
	if !strings.Contains(body, wantRequests) {
		t.Fatalf("exposition missing exactly-escaped series line %q;\nbody:\n%s", wantRequests, body)
	}
	wantSum := "model_proxy_latency_milliseconds_sum{provider=\"evil\t\\\"\\\\provider\"} 70\n"
	if !strings.Contains(body, wantSum) {
		t.Fatalf("exposition missing exactly-escaped sum line %q;\nbody:\n%s", wantSum, body)
	}
	// A %q rendering would have escaped the tab — assert it is absent.
	if strings.Contains(body, `evil\t`) {
		t.Fatalf("exposition contains Go-style \\t escaping (invalid Prometheus);\nbody:\n%s", body)
	}
}
