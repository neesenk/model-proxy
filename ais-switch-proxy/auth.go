package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// AuthProvider injects auth headers per-route auth strategy and supports 401 refresh retry.
type AuthProvider interface {
	// Inject adds auth headers to the upstream request, clearing the client's placeholder token.
	Inject(req *http.Request) error
	// Refresh re-mints / re-reads credentials (called on 401).
	Refresh() error
}

// ---- CQP key provider ----

type CQPProvider struct {
	mintURL      string
	ssoCookieFile string
	staticKey    string

	mu       sync.Mutex
	cached   string
	projectID string
	mintedAt time.Time
}

func newCQPProvider(cfg AuthCfg) *CQPProvider {
	return &CQPProvider{
		mintURL:       cfg.CQPMintURL,
		ssoCookieFile: cfg.SSOCookieFile,
		staticKey:     cfg.StaticKey,
	}
}

func (p *CQPProvider) Inject(req *http.Request) error {
	key, err := p.key()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	// Clear the client's placeholder token
	req.Header.Del("x-api-key")
	return nil
}

func (p *CQPProvider) Refresh() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = ""
	p.mintedAt = time.Time{}
	_, err := p.keyLocked()
	return err
}

func (p *CQPProvider) key() (string, error) {
	if p.staticKey != "" {
		return p.staticKey, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keyLocked()
}

func (p *CQPProvider) keyLocked() (string, error) {
	// Cache for 50 minutes (CQP keys are generally long-lived; 50m is conservative).
	if p.cached != "" && time.Since(p.mintedAt) < 50*time.Minute {
		return p.cached, nil
	}
	cookie, err := readSSOCookie(p.ssoCookieFile)
	if err != nil {
		return "", fmt.Errorf("read sso cookie: %w", err)
	}
	body := []byte(`{}`)
	req, err := http.NewRequest(http.MethodPost, p.mintURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("content-type", "application/json")
	// sso_session_cookie value already includes the SSO_C= prefix; use it as the whole Cookie header.
	req.Header.Set("Cookie", cookie)
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint cqp key: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("mint cqp key: HTTP %d: %s", resp.StatusCode, string(rb))
	}
	var parsed struct {
		Retcode int    `json:"retcode"`
		Data    struct {
			APIKey    string `json:"api_key"`
			ProjectID string `json:"project_id"`
			Email     string `json:"employee_email"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return "", fmt.Errorf("parse cqp response: %w", err)
	}
	if parsed.Retcode != 0 || parsed.Data.APIKey == "" {
		return "", fmt.Errorf("mint cqp key: retcode=%d msg=%s", parsed.Retcode, parsed.Message)
	}
	p.cached = parsed.Data.APIKey
	p.mintedAt = time.Now()
	p.projectID = parsed.Data.ProjectID
	return p.cached, nil
}

// MintKeyWithMeta mints a CQP key and returns key + project_id (used by login).
func (p *CQPProvider) MintKeyWithMeta() (key, projectID string, err error) {
	if err := p.Refresh(); err != nil {
		return "", "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cached, p.projectID, nil
}

// readSSOCookie reads sso_session_cookie from AIS Switch's google_oauth_auth.json.
func readSSOCookie(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("sso_cookie_file not set")
	}
	data, err := readFile(path)
	if err != nil {
		return "", err
	}
	var v struct {
		SSOSessionCookie string `json:"sso_session_cookie"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	if v.SSOSessionCookie == "" {
		return "", fmt.Errorf("sso_session_cookie empty in %s", path)
	}
	return v.SSOSessionCookie, nil
}

// ---- static key provider ----

type StaticProvider struct{ key string }

func (s *StaticProvider) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+s.key)
	req.Header.Del("x-api-key")
	return nil
}
func (s *StaticProvider) Refresh() error { return nil }

// ---- gemini key provider ----

type GeminiProvider struct{ envVar string; cached string }

func (g *GeminiProvider) Inject(req *http.Request) error {
	if g.cached == "" {
		g.cached = envOrEmpty(g.envVar)
	}
	if g.cached == "" {
		return fmt.Errorf("gemini api key env %s not set", g.envVar)
	}
	req.Header.Set("x-goog-api-key", g.cached)
	req.Header.Del("Authorization")
	return nil
}
func (g *GeminiProvider) Refresh() error { g.cached = ""; return nil }

// ---- factory ----

func newAuthProvider(route *Route, cfg *Config) AuthProvider {
	switch route.Auth {
	case "cqp":
		return newCQPProvider(cfg.Auth)
	case "static":
		return &StaticProvider{key: cfg.Auth.StaticKey}
	case "gemini_key":
		env := cfg.Auth.GeminiAPIKeyEnv
		if env == "" {
			env = "GEMINI_API_KEY"
		}
		return &GeminiProvider{envVar: env}
	default:
		return &StaticProvider{key: ""} // none
	}
}
