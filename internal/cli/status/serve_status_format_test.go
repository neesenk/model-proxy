package status_test

import (
	"testing"

	status "model-proxy/internal/cli/status"
)

func TestHealthLabel(t *testing.T) {
	cases := []struct {
		name string
		h    status.StatusHealth
		want string
	}{
		{"open", status.StatusHealth{CircuitState: "open"}, "circuit open"},
		{"half", status.StatusHealth{CircuitState: "half_open"}, "half-open"},
		{"rate", status.StatusHealth{RateLimitedUntil: "x"}, "rate-limited"},
		{"avail", status.StatusHealth{Available: true}, "available"},
		{"unavail", status.StatusHealth{}, "unavailable"},
	}
	for _, c := range cases {
		got, _ := status.HealthLabel(c.h)
		if got != c.want {
			t.Errorf("status.HealthLabel(%s)=%q want %q", c.name, got, c.want)
		}
	}
}
