package display

import (
	"strings"
	"testing"
	"time"
)

func TestProgressBar(t *testing.T) {
	bar := ProgressBar(50, 2)
	if !strings.HasPrefix(bar, "[") || !strings.HasSuffix(bar, "]") {
		t.Errorf("ProgressBar(width=2)=%q missing brackets", bar)
	}
}

func TestPad(t *testing.T) {
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad(%q,5)=%q want %q", "ab", got, "ab   ")
	}
	if got := Pad("abcde", 3); got != "abcde" {
		t.Errorf("Pad(%q,3)=%q want %q (no truncation)", "abcde", got, "abcde")
	}
	if got := Pad("", 3); got != "   " {
		t.Errorf("Pad(%q,3)=%q want %q", "", got, "   ")
	}
}

// Moved from internal/cli/models/models_cli_helpers_test.go: Pad is owned here.
func TestPad_AlreadyLong(t *testing.T) {
	if got := Pad("toolongalready", 5); got != "toolongalready" {
		t.Errorf("Pad(long,5)=%q want passthrough", got)
	}
}

func TestFormatResetAt(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 14, 30, 0, 0, now.Location())
	got := FormatResetAt(today.UnixMilli())
	if !strings.Contains(got, "14:30") || strings.Contains(got, "-") {
		t.Errorf("FormatResetAt(today 14:30)=%q want 14:30 (no date)", got)
	}
	other := time.Date(now.Year(), now.Month(), now.Day()+5, 9, 5, 0, 0, now.Location())
	got = FormatResetAt(other.UnixMilli())
	if !strings.Contains(got, "09:05") {
		t.Errorf("FormatResetAt(other day)=%q want MM-DD 09:05", got)
	}
}
