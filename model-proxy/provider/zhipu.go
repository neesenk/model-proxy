package provider

import (
	"fmt"
	"net/http"
	"os"
)

// ZhipuProvider implements the Zhipu BigModel provider using an API key
// stored in ~/.model-proxy/<providerName>_apikey.json.
type ZhipuProvider struct {
	*ApiKeyBase
	cfg          *Config
	providerName string
}

func init() {
	Register("zhipu", func(cfg *Config, providerName string) (Provider, error) {
		return &ZhipuProvider{
			ApiKeyBase:   NewApiKeyBase(providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

func (p *ZhipuProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body // no special rewriting
}

func (p *ZhipuProvider) Login() error {
	// Read API key from stdin.
	fmt.Printf("Enter API key for %s: ", p.providerName)
	var key string
	fmt.Scanln(&key)
	if key == "" {
		return fmt.Errorf("empty API key")
	}

	// Validate via /models endpoint.
	if p.cfg.UsageURL != "" {
		fmt.Fprintln(os.Stderr, "Validating API key...")
		req, _ := http.NewRequest("GET", p.cfg.UsageURL, nil)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := (&http.Client{Timeout: 15e9}).Do(req)
		if err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("validation failed: HTTP %d", resp.StatusCode)
		}
	}

	return p.SaveKey(key)
}

func (p *ZhipuProvider) Logout() error {
	return p.DeleteKey()
}

func (p *ZhipuProvider) Usage() (any, error) {
	// Delegate to the main-package UsageFn (showGenericUsage), which fetches
	// /models and prints the list. Matches compass/codex/deepseek.
	return p.cfg.UsageFn()
}

func (p *ZhipuProvider) FetchModels() ([]string, error) { return fetchModelsBearer(p.cfg) }
