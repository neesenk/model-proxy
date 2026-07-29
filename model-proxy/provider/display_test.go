package provider

import "testing"

// These helpers are implemented in this package and merely re-exported by the
// root command. Keep their branch assertions here so removing a root wrapper
// cannot silently discard coverage of the provider-owned behavior.
func TestFormatDuration(t *testing.T) {
	for _, tc := range []struct {
		secs int
		want string
	}{
		{-5, "—"},
		{0, "—"},
		{30, "0m"},
		{90, "1m"},
		{3700, "1h1m"},
		{7200, "2h0m"},
		{90000, "1d1h"},
		{86400, "1d0h"},
		{3 * 86400, "3d0h"},
	} {
		if got := FormatDuration(tc.secs); got != tc.want {
			t.Errorf("FormatDuration(%d)=%q want %q", tc.secs, got, tc.want)
		}
	}
}

func TestFormatWithCommas(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1,000"},
		{1234567, "1,234,567"},
		{-1234567, "-1,234,567"},
		{-999, "-999"},
	} {
		if got := FormatWithCommas(tc.n); got != tc.want {
			t.Errorf("FormatWithCommas(%d)=%q want %q", tc.n, got, tc.want)
		}
	}
}

func TestFormatCredits(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"330.258", "330"},
		{"22500", "22,500"},
		{"0.9", "0"},
		{"not-a-number", "0"},
		{"9999999", "9,999,999"},
	} {
		if got := FormatCredits(tc.in); got != tc.want {
			t.Errorf("FormatCredits(%q)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestMoney(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{12.5, "$12.50"},
		{0, "$0.00"},
	} {
		if got := Money(tc.in); got != tc.want {
			t.Errorf("Money(%v)=%q want %q", tc.in, got, tc.want)
		}
	}
}

func TestOr(t *testing.T) {
	if got := Or("", "fallback"); got != "fallback" {
		t.Errorf("Or(empty)=%q want fallback", got)
	}
	if got := Or("set", "fallback"); got != "set" {
		t.Errorf("Or(set)=%q want set", got)
	}
}

func TestMagenta_NoColorPassthrough(t *testing.T) {
	old := ColorEnabled
	SetColorEnabled(false)
	t.Cleanup(func() { SetColorEnabled(old) })
	for _, s := range []string{"x", "hello", "test-123"} {
		if got := Magenta(s); got != s {
			t.Errorf("Magenta(%q)=%q want exact %q with color disabled", s, got, s)
		}
	}
}
