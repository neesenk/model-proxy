package provider

import (
	"encoding/json"
	"net/http"
)

// CodexProvider wraps the main package's codex OAuth + device flow + wham/usage.
type CodexProvider struct {
	cfg *Config
}

func init() {
	Register("codex", func(cfg *Config, providerName string) (Provider, error) {
		return &CodexProvider{cfg: cfg}, nil
	})
}

func (p *CodexProvider) AuthHeaders(req *http.Request) error {
	return p.cfg.Auth.Inject(req)
}
func (p *CodexProvider) Refresh() error {
	return p.cfg.Auth.Refresh()
}
func (p *CodexProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	// codex backend requires store:false in the request body.
	body = ensureJSONField(body, "store", false)
	return targetURL, body
}
func (p *CodexProvider) Login() error        { return p.cfg.LoginFn() }
func (p *CodexProvider) Logout() error       { return p.cfg.LogoutFn() }
func (p *CodexProvider) Usage() (any, error) { return p.cfg.UsageFn() }
func (p *CodexProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }

// FetchModels returns the codex backend's known models. The codex backend's /models
// endpoint requires a client_version query param and is not a standard OpenAI-style
// list, so we hardcode the single model the backend serves.
func (p *CodexProvider) FetchModels() ([]string, error) {
	return []string{"gpt-5.5"}, nil
}

// ensureJSONField sets body[key] = val if the key is absent.
func ensureJSONField(body []byte, key string, val any) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	if _, ok := v[key]; !ok {
		v[key] = val
		out, err := json.Marshal(v)
		if err != nil {
			return body
		}
		return out
	}
	return body
}
