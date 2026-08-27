package presets

import (
	"io"
	domainpresets "model-proxy/internal/presets"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cli-side helper tests (config-level behavior lives in internal/presets).

const minimalConfig = `listen: 127.0.0.1:15721
log_level: info
`

func writeMinimalConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(minimalConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- interactive helpers (pure functions over injected streams) ---

func TestAskYesNo(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false}, // empty line → no
		{"garbage\n", false},
	}
	for _, tc := range cases {
		var out strings.Builder
		if got := askYesNo(strings.NewReader(tc.in), &out, "prompt: "); got != tc.want {
			t.Errorf("askYesNo(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// EOF without a newline defaults to no (fail-closed).
	var out strings.Builder
	if askYesNo(strings.NewReader(""), &out, "prompt: ") {
		t.Error("EOF must default to no")
	}
}

func TestBufioReadLine(t *testing.T) {
	r := strings.NewReader("first\nsecond\n")
	got, err := bufioReadLine(r)
	if err != nil || got != "first" {
		t.Fatalf("bufioReadLine = (%q, %v), want first", got, err)
	}
	got2, _ := bufioReadLine(r)
	if got2 != "second" {
		t.Fatalf("second read = %q, want second (must not over-buffer)", got2)
	}
	got3, err := bufioReadLine(strings.NewReader("no-newline"))
	if err == nil || got3 != "no-newline" {
		t.Fatalf("EOF without newline = (%q, %v)", got3, err)
	}
}

func TestPickPresetInteractively(t *testing.T) {
	catalog := []domainpresets.Preset{{Name: "zhipu"}, {Name: "deepseek"}}
	if name, err := pickPresetInteractively(strings.NewReader("1\n"), io.Discard, catalog); err != nil || name != "zhipu" {
		t.Fatalf("pick(1) = (%q, %v)", name, err)
	}
	if name, err := pickPresetInteractively(strings.NewReader("2\n"), io.Discard, catalog); err != nil || name != "deepseek" {
		t.Fatalf("pick(2) = (%q, %v)", name, err)
	}
	for _, in := range []string{"0\n", "99\n", "abc\n"} {
		if _, err := pickPresetInteractively(strings.NewReader(in), io.Discard, catalog); err == nil {
			t.Errorf("pick(%q) must fail", in)
		}
	}
	if _, err := pickPresetInteractively(strings.NewReader(""), io.Discard, catalog); err == nil {
		t.Error("EOF selection must fail")
	}
}

func TestFirstPositional(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"zhipu"}, "zhipu"},
		{[]string{"--config", "/tmp/c.yaml", "zhipu"}, "zhipu"},
		{[]string{"--config=/tmp/c.yaml", "zhipu"}, "zhipu"},
		{[]string{"--label", "work", "zhipu"}, "zhipu"},
		{[]string{"--api-key-env=K", "zhipu"}, "zhipu"},
		{[]string{}, ""},
		{[]string{"--replace"}, ""},
	}
	for _, tc := range cases {
		if got := firstPositional(tc.args); got != tc.want {
			t.Errorf("firstPositional(%v) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

func TestStdinIsInteractiveAndCfgOrHint(t *testing.T) {
	if stdinIsInteractive(strings.NewReader("")) {
		t.Error("strings.Reader is not an interactive terminal")
	}
	if got := cfgOrHint(""); got != "./config.yaml" {
		t.Errorf("cfgOrHint(\"\") = %q", got)
	}
	if got := cfgOrHint("/x/y.yaml"); got != "/x/y.yaml" {
		t.Errorf("cfgOrHint passthrough broken: %q", got)
	}
}
