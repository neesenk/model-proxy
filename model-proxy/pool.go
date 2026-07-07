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

func savePool(name string, p credentialPool) error {
	path := poolPath(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if p.Version == 0 {
		p.Version = 1
	}
	sort.SliceStable(p.Accounts, func(i, j int) bool { return p.Accounts[i].ID < p.Accounts[j].ID })
	data, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(path, data, 0o600)
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
