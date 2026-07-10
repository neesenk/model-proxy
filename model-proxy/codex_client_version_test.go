package main

import "testing"

func TestResolveCodexClientVersion(t *testing.T) {
	cli := func() string { return "0.144.1" }
	cache := func() string { return "0.130.0" }

	// config value wins.
	if got := resolveCodexClientVersion("0.200.0", cli, cache); got != "0.200.0" {
		t.Errorf("config should win: got %q want 0.200.0", got)
	}
	// config empty -> cli.
	if got := resolveCodexClientVersion("", cli, cache); got != "0.144.1" {
		t.Errorf("cli fallback: got %q want 0.144.1", got)
	}
	// config + cli empty -> cache.
	if got := resolveCodexClientVersion("", func() string { return "" }, cache); got != "0.130.0" {
		t.Errorf("cache fallback: got %q want 0.130.0", got)
	}
	// all empty -> constant.
	if got := resolveCodexClientVersion("", func() string { return "" }, func() string { return "" }); got != defaultCodexClientVersion {
		t.Errorf("constant fallback: got %q want %q", got, defaultCodexClientVersion)
	}
	// whitespace-only config is treated as empty.
	if got := resolveCodexClientVersion("  ", cli, cache); got != "0.144.1" {
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
		if got := parseSemver(c.in); got != c.want {
			t.Errorf("parseSemver(%q): got %q want %q", c.in, got, c.want)
		}
	}
}
