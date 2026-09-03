package takeover_test

import (
	"os"
	"path/filepath"
	"testing"

	takeover "model-proxy/internal/takeover"
)

// fileMode returns the target's permission bits (fatal-friendly).
func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}

// TestTakeoverWritersPreserveFileMode: takeover rewrites client config files
// that may hold real API keys alongside the proxy entry (claude settings.json,
// codex config.toml, opencode.json). Overwriting a 0600 file with the
// hardcoded 0644 widened it to world-readable — the write must keep the
// existing mode, and files the takeover itself creates start at 0600.
func TestTakeoverWritersPreserveFileMode(t *testing.T) {
	t.Run("WriteJSONConfig preserves 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := takeover.WriteJSONConfig(path, map[string]any{"b": 2}); err != nil {
			t.Fatal(err)
		}
		if got := fileMode(t, path); got != 0o600 {
			t.Fatalf("mode after WriteJSONConfig = %o, want 0600 preserved", got)
		}
	})
	t.Run("WriteJSONConfig new file 0600", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := takeover.WriteJSONConfig(path, map[string]any{"b": 2}); err != nil {
			t.Fatal(err)
		}
		if got := fileMode(t, path); got != 0o600 {
			t.Fatalf("mode of new file = %o, want 0600", got)
		}
	})
	t.Run("codex template rewrite preserves 0600", func(t *testing.T) {
		dir := t.TempDir()
		file := filepath.Join(dir, "codex.toml")
		if err := os.WriteFile(file, []byte("model_provider = \"old\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := presetFor(t, "codex", file).Rewrite(baseCfg(), nil, nil); err != nil {
			t.Fatal(err)
		}
		if got := fileMode(t, file); got != 0o600 {
			t.Fatalf("mode after codex template rewrite = %o, want 0600 preserved", got)
		}
	})
	t.Run("Restore preserves 0600", func(t *testing.T) {
		dir := t.TempDir()
		bakDir := filepath.Join(dir, "bak")
		target := filepath.Join(dir, "settings.json")
		original := `{"keep":"me"}`
		if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := takeover.Backup(target, bakDir, "claude"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(`{"takeover":true}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := takeover.Restore(target, bakDir, "claude"); err != nil {
			t.Fatal(err)
		}
		if got := fileMode(t, target); got != 0o600 {
			t.Fatalf("mode after Restore = %o, want 0600 preserved", got)
		}
		b, _ := os.ReadFile(target)
		if string(b) != original {
			t.Fatalf("restored content = %q, want the backup bytes", string(b))
		}
	})
}
