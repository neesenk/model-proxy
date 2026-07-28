package main

import (
	"path/filepath"
	"testing"
)

// --- expandPath: ~, env:, plain, empty ---

func TestExpandPath(t *testing.T) {
	t.Setenv("MP_TEST_PATH", "/from/env")
	if got := expandPath("env:MP_TEST_PATH"); got != "/from/env" {
		t.Errorf("expandPath(env:)=%q want /from/env", got)
	}
	if got := expandPath(""); got != "" {
		t.Errorf("expandPath(empty)=%q want empty", got)
	}
	// Plain path with no prefix passes through.
	if got := expandPath("/abs/path"); got != "/abs/path" {
		t.Errorf("expandPath(/abs/path)=%q want /abs/path", got)
	}
	// ~/ expands to home.
	home := homeDir()
	want := filepath.Join(home, "foo")
	if got := expandPath("~/foo"); got != want {
		t.Errorf("expandPath(~/foo)=%q want %q", got, want)
	}
}
