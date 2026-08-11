package config

import (
	"path/filepath"
	"testing"
)

// --- expandPath: ~, env:, plain, empty ---

func TestExpandPath(t *testing.T) {
	t.Setenv("MP_TEST_PATH", "/from/env")
	if got := ExpandPath("env:MP_TEST_PATH"); got != "/from/env" {
		t.Errorf("ExpandPath(env:)=%q want /from/env", got)
	}
	if got := ExpandPath(""); got != "" {
		t.Errorf("ExpandPath(empty)=%q want empty", got)
	}
	// Plain path with no prefix passes through.
	if got := ExpandPath("/abs/path"); got != "/abs/path" {
		t.Errorf("ExpandPath(/abs/path)=%q want /abs/path", got)
	}
	// ~/ expands to home.
	home := homeDir()
	want := filepath.Join(home, "foo")
	if got := ExpandPath("~/foo"); got != want {
		t.Errorf("ExpandPath(~/foo)=%q want %q", got, want)
	}
}
