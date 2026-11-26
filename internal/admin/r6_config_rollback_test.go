package admin

// r6_config_rollback_test.go — a hot reload that fails after the new config
// reached disk triggers a restore from the pre-write backup. When that
// restore fails too, the returned error must carry both facts: the caller is
// then the only one who can act on a config file the running generation never
// loaded (R6 B18).

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// r6RollbackNote names the consequence in the returned error.
const r6RollbackNote = "the config on disk was not restored and differs from the running generation"

// TestSaveConfigRollbackFailureSurfacesBothErrors: the reload error stays the
// wrapped cause, and the failed restore rides the same error with its own
// cause — whether the backup could not be read or the restore write failed.
func TestSaveConfigRollbackFailureSurfacesBothErrors(t *testing.T) {
	reloadErr := errors.New("build failed")
	next := []byte("listen: 127.0.0.1:9090\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test}\n")
	cases := []struct {
		name        string
		sabotage    func(t *testing.T, configFile string)
		assertCause func(t *testing.T, err error)
	}{
		{
			name: "backup unreadable",
			sabotage: func(t *testing.T, configFile string) {
				// The backup vanished between the write and the reload. Every
				// backup of this file goes, so a second boundary crossed
				// between BackupPath and the reload cannot leave one behind.
				backups, err := filepath.Glob(filepath.Join(filepath.Dir(configFile), ".model-proxy", "back", "*.bak"))
				if err != nil {
					t.Fatal(err)
				}
				if len(backups) == 0 {
					t.Fatal("no backup was written before the reload")
				}
				for _, backup := range backups {
					if err := os.Remove(backup); err != nil {
						t.Fatal(err)
					}
				}
			},
			assertCause: func(t *testing.T, err error) {
				if !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("error does not unwrap to fs.ErrNotExist (the unreadable backup): %v", err)
				}
			},
		},
		{
			name: "restore write fails",
			sabotage: func(t *testing.T, configFile string) {
				// Root-independent stand-in for a read-only or full disk: a
				// directory squatting on the config path makes the restore's
				// final rename fail.
				if err := os.Remove(configFile); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(configFile, 0o700); err != nil {
					t.Fatal(err)
				}
			},
			assertCause: func(t *testing.T, err error) {
				var renameErr *os.LinkError
				if !errors.As(err, &renameErr) {
					t.Errorf("error does not carry the restore's rename failure: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTestConfig(t)
			home := t.TempDir()
			service := New(Ports{
				ConfigFile: func() string { return path },
				HomeDir:    func() string { return home },
				Reload: func(configFile string) error {
					tc.sabotage(t, configFile)
					return reloadErr
				},
			})

			err := service.SaveConfig(next)
			if !errors.Is(err, reloadErr) {
				t.Fatalf("SaveConfig err = %v, want the reload error kept as the cause", err)
			}
			if got := err.Error(); !strings.HasPrefix(got, "build failed (config rollback failed: ") || !strings.Contains(got, r6RollbackNote) {
				t.Errorf("SaveConfig err = %q, want the reload error plus the failed rollback and %q", got, r6RollbackNote)
			}
			tc.assertCause(t, err)

			// The divergence the message reports is real: nothing put the
			// previous bytes back (the write-failure case left a directory).
			if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
				if after, _ := os.ReadFile(path); string(after) != string(next) {
					t.Errorf("config on disk = %q, want the rejected version the error describes", after)
				}
			}
		})
	}
}

// TestSaveConfigRollbackSuccessKeepsBareReloadError: when the restore works,
// the caller sees exactly the reload error — no rollback annotation.
func TestSaveConfigRollbackSuccessKeepsBareReloadError(t *testing.T) {
	path := writeTestConfig(t)
	before, _ := os.ReadFile(path)
	spy := &reloadSpy{err: errors.New("build failed")}
	if err := editService(t, path, spy).SaveConfig([]byte("listen: 127.0.0.1:9090\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: https://example.test}\n")); err == nil || err.Error() != "build failed" {
		t.Fatalf("SaveConfig err = %v, want the bare reload error", err)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("successful rollback must restore the previous config file")
	}
}
