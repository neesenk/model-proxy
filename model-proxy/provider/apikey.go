package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// ApiKeyBase provides shared auth-file storage and Bearer injection for
// API-key-based providers (zhipu, deepseek, etc.). It does NOT implement
// Login/Logout/Usage — each concrete provider adds its own.
//
// Auth file: ~/.model-proxy/<providerName>_apikey.json
type ApiKeyBase struct {
	authFile string // expanded path

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
func (b *ApiKeyBase) Refresh() error {
	b.mu.Lock()
	b.cached = ""
	b.mu.Unlock()
	return nil
}

// LoadKey reads the API key from the auth file (with in-memory cache).
func (b *ApiKeyBase) LoadKey() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.cached != "" {
		return b.cached, nil
	}
	data, err := os.ReadFile(b.authFile)
	if err != nil {
		return "", fmt.Errorf("not logged in; run `model-proxy login` for this provider")
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

// SaveKey writes the API key to the auth file (0600, parent dir 0700).
func (b *ApiKeyBase) SaveKey(key string) error {
	if err := os.MkdirAll(filepath.Dir(b.authFile), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(map[string]string{"api_key": key}, "", "  ")
	return os.WriteFile(b.authFile, data, 0o600)
}

// DeleteKey removes the auth file (logout).
func (b *ApiKeyBase) DeleteKey() error {
	err := os.Remove(b.authFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// AuthFilePath returns the auth file path (for logging/debugging).
func (b *ApiKeyBase) AuthFilePath() string {
	return b.authFile
}
