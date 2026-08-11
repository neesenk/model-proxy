package cli_test

import (
	"testing"

	"model-proxy/internal/cli"
)

func TestHealthLabel(t *testing.T) {
	cases := []struct {
		name string
		h    cli.StatusHealth
		want string
	}{
		{"open", cli.StatusHealth{CircuitState: "open"}, "circuit open"},
		{"half", cli.StatusHealth{CircuitState: "half_open"}, "half-open"},
		{"rate", cli.StatusHealth{RateLimitedUntil: "x"}, "rate-limited"},
		{"avail", cli.StatusHealth{Available: true}, "available"},
		{"unavail", cli.StatusHealth{}, "unavailable"},
	}
	for _, c := range cases {
		got, _ := cli.HealthLabel(c.h)
		if got != c.want {
			t.Errorf("cli.HealthLabel(%s)=%q want %q", c.name, got, c.want)
		}
	}
}
