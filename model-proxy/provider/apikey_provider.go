package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// ApiKeyProvider is a generic API key provider for providers that don't have
// a specialized implementation. Uses usageURL for validation + usage.
type ApiKeyProvider struct {
	*ApiKeyBase
	cfg          *Config
	providerName string
}

func init() {
	Register("apikey", func(cfg *Config, providerName string) (Provider, error) {
		return &ApiKeyProvider{
			ApiKeyBase:   NewApiKeyBase(providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

func (p *ApiKeyProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *ApiKeyProvider) Login() error {
	// Delegate to a callback set by main package (for stdin reading + validation).
	// If no callback, use the generic flow.
	return p.genericLogin()
}

func (p *ApiKeyProvider) genericLogin() error {
	// Main package's runApiKeyLogin handles this; provider just needs the key saved.
	// This is a fallback if called directly.
	return fmt.Errorf("login not wired; use main package's runApiKeyLogin")
}

func (p *ApiKeyProvider) Logout() error {
	return p.DeleteKey()
}

func (p *ApiKeyProvider) Usage() (any, error) {
	key, err := p.LoadKey()
	if err != nil {
		return nil, err
	}
	url := p.cfg.UsageURL
	if url == "" {
		return nil, fmt.Errorf("no usageURL configured for %s", p.providerName)
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30e9}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result any
	json.NewDecoder(resp.Body).Decode(&result)
	return result, nil
}

// Need imports
var _ = fmt.Sprintf
