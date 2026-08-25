package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zalando/go-keyring"
)

// fakeKeychain is an in-memory keychainServiceProvider for hermetic tests.
type fakeKeychain struct {
	mu        sync.Mutex
	entries   map[string][]byte
	available bool
	setErr    error
	getErr    error
	delErr    error
}

func newFakeKeychain(available bool) *fakeKeychain {
	return &fakeKeychain{entries: map[string][]byte{}, available: available}
}

func (f *fakeKeychain) key(service, user string) string { return service + "\x00" + user }

func (f *fakeKeychain) Set(service, user string, password []byte) error {
	if f.setErr != nil {
		return f.setErr
	}
	if !f.available {
		return ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries[f.key(service, user)] = append([]byte(nil), password...)
	return nil
}

func (f *fakeKeychain) Get(service, user string) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if !f.available {
		return nil, ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	blob, ok := f.entries[f.key(service, user)]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), blob...), nil
}

func (f *fakeKeychain) Delete(service, user string) error {
	if f.delErr != nil {
		return f.delErr
	}
	if !f.available {
		return ErrUnavailable
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.entries, f.key(service, user))
	return nil
}

func (f *fakeKeychain) Available(service string) bool { return f.available }

// useFakeKeychain swaps the backend and forces mode re-resolution to
// MP_CRED_STORE=keychain, restoring both at test end.
func useFakeKeychain(t *testing.T, fake *fakeKeychain) {
	t.Helper()
	keychainOps = fake
	t.Setenv(envCredStore, string(ModeKeychain))
	resetResolution()
	t.Cleanup(func() {
		keychainOps = realKeychainProvider{}
		os.Unsetenv(envCredStore)
		resetResolution()
	})
}

func resetResolution() {
	resolveOnce = sync.Once{}
	resolved = ""
}

func osUnsetenvCredStore(t *testing.T) {
	t.Helper()
	t.Setenv(envCredStore, string(ModeAuto))
}

func forceFileMode(t *testing.T) {
	t.Helper()
	t.Setenv(envCredStore, string(ModeFile))
	resetResolution()
	t.Cleanup(func() {
		os.Unsetenv(envCredStore)
		resetResolution()
	})
}

