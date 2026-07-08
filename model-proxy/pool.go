package main

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

// accountCred is one account's raw credentials, independent of storage format.
// APIKey is used for Bearer/x-api-key auth; AccessKey/SecretKey are volcengine's
// V4-signing pair (used by GetAFPUsage quota).
type accountCred struct {
	APIKey    string
	AccessKey string
	SecretKey string
}

type poolAccount struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key,omitempty"` // volcengine
	SecretKey string `json:"secret_key,omitempty"` // volcengine
	AddedAt   string `json:"added_at"`
}

type credentialPool struct {
	Version  int           `json:"version"`
	Accounts []poolAccount `json:"accounts"`
}

func (a poolAccount) cred() accountCred {
	return accountCred{APIKey: a.APIKey, AccessKey: a.AccessKey, SecretKey: a.SecretKey}
}

func poolPath(name string) string {
	return filepath.Join(homeDir(), ".model-proxy", name+"_apikeys.json")
}
func singularPoolPath(name string) string {
	return filepath.Join(homeDir(), ".model-proxy", name+"_apikey.json")
}

// loadPool reads the plural pool file; if absent, wraps the legacy singular
// <name>_apikey.json as a read-only 1-entry pool. A missing pool is empty (not
// an error) — the caller treats "no accounts" as "not logged in".
func loadPool(name, providerID string) (credentialPool, error) {
	data, err := os.ReadFile(poolPath(name))
	if err == nil {
		var p credentialPool
		if err := json.Unmarshal(data, &p); err != nil {
			return credentialPool{}, fmt.Errorf("parse %s: %w", poolPath(name), err)
		}
		return p, nil
	}
	if !os.IsNotExist(err) {
		return credentialPool{}, err
	}
	// Fall back to legacy singular file.
	sdata, serr := os.ReadFile(singularPoolPath(name))
	if serr != nil {
		if os.IsNotExist(serr) {
			return credentialPool{}, nil
		}
		return credentialPool{}, serr
	}
	var v struct {
		APIKey    string `json:"api_key"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
	}
	if err := json.Unmarshal(sdata, &v); err != nil {
		return credentialPool{}, fmt.Errorf("parse %s: %w", singularPoolPath(name), err)
	}
	cred := accountCred{APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey}
	return credentialPool{
		Version: 1,
		Accounts: []poolAccount{{
			ID:     accountIDFor(providerID, cred),
			Label:  providerID,
			APIKey: v.APIKey, AccessKey: v.AccessKey, SecretKey: v.SecretKey,
		}},
	}, nil
}

// savePool writes the pool atomically: marshal → write path+".tmp" → os.Rename
// onto path. Rename makes the on-disk file appear whole or not at all, so a crash
// mid-write never leaves a truncated pool file (the read side never observes a
// half-written JSON). Mirrors quota.go:persist. The temp file is 0o600 and lives
// in the same dir (MkdirAll 0o700), so rename is a same-directory atomic move.
func savePool(name string, p credentialPool) error {
	path := poolPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
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
	return os.Rename(tmp, path)
}

// withPoolLock runs fn while holding a cross-process lock for the named pool.
// stdlib only (no golang.org/x/sys → no flock); portable across daemon_unix.go
// and daemon_windows.go. The arbiter is an O_CREATE|O_EXCL lockfile at
// poolPath(name)+".lock": exactly one process can create it, the holder removes
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

func withPoolLock(name string, fn func() error) error {
	lockPath := poolPath(name) + ".lock"
	// Ensure the parent (~/.model-proxy) exists before the O_CREATE below —
	// O_CREATE does not create parent dirs, and on a first-ever login savePool
	// (which does MkdirAll) has not yet run because it runs INSIDE this lock.
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
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

// accountIDFor returns a stable per-account identifier for dedup + virtual-id
// suffixing. volcengine keys by AccessKey (account-level); other apikey
// providers hash the APIKey (key-level). volcengine with no AccessKey falls
// back to the key hash.
func accountIDFor(providerID string, c accountCred) string {
	if providerID == "volcengine" && c.AccessKey != "" {
		return c.AccessKey
	}
	sum := sha256.Sum256([]byte(c.APIKey))
	return hex.EncodeToString(sum[:])[:16]
}

// nowTS is a helper for AddedAt timestamps (tests can't call time.Now directly
// in some harnesses; kept simple here).
func nowTS() string { return time.Now().UTC().Format(time.RFC3339) }
