// Package credstore owns credential-at-rest storage: one stable seam through
// which every credential blob (API-key pools, OAuth stores, SSO cookies) is
// read and written, either as a plain file under ~/.model-proxy (the historical
// layout) or as an entry in the OS keychain.
//
// The unit of storage is a byte blob named by the credential FILENAME (e.g.
// "codex_oauth_auth.json"). The store never interprets blob contents; JSON
// schemas stay owned by their existing packages. This keeps the migration
// per-entry and lazy: the first successful read of a legacy plaintext file in
// keychain mode copies the blob into the keychain and renames the file to
// <path>.migrated.bak (one rollback generation kept).
//
// Mode selection (MP_CRED_STORE):
//   - "auto" (default): keychain when reachable, otherwise plain files.
//   - "file": always plain files (identical to pre-credstore behavior).
//   - "keychain": force keychain; an unreachable backend fails closed at op time.
//
// Security invariants (AGENTS.md red line 3):
//   - blobs never appear in logs, errors, or test output — errors carry only
//     paths and wrapped causes;
//   - a failed save NEVER destroys the previous authoritative copy (fail-closed);
//   - test binaries resolve to file mode unless a test explicitly opts in, so
//     no test ever touches the real OS keychain.
package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// serviceName is the keychain service namespace shared by all model-proxy
// entries. Account names are credential filenames.
const serviceName = "model-proxy"

// probeAccount is only probed for reachability; its absence (ErrNotFound)
// proves the keychain answers queries without touching any real entry.
const probeAccount = ".probe"

// Mode selects where credential blobs live.
type Mode string

const (
	ModeAuto     Mode = "auto"
	ModeFile     Mode = "file"
	ModeKeychain Mode = "keychain"
)

var (
	// ErrNotFound reports that the credential blob does not exist in the
	// active store. Callers translate it into domain errors ("not logged in").
	ErrNotFound = errors.New("credential not found")
	// ErrUnavailable reports that the configured store backend cannot be
	// reached (e.g. explicit keychain mode with no secret service). Fail-closed:
	// callers surface it instead of falling back silently.
	ErrUnavailable = errors.New("credential store unavailable")
)

var (
	resolveOnce sync.Once
	resolved    Mode
)

// ResolvedMode returns the effective storage mode, resolving once per process.
// Resolution order: MP_CRED_STORE env > test-binary guard > availability probe.
func ResolvedMode() Mode {
	resolveOnce.Do(func() { resolved = computeMode() })
	return resolved
}

func computeMode() Mode {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(envCredStore))) {
	case string(ModeFile):
		return ModeFile
	case string(ModeKeychain):
		// Explicit opt-in: no silent downgrade. An unreachable backend makes
		// every operation fail closed (ErrUnavailable) until fixed.
		return ModeKeychain
	case "", string(ModeAuto):
	default:
		// Unknown value: stay on the historical file layout rather than guess.
		return ModeFile
	}
	if testing.Testing() {
		// Test binaries must never probe or mutate the real OS keychain.
		// Keychain-semantics tests inject fake ops AND set the env explicitly.
		return ModeFile
	}
	if keychainAvailable() {
		return ModeKeychain
	}
	return ModeFile
}

const envCredStore = "MP_CRED_STORE"

// Ref identifies one credential blob. Name is the stable store key (the
// credential filename); Path is the canonical fallback file location. All
// provider-owned credential I/O funnels through a Ref so the storage backend
// stays swappable without touching call sites.
type Ref struct {
	Name string
	Path string
}

// NewRef builds a Ref for a canonical credential path.
func NewRef(path string) Ref {
	return Ref{Name: filepath.Base(path), Path: path}
}

// Load returns the stored blob. File mode reads Path directly (historical
// semantics). Keychain mode prefers the keychain entry; on miss it migrates
// lazily from a legacy plaintext Path (copy into keychain, rename file to
// <Path>.migrated.bak). Returns ErrNotFound when neither store has the blob.
func (r Ref) Load() ([]byte, error) {
	if ResolvedMode() != ModeKeychain {
		data, err := os.ReadFile(r.Path)
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return data, err
	}
	blob, err := keychainOps.Get(serviceName, r.Name)
	switch {
	case err == nil:
		return blob, nil
	case errors.Is(err, ErrUnavailable):
		return nil, err
	case !errors.Is(err, ErrNotFound):
		return nil, err
	}
	// Miss: migrate from the legacy plaintext file when present.
	data, ferr := os.ReadFile(r.Path)
	if ferr != nil {
		if os.IsNotExist(ferr) {
			return nil, ErrNotFound
		}
		return nil, ferr
	}
	if serr := keychainOps.Set(serviceName, r.Name, data); serr != nil {
		// Fail closed: keep serving the file next time, destroy nothing.
		return nil, serr
	}
	backupMigrated(r.Path)
	return data, nil
}

// Save persists blob. File mode keeps the exact historical contract: parent
// dir 0700, temp+fsync+rename at 0600 (pitfalls #18 — a crash mid-write must
// not truncate the only copy of rotated credentials). Keychain mode writes the
// entry first and only then moves any stale plaintext file aside, so the
// keychain is authoritative before the file stops being present.
func (r Ref) Save(blob []byte) error {
	if ResolvedMode() != ModeKeychain {
		if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
			return err
		}
		return atomicWriteFile(r.Path, blob, 0o600)
	}
	if err := keychainOps.Set(serviceName, r.Name, blob); err != nil {
		return err
	}
	if _, err := os.Stat(r.Path); err == nil {
		backupMigrated(r.Path)
	}
	return nil
}

// Delete removes the blob everywhere it may exist. Every step treats
// "absent" as success — logout must be idempotent even across mode switches
// (e.g. logged in under file mode, logging out under keychain mode).
func (r Ref) Delete() error {
	var firstErr error
	if ResolvedMode() == ModeKeychain {
		if err := keychainOps.Delete(serviceName, r.Name); err != nil && !errors.Is(err, ErrNotFound) && firstErr == nil {
			firstErr = err
		}
	}
	for _, path := range []string{r.Path, r.Path + migratedSuffix} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

const migratedSuffix = ".migrated.bak"

// backupMigrated moves a legacy plaintext credential file to
// <path>.migrated.bak, keeping exactly one rollback generation. Best-effort:
// a locked file leaves the plaintext in place (still readable by the lazy
// migration next round) and never blocks the caller.
func backupMigrated(path string) {
	bak := path + migratedSuffix
	_ = os.Remove(bak)
	_ = os.Rename(path, bak)
}

// atomicWriteFile writes via a unique temp file in the target directory plus
// fsync and rename. Mirrors provider.persist.atomicWriteFile (which cannot be
// imported here — provider depends on credstore, not the reverse). OAuth/SSO
// stores are rewritten with ROTATED tokens mid-flight: a crash during a direct
// write leaves a truncated file whose old refresh token is already invalidated
// upstream and whose new token was never persisted — the account locks until a
// full re-login (docs/engineering/pitfalls.md #18).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Consume any historical fixed-name ".tmp" sibling left by pre-credstore
	// writers (or a crashed one): successful save means no residue.
	_ = os.Remove(path + ".tmp")
	return nil
}
