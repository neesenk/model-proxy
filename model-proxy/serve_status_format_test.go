package main

import "testing"

func TestHealthLabel(t *testing.T) {
	cases := []struct {
		name string
		h    statusHealth
		want string
	}{
		{"open", statusHealth{CircuitState: "open"}, "circuit open"},
		{"half", statusHealth{CircuitState: "half_open"}, "half-open"},
		{"rate", statusHealth{RateLimitedUntil: "x"}, "rate-limited"},
		{"avail", statusHealth{Available: true}, "available"},
		{"unavail", statusHealth{}, "unavailable"},
	}
	for _, c := range cases {
		got, _ := healthLabel(c.h)
		if got != c.want {
			t.Errorf("healthLabel(%s)=%q want %q", c.name, got, c.want)
		}
	}
}
