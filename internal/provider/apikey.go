package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"model-proxy/internal/credstore"
)

// ApiKeyBase provides shared auth-file storage and Bearer injection for
// API-key-based providers (zhipu, deepseek, etc.). It does NOT implement
// Login/Logout/Usage — each concrete provider adds its own.
//
// Auth storage goes through credstore.Ref keyed by the auth filename: plain
// file under ~/.model-proxy by default, OS keychain entry when keychain mode
// is active (with lazy migration from the legacy file).
type ApiKeyBase struct {
	authFile string // expanded path
	bound    bool   // true → use cached key, never touch the store (pool-bound)

	mu     sync.Mutex
	cached string
}

// NewApiKeyBase creates an ApiKeyBase for the given provider name.
func NewApiKeyBase(providerName string) *ApiKeyBase {
	home, _ := os.UserHomeDir()
	return &ApiKeyBase{
		authFile: filepath.Join(home, ".model-proxy", providerName+"_apikey.json"),
	}
}

// NewApiKeyBaseWithKey binds an in-memory key (used when a provider is unrolled
// from a credential-pool entry). Store reads/writes are skipped — the bound key
// is the single source of truth for this instance.
func NewApiKeyBaseWithKey(providerName, key string) *ApiKeyBase {
	base := NewApiKeyBase(providerName)
	base.bound = true
	base.cached = key
	return base
}

// AuthHeaders reads the API key from the auth file and injects it as Bearer.
func (b *ApiKeyBase) AuthHeaders(req *http.Request) error {
	key, err := b.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Del("x-api-key")
	return nil
}

// Refresh clears the cached key (next AuthHeaders call re-reads the file).
// A bound base has no file to re-read and its key is immutable → no-op.
func (b *ApiKeyBase) Refresh() error {
	if b.bound {
		return nil
	}
	b.mu.Lock()
	b.cached = ""
	b.mu.Unlock()
	return nil
}

// LoadKey reads the API key from the auth file (with in-memory cache).
// A bound base (NewApiKeyBaseWithKey) short-circuits to its cached key.
func (b *ApiKeyBase) LoadKey() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bound || b.cached != "" {
		return b.cached, nil
	}
	data, err := credstore.NewRef(b.authFile).Load()
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) {
			return "", fmt.Errorf("not logged in; run `model-proxy login` for this provider")
		}
		return "", err
	}
	var v struct {
		APIKey string `json:"api_key"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("parse %s: %w", b.authFile, err)
	}
	if v.APIKey == "" {
		return "", fmt.Errorf("no api_key in %s", b.authFile)
	}
	b.cached = v.APIKey
	return b.cached, nil
}

// SaveKey writes the API key through the credential store (file mode: 0600,
// parent dir 0700, temp+fsync+rename — a crash mid-write must not destroy the
// only stored key; pitfalls #18 pattern). A bound base is never the source of
// truth for the store → no-op.
func (b *ApiKeyBase) SaveKey(key string) error {
	if b.bound {
		return nil
	}
	data, _ := json.MarshalIndent(map[string]string{"api_key": key}, "", "  ")
	return credstore.NewRef(b.authFile).Save(data)
}

// DeleteKey removes the stored credential (logout). A bound base is never the
// source of truth for the store → no-op.
func (b *ApiKeyBase) DeleteKey() error {
	if b.bound {
		return nil
	}
	return credstore.NewRef(b.authFile).Delete()
}

// AuthFilePath returns the auth file path (for logging/debugging).
func (b *ApiKeyBase) AuthFilePath() string {
	return b.authFile
}
