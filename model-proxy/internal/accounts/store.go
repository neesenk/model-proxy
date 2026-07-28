// Package accounts owns API-key account identity and on-disk pool storage.
package accounts

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Credentials is one account's raw credential tuple, independent of storage format.
// APIKey is used for Bearer/x-api-key auth; AccessKey/SecretKey are volcengine's
// V4-signing pair (used by GetAFPUsage quota).
type Credentials struct {
	APIKey    string
	AccessKey string
	SecretKey string
}

type Account struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key,omitempty"` // volcengine
	SecretKey string `json:"secret_key,omitempty"` // volcengine
	AddedAt   string `json:"added_at"`
}

type Pool struct {
	Version  int       `json:"version"`
	Accounts []Account `json:"accounts"`
}

// Credentials returns the complete credential tuple bound to this account.
func (a Account) Credentials() Credentials {
	return Credentials{APIKey: a.APIKey, AccessKey: a.AccessKey, SecretKey: a.SecretKey}
}

// Store resolves account files relative to one user home directory.
type Store struct {
	directory string
}

func NewStore(homeDir string) Store {
	return Store{directory: filepath.Join(homeDir, ".model-proxy")}
}

func (s Store) PoolPath(name string) string {
	return filepath.Join(s.directory, name+"_apikeys.json")
}

func (s Store) LegacyPath(name string) string {
	return filepath.Join(s.directory, name+"_apikey.json")
}

func (s Store) ensureDirectory() error {
	if err := os.MkdirAll(s.directory, 0o700); err != nil {
		return err
	}
	return os.Chmod(s.directory, 0o700)
}

// Load reads the plural pool file; if absent, wraps the legacy singular
// <name>_apikey.json as a read-only 1-entry pool. A missing pool is empty (not
// an error) — the caller treats "no accounts" as "not logged in".
func (s Store) Load(name, providerID string) (Pool, error) {
	data, err := os.ReadFile(s.PoolPath(name))
	if err == nil {
		var p Pool
		if err := json.Unmarshal(data, &p); err != nil {
			return Pool{}, fmt.Errorf("parse %s: %w", s.PoolPath(name), err)
		}
		return p, nil
	}
	if !os.IsNotExist(err) {
		return Pool{}, err
	}
	// Fall back to legacy singular file.
	sdata, serr := os.ReadFile(s.LegacyPath(name))
	if serr != nil {
		if os.IsNotExist(serr) {
			return Pool{}, nil
		}
		return Pool{}, serr
	}
	var v struct {
		APIKey    string `json:"api_key"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(sdata, &v); err != nil {
		return Pool{}, fmt.Errorf("parse %s: %w", s.LegacyPath(name), err)
	}
	cred := Credentials{APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey}
	return Pool{
		Version: 1,
		Accounts: []Account{{
			ID:     AccountID(providerID, cred),
			Label:  providerID,
			APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey,
		}},
	}, nil
}

// Save writes the pool atomically: marshal → write path+".tmp" → os.Rename
// onto path. Rename makes the on-disk file appear whole or not at all, so a crash
// mid-write never leaves a truncated pool file (the read side never observes a
// half-written JSON). Mirrors quota.go:persist. The temp file is 0o600 and lives
// in the same dir (MkdirAll 0o700), so rename is a same-directory atomic move.
func (s Store) Save(name string, p Pool) error {
	path := s.PoolPath(name)
	if err := s.ensureDirectory(); err != nil {
		return err
	}
	if p.Version == 0 {
		p.Version = 1
	}
	sort.SliceStable(p.Accounts, func(i, j int) bool { return p.Accounts[i].ID < p.Accounts[j].ID })
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal pool %s: %w", name, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WithLock runs fn while holding a cross-process lock for the named pool.
// stdlib only (no golang.org/x/sys → no flock); portable across daemon_unix.go
// and daemon_windows.go. The arbiter is an O_CREATE|O_EXCL lockfile at
// PoolPath(name)+".lock": exactly one process can create it, the holder removes
// it on return (ALWAYS, even on fn error). A crash strands a stale lockfile,
// recovered by mtime — if older than poolLockStaleAge, a waiter removes it and
// retries the create. SAFETY: the lock is held ONLY for the ms-scale
// load→modify→save, NEVER during user input (see runApiKeyLoginWithInput /
// cmdLogout, which read stdin outside the lock); poolLockStaleAge therefore
// dwarfs any legitimate hold, so stealing a >poolLockStaleAge lockfile can never
// race a live saver. Read-only callers (buildProviders, cmdUsage, FetchModels,
// poolVirtuals) skip the lock — atomic rename already gives them a consistent
// (whole-file) read.
const (
	poolLockRetry    = 50 * time.Millisecond
	poolLockMaxWait  = 10 * time.Second
	poolLockStaleAge = 60 * time.Second
)

func (s Store) WithLock(name string, fn func() error) error {
	lockPath := s.PoolPath(name) + ".lock"
	// Ensure the parent (~/.model-proxy) exists before the O_CREATE below —
	// O_CREATE does not create parent dirs, and on a first-ever login savePool
	// (which does MkdirAll) has not yet run because it runs INSIDE this lock.
	if err := s.ensureDirectory(); err != nil {
		return fmt.Errorf("pool %s lock dir: %w", name, err)
	}
	deadline := time.Now().Add(poolLockMaxWait)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			// PID is diagnostic (a human inspecting a stranded lockfile); O_EXCL
			// is the real arbiter, so a write error does not fail the lock. Close
			// immediately so the (deferred) Remove works on Windows too.
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			break
		}
		if !os.IsExist(err) {
			return fmt.Errorf("pool %s lock create: %w", name, err)
		}
		// Lockfile exists. Decide stale vs. busy: a mtime older than
		// poolLockStaleAge cannot belong to a live holder (holds are ms-scale),
		// so remove it and retry the O_EXCL create. If the file vanished between
		// the create attempt and the stat (another waiter recovered it), just
		// loop and retry the create.
		info, statErr := os.Stat(lockPath)
		if statErr == nil {
			if time.Since(info.ModTime()) > poolLockStaleAge {
				_ = os.Remove(lockPath)
				continue
			}
		} else if os.IsNotExist(statErr) {
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pool %s locked by another process after %s; if stale remove %s", name, poolLockMaxWait, lockPath)
		}
		time.Sleep(poolLockRetry)
	}
	defer os.Remove(lockPath)
	return fn()
}

// AccountID returns a stable per-account identifier for dedup + virtual-id
// suffixing. volcengine keys by AccessKey (account-level); other apikey
// providers hash the APIKey (key-level). volcengine with no AccessKey falls
// back to the key hash.
func AccountID(providerID string, c Credentials) string {
	if providerID == "volcengine" && c.AccessKey != "" {
		return c.AccessKey
	}
	sum := sha256.Sum256([]byte(c.APIKey))
	return hex.EncodeToString(sum[:])[:16]
}

// Timestamp formats an account-added time in the persisted UTC representation.
func Timestamp(now time.Time) string { return now.UTC().Format(time.RFC3339) }
