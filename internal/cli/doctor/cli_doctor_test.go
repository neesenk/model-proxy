package doctor_test

import (
	clidoctor "model-proxy/internal/cli/doctor"
	configdomain "model-proxy/internal/config"
	"strings"
	"testing"
)

func TestQuotaSourceLabel(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"aqp", "monthly_usage"},
		{"codex", "wham/usage"},
		{"zhipu", "quota/limit"},
		{"volcengine", "GetAFPUsage (AK/SK)"},
		{"deepseek", "user/balance"},
		{"kimi-code", "usages"},
		{"unknown", "(none → unknown at runtime)"},
	} {
		if got := clidoctor.QuotaSourceLabel(tc.id); got != tc.want {
			t.Errorf("clidoctor.QuotaSourceLabel(%q)=%q want %q", tc.id, got, tc.want)
		}
	}
}

// TestDryRunOrder: offline order is tier (plan before payg) then priority asc.
func TestDryRunOrder(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"plana": {}, "planb": {}, "payg": {Billing: "pay-as-you-go"},
	}}
	targets := []configdomain.RouteTarget{
		{Provider: "payg", Priority: 1},
		{Provider: "planb", Priority: 3},
		{Provider: "plana", Priority: 2},
	}
	got := clidoctor.DryRunOrder(cfg, targets)
	want := []string{"plana", "planb", "payg"}
	if len(got) != len(want) {
		t.Fatalf("len=%d, want %d: %+v", len(got), len(want), got)
	}
	for i, w := range want {
		if i >= len(got) || got[i].Provider != w {
			t.Errorf("pos %d: got %+v, want %q", i, got, w)
		}
	}
}

func TestPeakSummary(t *testing.T) {
	if got := clidoctor.PeakSummary(nil); got != "-" {
		t.Errorf("empty peakSummary=%q, want -", got)
	}
	got := clidoctor.PeakSummary(configdomain.PeakConfig{{Window: "09:00-12:00", Multiplier: 2}})
	if !strings.Contains(got, "09:00-12:00") || !strings.Contains(got, "×2") {
		t.Errorf("peakSummary=%q, want window + mult", got)
	}
	if got := clidoctor.PeakSummary(configdomain.PeakConfig{{Window: "09:00-12:00"}}); !strings.Contains(got, "×2") {
		t.Errorf("default multiplier: %q, want ×2", got)
	}
}
