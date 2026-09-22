package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	setCalls  int
	delCalls  int
}

func newFakeKeychain(available bool) *fakeKeychain {
	return &fakeKeychain{entries: map[string][]byte{}, available: available}
}

func (f *fakeKeychain) key(service, user string) string { return service + "\x00" + user }

func (f *fakeKeychain) Set(service, user string, password []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setCalls++
	if f.setErr != nil {
		return f.setErr
	}
	if !f.available {
		return ErrUnavailable
	}
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
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delCalls++
	if f.delErr != nil {
		return f.delErr
	}
	if !f.available {
		return ErrUnavailable
	}
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
	modeMu.Lock()
	defer modeMu.Unlock()
	resolvedOK = false
	resolved = ""
	resolvedSource = ""
	configMode = ""
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

// scriptedProbe scripts the auto-mode reachability probe for the
// EffectiveMode concurrency tests: every Available call is announced on
// entered and then waits for release to close (later calls pass straight
// through a closed release); failAfter == 0 stays reachable forever,
// otherwise only the first failAfter calls report reachable. The embedded
// fakeKeychain satisfies the rest of the seam — nothing on it is expected to
// run in these tests.
type scriptedProbe struct {
	*fakeKeychain
	entered   chan struct{}
	release   chan struct{}
	failAfter int

	mu    sync.Mutex
	calls int
}

func newScriptedProbe(failAfter int) *scriptedProbe {
	return &scriptedProbe{
		fakeKeychain: newFakeKeychain(true),
		entered:      make(chan struct{}, 32),
		release:      make(chan struct{}),
		failAfter:    failAfter,
	}
}

func (s *scriptedProbe) Available(_ string) bool {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	s.entered <- struct{}{}
	<-s.release
	return s.failAfter == 0 || n <= s.failAfter
}

func (s *scriptedProbe) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
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
	// A second Ref with NO plaintext file at all: an empty Load is a clean
	// ErrNotFound (fail-closed), and after Save the round-trip goes purely
	// through the keychain — still without any plaintext file.
	other := NewRef(filepath.Join(dir, "other.json"))
	if _, err := other.Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("empty Ref Load = %v, want ErrNotFound", err)
	}
	if err := other.Save([]byte(`{"second":true}`)); err != nil {
		t.Fatalf("Save under keychain mode (file-free ref): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "other.json")); !os.IsNotExist(err) {
		t.Fatalf("keychain-mode Save must not create a plaintext file (err=%v)", err)
	}
	if got3, err := other.Load(); err != nil || string(got3) != `{"second":true}` {
		t.Fatalf("file-free keychain round-trip: (%q, %v)", got3, err)
	}
	marker, err := os.ReadFile(other.Path + keychainOriginSuffix)
	if err != nil || string(marker) != keychainOriginContent {
		t.Fatalf("keychain-only Save origin marker = (%q, %v)", marker, err)
	}
	if info, err := os.Stat(other.Path + keychainOriginSuffix); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("keychain-only Save origin marker mode = (%v, %v), want 0600", info, err)
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

func TestRefLoadBackfillsOriginForLegacyKeychainOnlyEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	fake := newFakeKeychain(true)
	if err := fake.Set(serviceName, filepath.Base(path), []byte(`{"tokens":{"access_token":"legacy"}}`)); err != nil {
		t.Fatal(err)
	}
	useFakeKeychain(t, fake)

	if _, err := NewRef(path).Load(); err != nil {
		t.Fatalf("Load legacy keychain-only entry: %v", err)
	}
	marker, err := os.ReadFile(path + keychainOriginSuffix)
	if err != nil || string(marker) != keychainOriginContent {
		t.Fatalf("backfilled keychain origin marker = (%q, %v)", marker, err)
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

func TestRefSaveKeychainOriginFailureDoesNotWriteSecret(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	if err := os.Mkdir(path+keychainOriginSuffix, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	useFakeKeychain(t, fake)

	if err := NewRef(path).Save([]byte(`{"tokens":{"access_token":"secret"}}`)); err == nil {
		t.Fatal("Save must fail when the keychain origin marker cannot be persisted")
	}
	fake.mu.Lock()
	setCalls := fake.setCalls
	fake.mu.Unlock()
	if setCalls != 0 {
		t.Fatalf("keychain Set calls = %d, want 0 after marker failure", setCalls)
	}
	if _, err := fake.Get(serviceName, filepath.Base(path)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("secret reached keychain after marker failure: %v", err)
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
	if _, err := os.Stat(path + keychainOriginSuffix); err != nil {
		t.Fatalf("failed keychain delete must retain retry provenance: %v", err)
	}

	// A later file-mode delete must still retry the keychain cleanup even
	// though this legacy entry had no marker before the failed first attempt.
	t.Setenv(envCredStore, string(ModeFile))
	resetResolution()
	fake.mu.Lock()
	fake.delErr = nil
	fake.mu.Unlock()
	if err := NewRef(path).Delete(); err != nil {
		t.Fatalf("file-mode retry after keychain delete failure: %v", err)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(err) {
		t.Fatalf("successful retry retained keychain origin marker: %v", err)
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

// TestResolveModePriorityMatrix pins the full selection contract:
// non-empty MP_CRED_STORE > config `credentials:` (SetProcessMode) > default
// file, plus the test-binary hermeticity guard. resolveMode is pure, so the
// matrix runs without process state or a real keychain.
func TestResolveModePriorityMatrix(t *testing.T) {
	reachable := func() bool { return true }
	unreachable := func() bool { return false }
	cases := []struct {
		name       string
		env        string
		cfgMode    Mode
		testBinary bool
		probe      func() bool
		wantMode   Mode
		wantSource ModeSource
	}{
		{"env file beats config keychain", "file", ModeKeychain, false, reachable, ModeFile, SourceEnv},
		{"env keychain beats config file", "keychain", ModeFile, false, unreachable, ModeKeychain, SourceEnv},
		{"env auto probes reachable", "auto", ModeFile, false, reachable, ModeKeychain, SourceEnv},
		{"env auto probes unreachable", "auto", ModeKeychain, false, unreachable, ModeFile, SourceEnv},
		{"unknown env fails safe to file", "bogus", ModeKeychain, false, reachable, ModeFile, SourceEnv},
		{"config keychain applies without env", "", ModeKeychain, false, unreachable, ModeKeychain, SourceConfig},
		{"config file applies without env", "", ModeFile, false, reachable, ModeFile, SourceDefault},
		{"no env no config defaults file", "", "", false, reachable, ModeFile, SourceDefault},
		{"test binary guards config keychain", "", ModeKeychain, true, reachable, ModeFile, SourceDefault},
		{"test binary guards env auto probe", "auto", "", true, reachable, ModeFile, SourceEnv},
		{"test binary allows explicit env keychain", "keychain", "", true, reachable, ModeKeychain, SourceEnv},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, source := resolveMode(tc.env, tc.cfgMode, tc.testBinary, tc.probe)
			if mode != tc.wantMode || source != tc.wantSource {
				t.Fatalf("resolveMode(env=%q cfg=%q test=%t) = (%q, %q), want (%q, %q)",
					tc.env, tc.cfgMode, tc.testBinary, mode, source, tc.wantMode, tc.wantSource)
			}
		})
	}
}

// TestSetProcessModeRearmsResolution pins that config load points can swap the
// process mode and that the cached resolution re-arms (serve reload path).
func TestSetProcessModeRearmsResolution(t *testing.T) {
	t.Setenv(envCredStore, "") // no env override
	resetResolution()
	t.Cleanup(resetResolution)

	SetProcessMode(ModeFile)
	if mode, source := EffectiveMode(); mode != ModeFile || source != SourceDefault {
		t.Fatalf("EffectiveMode after SetProcessMode(file) = (%q, %q), want (file, default)", mode, source)
	}
	// Under the test-binary guard config-keychain still resolves to file, but
	// re-arming must actually recompute (the pure matrix above covers the
	// production outcome).
	SetProcessMode(ModeKeychain)
	if mode, _ := EffectiveMode(); mode != ModeFile {
		t.Fatalf("EffectiveMode under test guard = %q, want file", mode)
	}
	// An env override set later still wins once resolution re-arms.
	t.Setenv(envCredStore, string(ModeKeychain))
	resetResolution()
	if mode, source := EffectiveMode(); mode != ModeKeychain || source != SourceEnv {
		t.Fatalf("EffectiveMode with env override = (%q, %q), want (keychain, env MP_CRED_STORE)", mode, source)
	}
}

// TestConfigMismatchNote pins the one-line diagnostic for the only divergence
// possible after convergence: the env overrides OAuth stores while pools keep
// following config/default.
func TestConfigMismatchNote(t *testing.T) {
	cases := []struct {
		name      string
		env       string
		rawConfig string
		want      string // "" = no note; otherwise a required substring
	}{
		{"no env never mismatches", "", "keychain", ""},
		{"env file vs config keychain", "file", "keychain", "apikey pools use keychain (config credentials:), OAuth stores use file (env MP_CRED_STORE)"},
		{"env keychain vs default file", "keychain", "", "apikey pools use file (default), OAuth stores use keychain (env MP_CRED_STORE)"},
		{"env keychain vs config keychain", "keychain", "keychain", ""},
		{"env file vs config file", "file", "file", ""},
		{"env file vs default file", "file", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCredStore, tc.env)
			note := ConfigMismatchNote(tc.rawConfig)
			if tc.want == "" {
				if note != "" {
					t.Fatalf("ConfigMismatchNote(env=%q cfg=%q) = %q, want none", tc.env, tc.rawConfig, note)
				}
				return
			}
			if !strings.Contains(note, tc.want) {
				t.Fatalf("ConfigMismatchNote(env=%q cfg=%q) = %q, want substring %q", tc.env, tc.rawConfig, note, tc.want)
			}
		})
	}
}

// An oversized blob (ErrEntryTooLarge) physically cannot fit the backend.
// The lazy migration must keep serving the plaintext copy instead of locking
// the account out of credentials the process can still read — a
// deterministic size limit, unlike a reachability failure, which keeps
// failing closed (see TestRefLoadMigrationFailureKeepsPlaintextAuthoritative).
func TestRefLoadOversizedBlobKeepsServingPlaintext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	legacy := strings.Repeat("x", maxDarwinEntryBytes+1) // codex-sized OAuth archive
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	fake.setErr = ErrEntryTooLarge
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	got, err := ref.Load()
	if err != nil {
		t.Fatalf("oversized blob must keep serving the plaintext copy: %v", err)
	}
	if string(got) != legacy {
		t.Fatalf("Load returned %d bytes, want the %d-byte plaintext", len(got), len(legacy))
	}
	if _, serr := os.Stat(path + migratedSuffix); !os.IsNotExist(serr) {
		t.Fatal("no backup may be created when the store write was rejected")
	}
	// Retrying lands in the same place: the blob still does not fit.
	if _, err = ref.Load(); err != nil || len(fake.entries) != 0 {
		t.Fatalf("retry must stay on plaintext without keychain entries: (%v, %d entries)", err, len(fake.entries))
	}
}

// mapKeyringErr keeps the size-limit error class distinct so callers (and
// users) can tell "blob too large for this backend" apart from "backend
// unreachable" — the former has a config-level remedy (file mode), the
// latter needs the backend fixed.
func TestMapKeyringErrPreservesSizeClass(t *testing.T) {
	if err := mapKeyringErr(keyring.ErrSetDataTooBig); !errors.Is(err, ErrEntryTooLarge) {
		t.Errorf("ErrSetDataTooBig must map to ErrEntryTooLarge, got %v", err)
	}
	if err := mapKeyringErr(keyring.ErrSetDataTooBig); errors.Is(err, ErrUnavailable) {
		t.Error("size-limit errors must not be collapsed into ErrUnavailable")
	}
}

// Logout after switching keychain→file must still remove the keychain
// entry when the .migrated.bak archive marks that the blob once migrated
// there — otherwise the stale secret lingers in the keychain forever
// (decision: logout is the normal deletion path). Pure-file histories
// (no archive) never touch the backend.
func TestRefDeleteCleansKeychainAfterSwitchToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aqp_oauth_auth.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	if err := fake.Set(serviceName, "aqp_oauth_auth.json", []byte(`{"sso_session_cookie":"c"}`)); err != nil {
		t.Fatal(err)
	}
	// File mode + fake backend (no env opt-in: the test binary stays file).
	keychainOps = fake
	resetResolution()
	t.Cleanup(func() {
		keychainOps = realKeychainProvider{}
		resetResolution()
	})
	// No archive yet: file-mode Delete leaves the backend untouched.
	if err := NewRef(path).Delete(); err != nil {
		t.Fatal(err)
	}
	if _, err := fake.Get(serviceName, "aqp_oauth_auth.json"); err != nil {
		t.Fatalf("pure-file delete touched the backend: %v", err)
	}
	// With the legacy migration archive present, a failed first Delete must
	// first convert that provenance into the durable origin marker. Delete still
	// cleans the archive, but a later file-mode retry must not lose the evidence
	// that a keychain copy remains.
	if err := os.WriteFile(path+migratedSuffix, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.delErr = errors.New("delete rejected")
	fake.mu.Unlock()
	if err := NewRef(path).Delete(); err == nil {
		t.Fatal("legacy migration cleanup failure must propagate")
	}
	if _, err := os.Stat(path + keychainOriginSuffix); err != nil {
		t.Fatalf("legacy archive failure did not leave retry provenance: %v", err)
	}
	if _, serr := os.Stat(path + migratedSuffix); !os.IsNotExist(serr) {
		t.Fatal("archive cleanup must proceed after provenance is persisted")
	}
	fake.mu.Lock()
	fake.delErr = nil
	fake.mu.Unlock()
	if err := NewRef(path).Delete(); err != nil {
		t.Fatalf("retry legacy migration cleanup: %v", err)
	}
	if _, err := fake.Get(serviceName, "aqp_oauth_auth.json"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("keychain entry survived logout after mode switch: %v", err)
	}
	if _, serr := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(serr) {
		t.Fatal("origin marker not cleaned by successful retry")
	}
}

func TestRefDeleteCleansKeychainOnlySaveAfterSwitchToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	fake := newFakeKeychain(true)
	useFakeKeychain(t, fake)
	ref := NewRef(path)

	if err := ref.Save([]byte(`{"tokens":{"access_token":"secret"}}`)); err != nil {
		t.Fatalf("keychain-only Save: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("keychain-only Save created plaintext file: %v", err)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); err != nil {
		t.Fatalf("keychain-only Save did not persist origin marker: %v", err)
	}

	t.Setenv(envCredStore, string(ModeFile))
	resetResolution()
	fake.mu.Lock()
	fake.delErr = errors.New("delete rejected")
	fake.mu.Unlock()
	if err := ref.Delete(); err == nil {
		t.Fatal("keychain delete failure must propagate after mode switch")
	}
	if _, err := os.Stat(path + keychainOriginSuffix); err != nil {
		t.Fatalf("origin marker must survive failed keychain delete: %v", err)
	}
	fake.mu.Lock()
	fake.delErr = nil
	fake.mu.Unlock()
	if err := ref.Delete(); err != nil {
		t.Fatalf("Delete after keychain to file switch: %v", err)
	}
	if _, err := fake.Get(serviceName, ref.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("keychain entry survived mode switch delete: %v", err)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(err) {
		t.Fatalf("origin marker survived successful delete: %v", err)
	}

	if err := ref.Delete(); err != nil {
		t.Fatalf("idempotent second Delete: %v", err)
	}
	fake.mu.Lock()
	deleteCalls := fake.delCalls
	fake.mu.Unlock()
	if deleteCalls != 2 {
		t.Fatalf("keychain Delete calls = %d, want failed attempt plus one retry", deleteCalls)
	}
}

func TestRefDeletePureFileHistoryNeverTouchesKeychain(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	fake := newFakeKeychain(true)
	keychainOps = fake
	t.Cleanup(func() { keychainOps = realKeychainProvider{} })
	forceFileMode(t)
	ref := NewRef(path)

	if err := ref.Save([]byte(`{"tokens":{"access_token":"secret"}}`)); err != nil {
		t.Fatalf("file Save: %v", err)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(err) {
		t.Fatalf("pure-file Save created keychain origin marker: %v", err)
	}
	if err := ref.Delete(); err != nil {
		t.Fatalf("file Delete: %v", err)
	}
	fake.mu.Lock()
	deleteCalls := fake.delCalls
	fake.mu.Unlock()
	if deleteCalls != 0 {
		t.Fatalf("pure-file Delete touched keychain %d times", deleteCalls)
	}
}

// A13 regression: the auto-mode keychain probe (external call, bounded by
// keychainOpTimeout) must run WITHOUT modeMu — every serve-reload re-arm
// re-triggers it, and holding the mutex across it would block all concurrent
// ResolvedMode/Ref.Load readers for the probe's full budget. Concurrent
// resolutions may duplicate the probe (first committer wins); all must agree.
// effectiveMode is driven directly with testBinary=false because the
// testing.Testing() guard keeps package-level auto mode off the real probe.
func TestEffectiveModeKeepsLockFreeDuringAutoProbe(t *testing.T) {
	probe := newScriptedProbe(0)
	keychainOps = probe
	t.Cleanup(func() { keychainOps = realKeychainProvider{} })
	resetResolution()
	t.Cleanup(resetResolution)

	const readers = 8
	type result struct {
		mode   Mode
		source ModeSource
	}
	results := make(chan result, readers)
	for i := 0; i < readers; i++ {
		go func() {
			mode, source := effectiveMode(string(ModeAuto), false)
			results <- result{mode, source}
		}()
	}
	// All readers are now in flight inside the probe (each announced before
	// blocking on release) — none can have committed yet.
	for i := 0; i < readers; i++ {
		<-probe.entered
	}

	// While the probe is blocked, modeMu must be free for other takers. The
	// timeout is a failure watchdog, not the ordering mechanism.
	acquired := make(chan struct{})
	go func() {
		modeMu.Lock()
		close(acquired)
		modeMu.Unlock()
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("modeMu is held while the auto-mode keychain probe is in flight")
	}

	close(probe.release)
	for i := 0; i < readers; i++ {
		select {
		case r := <-results:
			if r.mode != ModeKeychain || r.source != SourceEnv {
				t.Fatalf("concurrent resolution = (%q, %q), want (keychain, env MP_CRED_STORE)", r.mode, r.source)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("EffectiveMode readers stalled after the probe was released")
		}
	}
	// Every reader probed exactly once (duplicated probes are the documented
	// cost), and the committed cache serves later readers without re-probing.
	if calls := probe.count(); calls != readers {
		t.Fatalf("probe calls = %d, want %d", calls, readers)
	}
	mode, source := effectiveMode(string(ModeAuto), false)
	if mode != ModeKeychain || source != SourceEnv {
		t.Fatalf("post-commit resolution = (%q, %q), want cached (keychain, env)", mode, source)
	}
	if calls := probe.count(); calls != readers {
		t.Fatalf("cached resolution re-probed: %d calls", calls)
	}
}

// A13 companion: a SetProcessMode re-arm landing while a probe is in flight
// must invalidate that resolution's inputs — the stale result is recomputed
// (and re-probed), never cached, keeping serve-reload semantics intact with
// the probe outside the lock.
func TestEffectiveModeRecomputesAfterRearmDuringProbe(t *testing.T) {
	probe := newScriptedProbe(1) // first probe reachable, later ones not
	keychainOps = probe
	t.Cleanup(func() { keychainOps = realKeychainProvider{} })
	resetResolution()
	t.Cleanup(resetResolution)

	type result struct {
		mode   Mode
		source ModeSource
	}
	results := make(chan result, 1)
	go func() {
		mode, source := effectiveMode(string(ModeAuto), false)
		results <- result{mode, source}
	}()
	<-probe.entered
	SetProcessMode(ModeKeychain) // serve-reload re-arm, mid-probe
	close(probe.release)

	select {
	case r := <-results:
		// The pre-re-arm probe answered reachable (keychain); only the
		// recomputation with fresh inputs — whose probe answers unreachable —
		// may be returned and cached.
		if r.mode != ModeFile || r.source != SourceEnv {
			t.Fatalf("resolution after mid-probe re-arm = (%q, %q), want recomputed (file, env)", r.mode, r.source)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resolution stalled after mid-probe re-arm")
	}
	if calls := probe.count(); calls != 2 {
		t.Fatalf("probe calls = %d, want initial plus exactly one recomputation", calls)
	}
	mode, source := effectiveMode(string(ModeAuto), false)
	if mode != ModeFile || source != SourceEnv {
		t.Fatalf("cached resolution after re-arm = (%q, %q), want (file, env)", mode, source)
	}
	if calls := probe.count(); calls != 2 {
		t.Fatalf("cached resolution re-probed: %d calls", calls)
	}
}

// C-1 regression: a keychain Set failing with ErrUnavailable models the
// timeout path whose abandoned goroutine may still land the write later. The
// origin marker must survive so a later Delete — even after switching back to
// file mode — can still clean an entry that materialized late; a marker left
// behind by a write that never landed costs only a tolerated not-found.
func TestRefSaveSetTimeoutKeepsOriginMarkerAndStaysDeletable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "codex_oauth_auth.json")
	fake := newFakeKeychain(true)
	fake.setErr = ErrUnavailable
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	if err := ref.Save([]byte(`{"tokens":{"access_token":"secret"}}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Save against timing-out backend = %v, want ErrUnavailable", err)
	}
	// (a) Provenance survives the ambiguous failure.
	marker, err := os.ReadFile(path + keychainOriginSuffix)
	if err != nil || string(marker) != keychainOriginContent {
		t.Fatalf("origin marker after timed-out Set = (%q, %v), want retained marker", marker, err)
	}
	if _, err := fake.Get(serviceName, ref.Name); !errors.Is(err, ErrNotFound) {
		t.Fatalf("fake backend unexpectedly holds the entry: %v", err)
	}

	// (b) After switching back to file mode, Delete tolerates the absent
	// keychain entry (backend not-found) and still cleans the marker.
	t.Setenv(envCredStore, string(ModeFile))
	resetResolution()
	fake.mu.Lock()
	fake.setErr = nil
	fake.delErr = ErrNotFound // the entry genuinely never landed
	fake.mu.Unlock()
	if err := ref.Delete(); err != nil {
		t.Fatalf("Delete with absent keychain entry = %v, want tolerated nil", err)
	}
	fake.mu.Lock()
	delCalls := fake.delCalls
	fake.mu.Unlock()
	if delCalls != 1 {
		t.Fatalf("keychain Delete calls = %d, want 1 (retained marker drove the cleanup)", delCalls)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(err) {
		t.Fatal("origin marker must be cleaned once the backend reports absence")
	}
}

// C-1 companion for the lazy-migration path: a migration Set failing with
// ErrUnavailable keeps the plaintext authoritative AND keeps the origin
// marker, so the cross-mode cleanup contract above holds there too.
func TestRefLoadMigrationSetTimeoutKeepsOriginMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kimi-code_apikeys.json")
	legacy := []byte(`{"version":1,"accounts":[{"id":"a1","label":"a1","api_key":"sk-x"}]}`)
	if err := os.WriteFile(path, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := newFakeKeychain(true)
	fake.setErr = ErrUnavailable
	useFakeKeychain(t, fake)

	ref := NewRef(path)
	if _, err := ref.Load(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("migrating Load against timing-out backend = %v, want ErrUnavailable", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(legacy) {
		t.Fatalf("plaintext must survive the failed migration: (%q, %v)", got, err)
	}
	marker, err := os.ReadFile(path + keychainOriginSuffix)
	if err != nil || string(marker) != keychainOriginContent {
		t.Fatalf("origin marker after timed-out migration Set = (%q, %v), want retained marker", marker, err)
	}

	t.Setenv(envCredStore, string(ModeFile))
	resetResolution()
	fake.mu.Lock()
	fake.setErr = nil
	fake.delErr = ErrNotFound
	fake.mu.Unlock()
	if err := ref.Delete(); err != nil {
		t.Fatalf("Delete with absent keychain entry = %v, want tolerated nil", err)
	}
	if _, err := os.Stat(path + keychainOriginSuffix); !os.IsNotExist(err) {
		t.Fatal("origin marker must be cleaned once the backend reports absence")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plaintext must be removed by the successful Delete")
	}
}
