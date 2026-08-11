package framework

import (
	"model-proxy/provider"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// --- util.go ---

func TestEnvOrEmpty(t *testing.T) {
	if got := EnvOrEmpty("MP_TEST_UNSET_VAR"); got != "" {
		t.Errorf("EnvOrEmpty(unset)=%q want empty", got)
	}
	t.Setenv("MP_TEST_SET", "hello")
	if got := EnvOrEmpty("MP_TEST_SET"); got != "hello" {
		t.Errorf("EnvOrEmpty(set)=%q want hello", got)
	}
}

func TestRuntimeOS(t *testing.T) {
	if got := RuntimeOS(); got != runtime.GOOS {
		t.Errorf("RuntimeOS()=%q want %q", got, runtime.GOOS)
	}
}

func TestReadFile_Missing(t *testing.T) {
	if _, err := ReadFile(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("ReadFile(missing): want error, got nil")
	}
}

func TestWriteFile_ReadFile_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := WriteFile(p, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(p)
	if err != nil || string(got) != "hi" {
		t.Errorf("round-trip: got=%q err=%v", got, err)
	}
}

func TestMask_ShortAndEmpty(t *testing.T) {
	if got := Mask(""); got != "(empty)" {
		t.Errorf("Mask(empty)=%q want (empty)", got)
	}
	if got := Mask("short"); got != "****" {
		t.Errorf("Mask(short)=%q want ****", got)
	}
	if got := Mask("ab"); got != "****" {
		t.Errorf("Mask(2-char)=%q want ****", got)
	}
}

func TestMask_Long(t *testing.T) {
	if got := Mask("abcdefghijklmnop"); got != "ab…op" {
		t.Errorf("Mask(long)=%q want ab…op", got)
	}
}

func TestAuthFilePath(t *testing.T) {
	got := AuthFilePath("zhipu-work", "apikey")
	if !strings.Contains(got, "zhipu-work_apikey.json") || !strings.Contains(got, ".model-proxy") {
		t.Errorf("authFilePath=%q want zhipu-work_apikey.json under .model-proxy", got)
	}
}

// --- color.go: root CLI color wrapper via the color-disabled path ---

func TestColorHelpers_NoColorPassthrough(t *testing.T) {
	// In tests stdout is not a tty (and NO_COLOR may be set), so color helpers
	// return the input verbatim. Assert the exact string rather than Contains,
	// which would also pass with ANSI codes around the input.
	for _, s := range []string{"x", "hello", "test-123"} {
		if got := provider.Yellow(s); got != s {
			t.Errorf("provider.Yellow(%q)=%q, want exact %q (no ANSI in test env)", s, got, s)
		}
	}
}
