package framework

import (
	"os"
	"path/filepath"
	"testing"
)

// TestConfigPath_LookupOrder verifies the lookup order:
// --config flag > ~/.model-proxy/config.yaml > ./config.yaml.
func TestConfigPath_LookupOrder(t *testing.T) {
	// 1. explicit flag wins over everything.
	got := ConfigPath([]string{"--config", "/explicit.yaml"})
	if got != "/explicit.yaml" {
		t.Errorf("flag: got %q", got)
	}
	got = ConfigPath([]string{"--config=/explicit2.yaml"})
	if got != "/explicit2.yaml" {
		t.Errorf("flag=: got %q", got)
	}

	// 2. user-level file wins over ./config.yaml. Point HOME at a temp dir with
	// the user config present (under .model-proxy/), and a different CWD config —
	// the user one wins.
	dir := t.TempDir()
	aisDir := filepath.Join(dir, ".model-proxy")
	if err := os.MkdirAll(aisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeCfg := filepath.Join(aisDir, "config.yaml")
	if err := os.WriteFile(homeCfg, []byte("listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	// Without a flag and with a user-level file present, configPath returns it.
	got = ConfigPath(nil)
	if got != homeCfg {
		t.Errorf("user-level: got %q want %q", got, homeCfg)
	}

	// 3. CWD fallback: no flag and no user-level file → ./config.yaml relative
	// path. Without this branch pinned, deleting it would go unnoticed.
	emptyHome := t.TempDir()
	t.Setenv("HOME", emptyHome)
	if got := ConfigPath(nil); got != "config.yaml" {
		t.Errorf("CWD fallback: got %q, want relative config.yaml", got)
	}
}

// TestFlagStringValue pins both flag forms and the absent/trailing-flag
// fallbacks ("" so callers can distinguish "flag missing" cleanly).
func TestFlagStringValue(t *testing.T) {
	args := []string{"cmd", "--name", "value", "--other=x"}
	if got := FlagStringValue(args, "--name"); got != "value" {
		t.Errorf("space form: got %q, want value", got)
	}
	if got := FlagStringValue(args, "--other"); got != "x" {
		t.Errorf("= form: got %q, want x", got)
	}
	if got := FlagStringValue(args, "--missing"); got != "" {
		t.Errorf("absent: got %q, want empty", got)
	}
	if got := FlagStringValue([]string{"--name"}, "--name"); got != "" {
		t.Errorf("trailing flag without value: got %q, want empty", got)
	}
}

// TestHasFlagValue pins exact and =-form matching, including that a longer
// flag sharing the prefix does NOT match.
func TestHasFlagValue(t *testing.T) {
	args := []string{"cmd", "--force", "--ttl=60"}
	if !HasFlagValue(args, "--force") {
		t.Error("exact flag: want true")
	}
	if !HasFlagValue(args, "--ttl") {
		t.Error("= form: want true")
	}
	if HasFlagValue(args, "--tt") {
		t.Error("prefix of a longer flag must not match")
	}
	if HasFlagValue(args, "--missing") {
		t.Error("absent: want false")
	}
}

func TestPlural(t *testing.T) {
	if got := Plural(1, "account", "accounts"); got != "account" {
		t.Errorf("n=1: got %q, want singular", got)
	}
	for _, n := range []int{0, 2, -1} {
		if got := Plural(n, "account", "accounts"); got != "accounts" {
			t.Errorf("n=%d: got %q, want plural", n, got)
		}
	}
}

// TestHomeDir pins that HOME is read per call (tests isolate it via Setenv).
func TestHomeDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if got := HomeDir(); got != dir {
		t.Errorf("HomeDir()=%q, want %q", got, dir)
	}
}

// TestPositionalArgs: --flag value pairs are skipped; bare positionals are kept
// in order (used by pin/unpin/replay to pull <route> [<provider>] / <id>).
func TestPositionalArgs(t *testing.T) {
	got := PositionalArgs([]string{"glm", "--config", "x.yaml", "zhipu", "--ttl", "1h"})
	if len(got) != 2 || got[0] != "glm" || got[1] != "zhipu" {
		t.Errorf("positionalArgs=%v want [glm zhipu]", got)
	}
	// --flag=value form doesn't consume a following bare token.
	got = PositionalArgs([]string{"--config=x.yaml", "glm"})
	if len(got) != 1 || got[0] != "glm" {
		t.Errorf("positionalArgs=%v want [glm]", got)
	}
}
