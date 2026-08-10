package takeover_test

import (
	"strings"

	takeover "model-proxy/internal/takeover"
	"os"
	"path/filepath"
	"testing"
)

// takeover_extra_test.go covers the backup/restore/listClients/sha256hex
// helpers in takeover.go and the small util.go helpers — all pure file/string
// ops, fully testable with temp dirs.

// --- backup: copies file + writes meta, idempotent ---

func TestBackup_CreatesCopyAndMeta(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "settings.json")
	os.WriteFile(src, []byte(`{"x":1}`), 0o644)
	bakDir := filepath.Join(dir, ".mp")

	if err := takeover.Backup(src, bakDir, "claude"); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(bakDir, "claude.bak")
	data, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup file not created: %v", err)
	}
	if string(data) != `{"x":1}` {
		t.Errorf("backup content=%q want {\"x\":1}", data)
	}
	meta, err := os.ReadFile(bak + ".meta")
	if err != nil {
		t.Fatalf("meta file not created: %v", err)
	}
	if !strings.Contains(string(meta), "sha256") || !strings.Contains(string(meta), "path") {
		t.Errorf("meta missing fields: %s", meta)
	}
}

func TestBackup_Idempotent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	os.WriteFile(src, []byte("original"), 0o644)
	bakDir := filepath.Join(dir, ".mp")
	if err := takeover.Backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	// Change source; second backup must NOT overwrite the existing .bak.
	os.WriteFile(src, []byte("changed"), 0o644)
	if err := takeover.Backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(bakDir, "c.bak"))
	if string(data) != "original" {
		t.Errorf("idempotent backup overwrote: got %q want original", data)
	}
}

func TestBackup_MissingSource(t *testing.T) {
	dir := t.TempDir()
	err := takeover.Backup(filepath.Join(dir, "nope"), filepath.Join(dir, ".mp"), "c")
	if err == nil {
		t.Error("backup of missing file: want error, got nil")
	}
}

// --- restore: copies .bak back ---

func TestRestore_WritesBack(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	os.WriteFile(src, []byte("new"), 0o644)
	bakDir := filepath.Join(dir, ".mp")
	os.MkdirAll(bakDir, 0o700)
	os.WriteFile(filepath.Join(bakDir, "c.bak"), []byte("original"), 0o600)

	if err := takeover.Restore(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(src)
	if string(data) != "original" {
		t.Errorf("restore content=%q want original", data)
	}
}

func TestRestore_NoBackup(t *testing.T) {
	dir := t.TempDir()
	err := takeover.Restore(filepath.Join(dir, "out"), filepath.Join(dir, ".mp"), "c")
	if err == nil {
		t.Error("restore with no backup: want error, got nil")
	}
}

// --- sha256hex ---

func TestSha256hex(t *testing.T) {
	// Known: sha256("hello") = 2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824
	got := takeover.Sha256hex([]byte("hello"))
	want := "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got != want {
		t.Errorf("takeover.Sha256hex(hello)=%q want %q", got, want)
	}
}

// --- backupDir ---

func TestBackupDir(t *testing.T) {
	got := takeover.BackupDir("/home/user/.config/foo/config.yaml")
	want := "/home/user/.config/foo/.model-proxy"
	if got != want {
		t.Errorf("backupDir=%q want %q", got, want)
	}
}
