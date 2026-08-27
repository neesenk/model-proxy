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
// Mode selection (env MP_CRED_STORE > config `credentials:` > default file):
//   - env "file"/"keychain": explicit override of any config selection.
//   - env "auto": explicit override that probes keychain reachability (the
//     pre-config default behavior, kept as an opt-in).
//   - env unset: the config `credentials:` value applied via SetProcessMode
//     (file|keychain); with no config loaded, plain files.
//
// "keychain" (either source) is fail-closed: an unreachable backend errors at
// op time instead of silently downgrading.
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
	"fmt"
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

// ModeSource records where the effective mode came from. Surfaced by
// `config check` and startup logs so a credentials-mode mismatch shows each
// side's effective value AND origin.
type ModeSource string

const (
	SourceEnv     ModeSource = "env MP_CRED_STORE"
	SourceConfig  ModeSource = "config credentials:"
	SourceDefault ModeSource = "default"
)

var (
	// modeMu guards the process-level config mode and the cached resolution.
	// SetProcessMode runs at config load points — including serve reload, while
	// request-path readers may resolve concurrently — so the cache must be
	// re-armable and race-clean (a sync.Once cannot).
	modeMu         sync.Mutex
	configMode     Mode
	resolved       Mode
	resolvedSource ModeSource
	resolvedOK     bool
)

// SetProcessMode applies the config `credentials:` selection to OAuth blob
// storage for the rest of the process. A non-empty MP_CRED_STORE remains an
// explicit override on top of it. Config load points call this together with
// the apikey-pool backend selection (accounts.SetProcessCredentialsMode wraps
// both); re-calling on serve reload re-arms resolution so a `credentials:`
// change takes effect without a restart.
func SetProcessMode(mode Mode) {
	modeMu.Lock()
	defer modeMu.Unlock()
	configMode = mode
	resolvedOK = false
}

// ResolvedMode returns the effective storage mode, resolving once per process
// (re-armed by SetProcessMode). Resolution order: MP_CRED_STORE env > config
// mode > test-binary guard / default file.
func ResolvedMode() Mode {
	mode, _ := EffectiveMode()
	return mode
}

// EffectiveMode returns the effective storage mode and where it came from.
func EffectiveMode() (Mode, ModeSource) {
	modeMu.Lock()
	defer modeMu.Unlock()
	if !resolvedOK {
		resolved, resolvedSource = computeModeLocked()
		resolvedOK = true
	}
	return resolved, resolvedSource
}

func computeModeLocked() (Mode, ModeSource) {
	env := strings.ToLower(strings.TrimSpace(os.Getenv(envCredStore)))
	return resolveMode(env, configMode, testing.Testing(), keychainAvailable)
}

// resolveMode maps (env override, config mode, test-binary guard, keychain
// probe) onto the effective mode and its source. Pure: tests drive the full
// priority matrix without touching process state or the real keychain.
func resolveMode(env string, cfgMode Mode, testBinary bool, probe func() bool) (Mode, ModeSource) {
	switch env {
	case string(ModeFile):
		return ModeFile, SourceEnv
	case string(ModeKeychain):
		// Explicit opt-in: no silent downgrade. An unreachable backend makes
		// every operation fail closed (ErrUnavailable) until fixed.
		return ModeKeychain, SourceEnv
	case string(ModeAuto):
		if !testBinary && probe() {
			return ModeKeychain, SourceEnv
		}
		return ModeFile, SourceEnv
	case "":
		// No env override: config selects. Test binaries must never let config
		// alone reach the real OS keychain — keychain-semantics tests inject
		// fake ops AND set the env explicitly.
		if !testBinary && cfgMode == ModeKeychain {
			return ModeKeychain, SourceConfig
		}
		return ModeFile, SourceDefault
	default:
		// Unknown value: stay on the historical file layout rather than guess.
		return ModeFile, SourceEnv
	}
}

// ConfigMismatchNote reports a one-line warning when MP_CRED_STORE (explicit
// env override, OAuth blobs only) disagrees with the config `credentials:`
// mode that drives the apikey-pool backend. rawConfig is the raw config value
// ("" = unset → file). Returns "" when both sides converge: without an env
// override, config/default drive both stores identically, so a mismatch is
// only possible when the env is set. The note carries modes and sources only —
// never credential material.
func ConfigMismatchNote(rawConfig string) string {
	env := strings.ToLower(strings.TrimSpace(os.Getenv(envCredStore)))
	if env == "" {
		return ""
	}
	envMode, _ := resolveMode(env, ModeFile, testing.Testing(), keychainAvailable)
	cfgMode := ModeFile
	cfgSource := SourceDefault
	if strings.EqualFold(strings.TrimSpace(rawConfig), string(ModeKeychain)) {
		cfgMode = ModeKeychain
		cfgSource = SourceConfig
	}
	if envMode == cfgMode {
		return ""
	}
	return fmt.Sprintf("credentials mode mismatch: apikey pools use %s (%s), OAuth stores use %s (%s) — MP_CRED_STORE overrides only OAuth stores; align `credentials:` or unset the env to converge",
		cfgMode, cfgSource, envMode, SourceEnv)
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
		return AtomicWriteFile(r.Path, blob, 0o600)
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

// AtomicWriteFile writes via a unique temp file in the target directory plus
// fsync and rename. Mirrors provider.persist.atomicWriteFile (which cannot be
// imported here — provider depends on credstore, not the reverse). OAuth/SSO
// stores are rewritten with ROTATED tokens mid-flight: a crash during a direct
// write leaves a truncated file whose old refresh token is already invalidated
// upstream and whose new token was never persisted — the account locks until a
// full re-login (docs/engineering/pitfalls.md #18). Exported for the accounts
// pool metadata file, which needs the same crash-safety without routing
// through Ref (its backend is selected by config, not ResolvedMode).
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
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
