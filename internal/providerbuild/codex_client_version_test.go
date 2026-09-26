package providerbuild

import (
	"testing"
)

func TestResolveCodexClientVersion(t *testing.T) {
	cli := func() string { return "0.144.1" }
	cache := func() string { return "0.130.0" }

	// nil probes are "no answer", not a panic — offline callers (takeover's
	// AuthenticatedProvidersForHome) pass zero BuildOptions and must not need
	// the process/filesystem probes just to fold a codex provider into the
	// authenticated set.
	if got := ResolveCodexClientVersion("", nil, nil); got != DefaultCodexClientVersion {
		t.Errorf("nil probes should fall back to the constant: got %q want %q", got, DefaultCodexClientVersion)
	}
	if got := ResolveCodexClientVersion("0.200.0", nil, nil); got != "0.200.0" {
		t.Errorf("config should win over nil probes: got %q", got)
	}

	// config value wins.
	if got := ResolveCodexClientVersion("0.200.0", cli, cache); got != "0.200.0" {
		t.Errorf("config should win: got %q want 0.200.0", got)
	}
	// config empty -> cli.
	if got := ResolveCodexClientVersion("", cli, cache); got != "0.144.1" {
		t.Errorf("cli fallback: got %q want 0.144.1", got)
	}
	// config + cli empty -> cache.
	if got := ResolveCodexClientVersion("", func() string { return "" }, cache); got != "0.130.0" {
		t.Errorf("cache fallback: got %q want 0.130.0", got)
	}
	// all empty -> constant.
	if got := ResolveCodexClientVersion("", func() string { return "" }, func() string { return "" }); got != DefaultCodexClientVersion {
		t.Errorf("constant fallback: got %q want %q", got, DefaultCodexClientVersion)
	}
	// whitespace-only config is treated as empty.
	if got := ResolveCodexClientVersion("  ", cli, cache); got != "0.144.1" {
		t.Errorf("whitespace config should fall through: got %q", got)
	}
}

func TestParseSemver(t *testing.T) {
	cases := []struct{ in, want string }{
		{"codex 0.144.1", "0.144.1"},
		{"codex 0.144.1\n", "0.144.1"},
		{"codex 1.0.0 (abcd123) ", "1.0.0"},
		{"no version here", ""},
	}
	for _, c := range cases {
		if got := ParseSemver(c.in); got != c.want {
			t.Errorf("ParseSemver(%q): got %q want %q", c.in, got, c.want)
		}
	}
}