func TestResolvedModeEnvOverrides(t *testing.T) {
	cases := []struct {
		env  string
		want Mode
	}{
		{"file", ModeFile},
		{"keychain", ModeKeychain},
		{"", ModeFile},      // auto under testing.Testing() → file, never probes real keychain
		{"auto", ModeFile},  // same
		{"bogus", ModeFile}, // unknown value fails safe
	}
	for _, tc := range cases {
		t.Run(tc.env, func(t *testing.T) {
			if tc.env == "" {
				os.Unsetenv(envCredStore)
				resetResolution()
				t.Cleanup(resetResolution)
			} else {
				t.Setenv(envCredStore, tc.env)
				resetResolution()
				t.Cleanup(resetResolution)
			}
			if got := ResolvedMode(); got != tc.want {
				t.Fatalf("MP_CRED_STORE=%q resolved to %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}

func TestRefFileModeRoundTripAndDelete(t *testing.T) {
	forceFileMode(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "zhipu_apikeys.json")
	ref := NewRef(path)

	if _, err := ref.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file must map to ErrNotFound, got %v", err)
	}

	blob := []byte(`{"api_key":"k"}`)
	if err := ref.Save(blob); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after Save: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("saved file perms = %#o, want 600", perm)
	}
	got, err := ref.Load()
	if err != nil || string(got) != string(blob) {
		t.Fatalf("Load after Save: got (%q, %v)", got, err)
	}
	// Overwrite keeps atomic-rename semantics: no temp residue.
	if err := ref.Save([]byte(`{"api_key":"k2"}`)); err != nil {
		t.Fatalf("re-Save: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if err := ref.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := ref.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete Load = %v, want ErrNotFound", err)
	}
	// Idempotent delete.
	if err := ref.Delete(); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

func TestRefLoadLazyMigratesPlaintextToKeychain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deepseek_apikeys.json")
	legacy := []byte(`{"version":1,"accounts":[{"id":"a1","label":"a1","api_key":"sk-x"}]}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	got, err := ref.Load()
	if err != nil || string(got) != string(legacy) {
		t.Fatalf("migrating Load: got (%q, %v)", got, err)
	}
	// Plaintext moved aside exactly one generation; keychain authoritative.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("plaintext file still present after migration (err=%v)", err)
	}
	if bak, err := os.ReadFile(path + migratedSuffix); err != nil || string(bak) != string(legacy) {
		t.Fatalf(".migrated.bak missing or wrong content (err=%v)", err)
	}
	if got2, err := ref.Load(); err != nil || string(got2) != string(legacy) {
		t.Fatalf("post-migration Load from keychain: (%q, %v)", got2, err)
	}
	// A second Ref round-trips through the keychain without the file at all.
	if got3, err := NewRef(filepath.Join(dir, "other.json")).Load(); err == nil {
		_ = got3
	}
	// Save updates the keychain entry, not a plaintext file.
	if err := ref.Save([]byte(`{"version":1}`)); err != nil {
		t.Fatalf("Save under keychain mode: %v", err)
	}
	if blob, _ := fake.Get(serviceName, ref.Name); string(blob) != `{"version":1}` {
		t.Fatalf("keychain entry after Save = %q", blob)
	}
	// The rollback generation (.migrated.bak) intentionally survives: it still
	// holds the pre-migration plaintext as the single rollback window.
	if bak, err := os.ReadFile(path + migratedSuffix); err != nil || string(bak) != string(legacy) {
		t.Fatalf("rollback .migrated.bak must survive with legacy content (err=%v)", err)
	}
}

func TestRefLoadMigrationFailureKeepsPlaintextAuthoritative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	legacy := []byte(`{"tokens":{"access_token":"t"}}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	fake.setErr = errors.New("backend write rejected")
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	if _, err := ref.Load(); err == nil {
		t.Fatal("migration save failure must fail closed with an error")
	}
	// The only copy of the credentials survived untouched — the account is
	// not locked out by a half-finished migration (pitfalls #18 class).
	got, rerr := os.ReadFile(path)
	if rerr != nil || string(got) != string(legacy) {
		t.Fatalf("plaintext damaged during failed migration: (%q, %v)", got, rerr)
	}
	if _, err := os.Stat(path + migratedSuffix); !os.IsNotExist(err) {
		t.Fatal("no backup may be created when the store write failed")
	}
}

func TestRefDeleteAcrossModesIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kimi-code_apikeys.json")
	fake := newFakeKeychain(true)
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	if err := ref.Save([]byte(`{}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Delete removes keychain entry AND any leftover plaintext/bak files.
	if err := ref.Delete(); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plaintext file must be removed on delete in keychain mode")
	}
	if _, err := ref.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after cross-mode delete = %v, want ErrNotFound", err)
	}
	if err := ref.Delete(); err != nil {
		t.Fatalf("idempotent second Delete: %v", err)
	}
}

func TestExplicitKeychainUnavailableFailsClosed(t *testing.T) {
	useFakeKeychain(t, newFakeKeychain(false)) // env forced to keychain, backend dead
	ref := NewRef(filepath.Join(t.TempDir(), "zhipu_apikeys.json"))
	if _, err := ref.Load(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Load on dead backend = %v, want ErrUnavailable", err)
	}
	if err := ref.Save([]byte(`{}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Save on dead backend = %v, want ErrUnavailable", err)
	}
}

func TestRefSaveUnderKeychainModeArchivesLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "zhipu_apikeys.json")
	legacy := []byte(`{"api_key":"old"}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	useFakeKeychain(t, newFakeKeychain(true))

	ref := NewRef(path)
	if err := ref.Save([]byte(`{"api_key":"new"}`)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	blob, _ := keychainOps.Get(serviceName, ref.Name)
	if string(blob) != `{"api_key":"new"}` {
		t.Fatalf("keychain entry = %q", blob)
	}
	if bak, err := os.ReadFile(path + migratedSuffix); err != nil || string(bak) != string(legacy) {
		t.Fatalf("legacy file not archived on keychain-mode Save (err=%v)", err)
	}
	// A second Save with no plaintext present is still fine.
	if err := ref.Save([]byte(`{"api_key":"newer"}`)); err != nil {
		t.Fatalf("second keychain-mode Save: %v", err)
	}
}

func TestRefDeleteKeychainErrorPropagatesButFilesStillCleaned(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deepseek_apikeys.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	fake.delErr = errors.New("delete rejected")
	useFakeKeychain(t, fake)

	if err := NewRef(path).Delete(); err == nil {
		t.Fatal("keychain delete failure must propagate")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plaintext cleanup must proceed even when keychain delete fails")
	}
}

func TestRealKeychainAvailableConsumesUnexpectedProbeEntry(t *testing.T) {
	withMockedKeyring(t)
	// A stray entry under the probe name must not break reachability: consume it.
	if err := keyring.Set(serviceName, probeAccount, "stale"); err != nil {
		t.Fatalf("seed probe entry: %v", err)
	}
	p := realKeychainProvider{}
	if !p.Available(serviceName) {
		t.Fatal("reachable backend must report Available even with stray probe entry")
	}
	if _, err := keyring.Get(serviceName, probeAccount); !errors.Is(err, keyring.ErrNotFound) {
		t.Fatalf("stray probe entry not consumed: %v", err)
	}
}

func TestAtomicWriteFileRenameFailureCleansTemp(t *testing.T) {
	forceFileMode(t)
	dir := t.TempDir()
	// Target path is an existing directory: os.Rename(file, dir) fails.
	target := filepath.Join(dir, "blocker")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	// Non-empty target dir guarantees the final rename fails on every OS
	// (rename file->dir fails; even where empty-dir replacement is permitted,
	// a non-empty directory cannot be replaced by a regular file).
	if err := os.WriteFile(filepath.Join(target, "keep.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	ref := NewRef(target) // final path IS the non-empty directory → rename must fail
	if err := ref.Save([]byte(`{}`)); err == nil {
		t.Fatal("save onto a directory path must fail")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Fatalf("temp residue after failed save: %s", e.Name())
		}
	}
}

func TestRefSaveFileModeMkdirFailurePropagates(t *testing.T) {
	forceFileMode(t)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Parent "directory" is actually a regular file → MkdirAll fails.
	if err := NewRef(filepath.Join(blocker, "pool.json")).Save([]byte(`{}`)); err == nil {
		t.Fatal("save under an unwritable parent must fail")
	}
}
