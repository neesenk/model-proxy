package stats

import "testing"

// TestParseWindow pins the enumerated /api/tokens window contract: absent/all
// is the cumulative default (0), the named windows map to seconds, and any
// other value is rejected (the handler turns !ok into 400).
func TestParseWindow(t *testing.T) {
	cases := []struct {
		in      string
		seconds int64
		ok      bool
	}{
		{"", 0, true},
		{"all", 0, true},
		{"1h", 3600, true},
		{"24h", 24 * 3600, true},
		{"7d", 7 * 24 * 3600, true},
		{"60m", 0, false},
		{"1d", 0, false},
		{"ALL", 0, false},
		{" 1h", 0, false},
		{"-1h", 0, false},
	}
	for _, c := range cases {
		seconds, ok := ParseWindow(c.in)
		if seconds != c.seconds || ok != c.ok {
			t.Errorf("ParseWindow(%q) = (%d, %v), want (%d, %v)", c.in, seconds, ok, c.seconds, c.ok)
		}
	}
}
