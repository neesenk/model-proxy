package takeover_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	takeover "model-proxy/internal/takeover"
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

// Regression: Backup treated ANY os.Stat error on the .bak as "no backup
// exists" and entered the create/overwrite path — a permission/IO error
// would overwrite an existing backup with the post-takeover file, losing
// the user's original config. Only os.IsNotExist may create; other Stat
// errors must fail closed.
func TestBackup_StatErrorFailsClosed(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	if err := os.WriteFile(src, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A NUL byte in the backup path makes os.Stat fail with EINVAL — an
	// error that is NOT os.IsNotExist — deterministically, on every platform.
	err := takeover.Backup(src, filepath.Join(dir, ".mp"), "bad\x00name")
	if err == nil {
		t.Fatal("Backup with an un-stat-able backup path: want error, got nil")
	}
	if err == takeover.ErrNoFile {
		t.Fatalf("Backup returned ErrNoFile for a stat failure on the backup: %v", err)
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

// --- restore integrity: the .meta sha256 Backup records must gate restores ---

// Regression: Backup wrote a sha256 into <bak>.meta but Restore never checked
// it — a corrupted or tampered .bak was copied verbatim over the user's config.
// A present-but-mismatching sha256 must refuse the restore.
func TestRestore_ShaMismatchRefuses(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	if err := os.WriteFile(src, []byte("current"), 0o644); err != nil {
		t.Fatal(err)
	}
	bakDir := filepath.Join(dir, ".mp")
	if err := takeover.Backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}

	// Tamper with the backup content after the meta was written.
	if err := os.WriteFile(filepath.Join(bakDir, "c.bak"), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := takeover.Restore(src, bakDir, "c")
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("Restore err = %v, want sha256 integrity failure", err)
	}
	// Fail-closed: the target file must not have been overwritten.
	got, readErr := os.ReadFile(src)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != "current" {
		t.Fatalf("restore overwrote target despite integrity failure: %q", got)
	}
	// Fail-closed also means the takeover marker survives: the client is
	// still taken over (its config still points at the proxy), so the .bak
	// must remain for a future retry.
	if _, statErr := os.Stat(filepath.Join(bakDir, "c.bak")); statErr != nil {
		t.Fatalf("refused restore removed the backup marker: %v", statErr)
	}
}

// Regression (M3): restore never cleared the takeover marker, so a
// deliberately restored client kept its .bak and CheckTakeoverDrift reported
// it as taken over (and drifted) forever. A successful restore ends the
// takeover: content comes back AND the .bak/.meta markers are removed.
func TestRestore_RemovesMarker(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	if err := os.WriteFile(src, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	bakDir := filepath.Join(dir, ".mp")
	if err := takeover.Backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("taken-over"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := takeover.Restore(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "original" {
		t.Fatalf("restored content = %q, want original", got)
	}
	for _, marker := range []string{"c.bak", "c.bak.meta"} {
		if _, err := os.Stat(filepath.Join(bakDir, marker)); !os.IsNotExist(err) {
			t.Errorf("restore left takeover marker %s behind (stat err=%v)", marker, err)
		}
	}
	// A second restore now reports "no backup" — the client is not taken over.
	if err := takeover.Restore(src, bakDir, "c"); err != takeover.ErrNoFile {
		t.Errorf("second Restore err = %v, want ErrNoFile", err)
	}
}

// A meta recorded by Backup (matching content) must restore successfully.
func TestRestore_MatchingMetaSucceeds(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "s.json")
	original := []byte("original-content")
	if err := os.WriteFile(src, original, 0o644); err != nil {
		t.Fatal(err)
	}
	bakDir := filepath.Join(dir, ".mp")
	if err := takeover.Backup(src, bakDir, "c"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("rewritten"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := takeover.Restore(src, bakDir, "c"); err != nil {
		t.Fatalf("Restore with matching meta: %v", err)
	}
	got, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("restored content = %q, want %q", got, original)
	}
}

// A missing or corrupt meta is a WARNING, not a blocker: backups taken before
// meta existed must stay restorable.
func TestRestore_MissingOrCorruptMetaWarnsAndRestores(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metaBody string
	}{
		{"no meta", ""},
		{"corrupt meta", "not-json"},
		{"meta without sha256", `{"backed_up_at":"2026-01-01T00:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "out.json")
			bakDir := filepath.Join(dir, ".mp")
			if err := os.MkdirAll(bakDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(bakDir, "c.bak"), []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			if tc.metaBody != "" {
				if err := os.WriteFile(filepath.Join(bakDir, "c.bak.meta"), []byte(tc.metaBody), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := takeover.Restore(src, bakDir, "c"); err != nil {
				t.Fatalf("Restore with %s must degrade to a warning, got %v", tc.name, err)
			}
			got, err := os.ReadFile(src)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != "original" {
				t.Fatalf("restored content = %q, want original", got)
			}
		})
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

// Regression (atomic backup/restore): Backup, Restore and WriteJSONConfig used
// to write the destination directly, so a crash mid-write left a truncated
// file — a truncated .bak would be preserved forever by the "existing backup
// is kept" idempotency rule, losing the user's original client config. All
// three paths now write via a unique temp file + rename: the destination is
// always the complete old or complete new content, concurrent writers can't
// interleave, and no temp leftovers remain.
func TestBackupRestore_NoTempLeftoversAndByteFidelity(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "settings.json")
	original := []byte(`{"a":1,"b":"x"}` + strings.Repeat("//pad", 512))
	if err := os.WriteFile(src, original, 0o600); err != nil {
		t.Fatal(err)
	}
	bakDir := filepath.Join(dir, "bak")

	if err := takeover.Backup(src, bakDir, "claude"); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(bakDir, "claude.bak"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("backup not byte-identical: %d vs %d bytes", len(got), len(original))
	}

	// Corrupt the original, restore, and require the exact original bytes back.
	if err := os.WriteFile(src, []byte(`{"c":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := takeover.Restore(src, bakDir, "claude"); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("restore did not return the exact backup bytes")
	}
	for _, path := range []string{dir, bakDir} {
		entries, _ := os.ReadDir(path)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp") {
				t.Errorf("temp leftover: %s/%s", path, e.Name())
			}
		}
	}
}

// Concurrent writers to the same client config must never interleave: the
// final file is valid JSON identical to exactly one writer's payload.
func TestWriteJSONConfig_ConcurrentWritersNeverInterleave(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.json")
	if err := takeover.WriteJSONConfig(file, map[string]any{"init": true}); err != nil {
		t.Fatal(err)
	}
	const writers, rounds = 8, 25
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		payload := map[string]any{
			"writer": w,
			"pad":    strings.Repeat("x", 4096),
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if err := takeover.WriteJSONConfig(file, payload); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("final file is not valid JSON (interleaved writers): %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Errorf("temp leftover: %s", e.Name())
		}
	}
}
