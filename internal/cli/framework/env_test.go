package framework

import (
	"model-proxy/internal/provider"
	"strings"
	"testing"
)

// --- util.go ---

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
