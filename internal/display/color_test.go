package display

import (
	"os"
	"testing"
)

// The semantic helpers share C; with color enabled each must wrap in its ANSI
// code, and with color disabled each must pass through unchanged.
func TestSemanticHelpers_WrapWhenColorEnabled(t *testing.T) {
	old := ColorEnabled
	SetColorEnabled(true)
	t.Cleanup(func() { SetColorEnabled(old) })
	for _, tc := range []struct {
		name string
		fn   func(string) string
		code string
	}{
		{"Dim", Dim, ansiDim},
		{"Bold", Bold, ansiBold},
		{"Green", Green, ansiGreen},
		{"Yellow", Yellow, ansiYellow},
		{"Red", Red, ansiRed},
		{"Cyan", Cyan, ansiCyan},
		{"Blue", Blue, ansiBlue},
		{"Magenta", Magenta, ansiMagenta},
		{"Gray", Gray, ansiGray},
	} {
		if got := tc.fn("x"); got != tc.code+"x"+ansiReset {
			t.Errorf("%s(x) with color on=%q want %q", tc.name, got, tc.code+"x"+ansiReset)
		}
	}
}

func TestUsageRatioColor_Branches(t *testing.T) {
	old := ColorEnabled
	SetColorEnabled(true)
	t.Cleanup(func() { SetColorEnabled(old) })
	for _, tc := range []struct {
		name         string
		balance, tot float64
		want         string
	}{
		{"no total falls back to yellow", 5, 0, ansiYellow + "x" + ansiReset},
		{"negative total falls back to yellow", 5, -1, ansiYellow + "x" + ansiReset},
		{"half or more is green", 50, 100, ansiGreen + "x" + ansiReset},
		{"a fifth or more is yellow", 20, 100, ansiYellow + "x" + ansiReset},
		{"below a fifth is red", 19, 100, ansiRed + "x" + ansiReset},
	} {
		if got := UsageRatioColor(tc.balance, tc.tot, "x"); got != tc.want {
			t.Errorf("UsageRatioColor(%v,%v) [%s]=%q want %q", tc.balance, tc.tot, tc.name, got, tc.want)
		}
	}
}

// decideColor and DecideLogColor share the same NO_COLOR / CLICOLOR_FORCE /
// tty-detection policy; exercise every branch against a non-tty file.
func TestDecideColor_EnvOverrides(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "1")
	if decideColor(f) || DecideLogColor(f) {
		t.Error("NO_COLOR set must disable color even when CLICOLOR_FORCE is set")
	}

	t.Setenv("NO_COLOR", "")
	if !decideColor(f) || !DecideLogColor(f) {
		t.Error("CLICOLOR_FORCE=1 must force color on for a non-tty file")
	}

	t.Setenv("CLICOLOR_FORCE", "0")
	if decideColor(f) || DecideLogColor(f) {
		t.Error("CLICOLOR_FORCE=0 must not force color; non-tty file stays off")
	}

	t.Setenv("CLICOLOR_FORCE", "")
	if decideColor(f) || DecideLogColor(f) {
		t.Error("no env overrides: non-tty file must stay colorless")
	}
}

func TestIsTerminal_NonTTYAndStatError(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "notty")
	if err != nil {
		t.Fatal(err)
	}
	if isTerminal(f) || IsTerminalLog(f) {
		t.Error("regular file must not be detected as a terminal")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if isTerminal(f) || IsTerminalLog(f) {
		t.Error("stat error (closed file) must report not-a-terminal")
	}
}

// TestIsTerminalExcludesDevNull pins the char-device approximation's one
// documented exclusion: /dev/null is a char device but never interactive, so
// color must stay off when stdout is redirected there.
func TestIsTerminalExcludesDevNull(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Errorf("isTerminal(%s) = true, want false", os.DevNull)
	}
}
