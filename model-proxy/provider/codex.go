package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
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
func (p *CodexProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *CodexProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *CodexProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *CodexProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
func (p *CodexProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

// FetchModels queries the codex backend's /models endpoint and returns the
// slugs with visibility "list". The endpoint is gated on a client_version
// query param (the backend uses it to decide which models to expose — a stale
// version hides newer models) and returns a custom {"models":[{slug,visibility,
// ...}]} shape rather than OpenAI's {data:[]}, so it cannot reuse
// fetchModelsBearer. client_version is resolved by the main package (config >
// codex CLI > ~/.codex cache > baked constant) and passed in via cfg.ClientVersion.
func (p *CodexProvider) FetchModels() ([]string, error) {
	url := strings.TrimRight(p.cfg.OpenAIBaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	q.Set("client_version", p.cfg.ClientVersion)
	req.URL.RawQuery = q.Encode()
	if err := p.cfg.Auth.Inject(req); err != nil {
		return nil, fmt.Errorf("codex models auth: %w", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch codex models: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("fetch codex models: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	var v struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("parse codex models: %w", err)
	}
	ids := make([]string, 0, len(v.Models))
	for _, m := range v.Models {
		if m.Visibility == "list" {
			ids = append(ids, m.Slug)
		}
	}
	return ids, nil
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
