package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- util.go ---

func TestEnvOrEmpty(t *testing.T) {
	if got := envOrEmpty("MP_TEST_UNSET_VAR"); got != "" {
		t.Errorf("envOrEmpty(unset)=%q want empty", got)
	}
	t.Setenv("MP_TEST_SET", "hello")
	if got := envOrEmpty("MP_TEST_SET"); got != "hello" {
		t.Errorf("envOrEmpty(set)=%q want hello", got)
	}
}

func TestRuntimeOS(t *testing.T) {
	if got := runtimeOS(); got != runtime.GOOS {
		t.Errorf("runtimeOS()=%q want %q", got, runtime.GOOS)
	}
}

func TestReadFile_Missing(t *testing.T) {
	if _, err := readFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("readFile(missing): want error, got nil")
	}
}

func TestWriteFile_ReadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := writeFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readFile(p)
	if err != nil || string(got) != "hi" {
		t.Errorf("round-trip: got=%q err=%v", got, err)
	}
}

func TestMask_ShortAndEmpty(t *testing.T) {
	if got := mask(""); got != "(empty)" {
		t.Errorf("mask(empty)=%q want (empty)", got)
	}
	if got := mask("short"); got != "****" {
		t.Errorf("mask(short)=%q want ****", got)
	}
	if got := mask("ab"); got != "****" {
		t.Errorf("mask(2-char)=%q want ****", got)
	}
}

func TestMask_Long(t *testing.T) {
	if got := mask("abcdefghijklmnop"); got != "ab…op" {
		t.Errorf("mask(long)=%q want ab…op", got)
	}
}

func TestAuthFilePath(t *testing.T) {
	got := authFilePath("zhipu-work", "apikey")
	if !contains(got, "zhipu-work_apikey.json") || !contains(got, ".model-proxy") {
		t.Errorf("authFilePath=%q want zhipu-work_apikey.json under .model-proxy", got)
	}
}

// --- color.go: cYellow/cMagenta via the color-disabled path ---

func TestColorHelpers_NoColorPassthrough(t *testing.T) {
	// colorEnabled reflects os.Stdout at init. In tests stdout is not a tty
	// (and NO_COLOR may be set), so color helpers return the input verbatim
	// (no ANSI escape codes). Assert the EXACT string — not Contains (which
	// would pass even with ANSI codes wrapping the input).
	for _, s := range []string{"x", "hello", "test-123"} {
		if got := cYellow(s); got != s {
			t.Errorf("cYellow(%q)=%q, want exact %q (no ANSI in test env)", s, got, s)
		}
		if got := cMagenta(s); got != s {
			t.Errorf("cMagenta(%q)=%q, want exact %q", s, got, s)
		}
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
