package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
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

// ---- codex OAuth provider (chatgpt.com backend) ----

const (
	codexOAuthTokenURL = "https://auth.openai.com/oauth/token"
	codexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// CodexOAuthProvider reads ~/.codex/auth.json's access_token and injects it as
// Bearer for the chatgpt.com/backend-api/codex backend. On 401 it refreshes via
// refresh_token and writes the new tokens back to auth.json (shared with codex CLI).
type CodexOAuthProvider struct {
	authFile  string
	tokenURL  string // override for tests; defaults to codexOAuthTokenURL

	mu     sync.Mutex
	cached string    // access_token
	exp    time.Time // access_token expiry (parsed from JWT)
}

func newCodexOAuthProvider(authFile string) *CodexOAuthProvider {
	return &CodexOAuthProvider{authFile: authFile, tokenURL: codexOAuthTokenURL}
}

// codexAuthFile is the on-disk format of ~/.codex/auth.json.
type codexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

func (p *CodexOAuthProvider) Inject(req *http.Request) error {
	tok, err := p.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Del("x-api-key")
	return nil
}

// token returns a valid access_token, refreshing if cached is missing/expired.
func (p *CodexOAuthProvider) token() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Use cached if present and not near expiry (5 min skew).
	if p.cached != "" && (p.exp.IsZero() || time.Until(p.exp) > 5*time.Minute) {
		return p.cached, nil
	}
	// Load from file (codex CLI may have refreshed it independently).
	af, err := p.load()
	if err != nil {
		return "", fmt.Errorf("read codex auth: %w", err)
	}
	exp := jwtExpiry(af.Tokens.AccessToken)
	// If file token still valid, cache and use it.
	if af.Tokens.AccessToken != "" && (exp.IsZero() || time.Until(exp) > 5*time.Minute) {
		p.cached, p.exp = af.Tokens.AccessToken, exp
		return p.cached, nil
	}
	// Need refresh.
	if af.Tokens.RefreshToken == "" {
		return "", fmt.Errorf("codex auth has no refresh_token; run `codex login`")
	}
	if err := p.refreshLocked(af); err != nil {
		return "", err
	}
	return p.cached, nil
}

// Refresh forces a token refresh (called on upstream 401).
func (p *CodexOAuthProvider) Refresh() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	af, err := p.load()
	if err != nil {
		return fmt.Errorf("read codex auth: %w", err)
	}
	if af.Tokens.RefreshToken == "" {
		return fmt.Errorf("codex auth has no refresh_token; run `codex login`")
	}
	return p.refreshLocked(af)
}

func (p *CodexOAuthProvider) load() (*codexAuthFile, error) {
	data, err := readFile(p.authFile)
	if err != nil {
		return nil, err
	}
	var af codexAuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.authFile, err)
	}
	return &af, nil
}

// refreshLocked exchanges refresh_token for a new access_token and writes it
// back to auth.json. Caller holds p.mu.
func (p *CodexOAuthProvider) refreshLocked(af *codexAuthFile) error {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {af.Tokens.RefreshToken},
		"client_id":     {codexOAuthClientID},
		"scope":         {"openid profile email"},
	}.Encode()
	url := p.tokenURL
	if url == "" {
		url = codexOAuthTokenURL
	}
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("codex oauth refresh: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("codex oauth refresh: HTTP %d: %s", resp.StatusCode, truncate(string(rb), 200))
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(rb, &tok); err != nil {
		return fmt.Errorf("parse codex oauth response: %w", err)
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("codex oauth response missing access_token")
	}
	// Update the auth file (rotate refresh_token if a new one was returned).
	af.Tokens.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		af.Tokens.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		// Preserve other fields; only rewrite tokens we track. Re-marshal full file.
	}
	af.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.save(af); err != nil {
		return fmt.Errorf("write codex auth: %w", err)
	}
	p.cached, p.exp = tok.AccessToken, jwtExpiry(tok.AccessToken)
	return nil
}

func (p *CodexOAuthProvider) save(af *codexAuthFile) error {
	b, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p.authFile, b, 0o600)
}

// jwtExpiry extracts the `exp` claim from a JWT without validating it.
// Returns zero time on any error (treated as "unknown expiry" → use token).
func jwtExpiry(jwt string) time.Time {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return time.Time{}
	}
	payload := parts[1]
	// base64url pad to length 4.
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	b, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return time.Time{}
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return time.Time{}
	}
	if c.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(c.Exp, 0)
}

// ---- factory ----

// newAuthProvider builds an AuthProvider for a given auth strategy name.
// Used both for top-level route auth and per-model model_routing entries.
func newAuthProvider(authName string, cfg *Config) AuthProvider {
	switch authName {
	case "cqp":
		return newCQPProvider(cfg.Auth)
	case "codex_oauth":
		f := cfg.Auth.CodexAuthFile
		if f == "" {
			f = "~/.codex/auth.json"
		}
		return newCodexOAuthProvider(expandPath(f))
	case "static":
		return &StaticProvider{key: cfg.Auth.StaticKey}
	default:
		return &StaticProvider{key: ""} // none
	}
}
