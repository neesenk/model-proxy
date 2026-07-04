package provider

import "testing"

func TestBindingRemaining(t *testing.T) {
	cases := []struct {
		name    string
		windows []QuotaWindow
		want    float64
	}{
		{"empty", nil, -1},
		{"single", []QuotaWindow{{RemainingPct: 0.8}}, 0.8},
		{"min wins", []QuotaWindow{{RemainingPct: 0.8}, {RemainingPct: 0.3}, {RemainingPct: 0.9}}, 0.3},
		{"skip unmeasured", []QuotaWindow{{RemainingPct: -1}, {RemainingPct: 0.5}}, 0.5},
		{"all unmeasured", []QuotaWindow{{RemainingPct: -1}, {RemainingPct: -1}}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BindingRemaining(tc.windows); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaOrUnknown(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.QuotaOrUnknown()
	if err != nil {
		t.Fatalf("nil QuotaFn should not error, got %v", err)
	}
	if got.Billing != BillingUnknown {
		t.Errorf("nil QuotaFn → Billing %v, want BillingUnknown", got.Billing)
	}
}
