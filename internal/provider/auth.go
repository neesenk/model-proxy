package provider

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"model-proxy/internal/display"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"model-proxy/internal/credstore"
	"model-proxy/internal/upstreamproxy"
)

// auth.go holds the provider-owned auth injectors (aqp key minting, codex OAuth,
// SSO cookie read, JWT helpers), moved from main's auth.go in Phase 4. Each
// provider struct owns its auth injector (the auth field) and AuthHeaders/Refresh
// delegate to it directly (no cfg.Auth callback).

// refreshFailureTTL is how long a failed auth refresh (codex token refresh,
// aqp key mint) is cached as the immediate answer before another network
// attempt is allowed. Both providers refresh while holding their mutex, so
// without this negative cache a down auth endpoint makes EVERY request queue
// on the lock for a fresh 30s network round trip — the whole provider
// collapses while auth is erroring. The TTL is deliberately short: once the
// auth endpoint recovers, the provider self-heals within seconds, and
// correctness never depends on the cache — forward's failover covers the
// provider for as long as its auth keeps erroring.
const refreshFailureTTL = 15 * time.Second

// authInjector is the internal contract a provider's auth field satisfies. The
// concrete types (AqpKeyProvider, CodexOAuthProvider) implement it; tests inject
// fakes (fakeAuth/errAuth). The constructor wires the real injector.
type authInjector interface {
	Inject(req *http.Request) error
	Refresh() error
}

// SecretReporter is the opt-in interface for providers (or their auth
// injectors) that cache credential values in memory which the on-disk guard
// secret collection cannot see: aqp's managed API key is minted at runtime and
// lives only in AqpKeyProvider's cache, and codex's in-memory access_token can
// be fresher than the auth file between refreshes. The returned values feed
// ONLY the in-process guard known-secret scanner (internal/app merges them on
// the refresh beat): they must never be serialized, logged, persisted, or
// exposed through any DTO/Web API. This is the first interface-level exposure
// of provider credential values — the exception rationale is documented in
// docs/decisions/intentional-behaviors.md item 15.
type SecretReporter interface {
	ReportSecrets() []string
}

// removeAuthFile deletes a credential/auth store via credstore (treating
// "absent" as success — idempotent logout). Used by aqp/codex Logout.
func removeAuthFile(path string) error {
	return credstore.NewRef(path).Delete()
}

// ---- AQP key provider ----

// AqpKeyProvider mints an AQP API key from the persisted SSO cookie (the
// compass /api_key/get_or_generate endpoint), caching it for 50 minutes. The
// minted key exists only in this memory cache — no file ever stores it — so it
// is additionally surfaced through SecretReporter for the in-process guard
// known-secret scanner (memory only; see the interface's contract).
type AqpKeyProvider struct {
	mintURL  string
	authFile string

	mu        sync.Mutex
	cached    string
	projectID string
	mintedAt  time.Time

	// Negative cache for failed mints (refreshFailureTTL): while now is before
	// mintFailedUntil, keyLocked returns lastMintErr without touching the
	// network, so a down mint endpoint cannot serialize every request on a
	// fresh 30s timeout under p.mu. A successful mint clears both.
	lastMintErr     error
	mintFailedUntil time.Time
	// now is the clock seam for the failure TTL (tests inject a fake clock;
	// nil = time.Now). Struct-literal construction stays valid via nowTime.
	now func() time.Time
}

// nowTime returns the injected clock, or the wall clock when unset.
func (p *AqpKeyProvider) nowTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// NewAqpKeyProvider builds an AqpKeyProvider from the mint URL + SSO store path.
func NewAqpKeyProvider(mintURL, authFile string) *AqpKeyProvider {
	return &AqpKeyProvider{mintURL: mintURL, authFile: authFile}
}

func (p *AqpKeyProvider) Inject(req *http.Request) error {
	key, err := p.key()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Del("x-api-key")
	return nil
}

func (p *AqpKeyProvider) Refresh() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cached = ""
	p.mintedAt = time.Time{}
	_, err := p.keyLocked()
	return err
}

func (p *AqpKeyProvider) key() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.keyLocked()
}

// ReportSecrets implements SecretReporter: the currently cached minted key, or
// nothing when no key has been minted yet. The minted managed key exists only
// in this in-memory cache (no file ever stores it), so without this hook the
// guard known-secret set could not cover it. Memory only, guard scanning only
// — never serialize or log the returned value.
func (p *AqpKeyProvider) ReportSecrets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == "" {
		return nil
	}
	return []string{p.cached}
}

func (p *AqpKeyProvider) keyLocked() (string, error) {
	// Cache for 50 minutes (AQP keys are generally long-lived; 50m is conservative).
	if p.cached != "" && time.Since(p.mintedAt) < 50*time.Minute {
		return p.cached, nil
	}
	// Negative cache: a mint that failed within refreshFailureTTL is returned
	// as-is — no new network attempt, so requests never queue on p.mu for a
	// fresh round trip against a down endpoint.
	if p.lastMintErr != nil && p.nowTime().Before(p.mintFailedUntil) {
		return "", p.lastMintErr
	}
	key, err := p.mintLocked()
	if err != nil {
		p.lastMintErr, p.mintFailedUntil = err, p.nowTime().Add(refreshFailureTTL)
		return "", err
	}
	p.lastMintErr, p.mintFailedUntil = nil, time.Time{}
	return key, nil
}

// mintLocked performs the network mint (caller holds p.mu); keyLocked
// negative-caches its failures for refreshFailureTTL.
func (p *AqpKeyProvider) mintLocked() (string, error) {
	cookie, err := ReadSSOCookie(p.authFile)
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
	client := &http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("mint aqp key: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		// Truncated like the codex refresh error: the body may be a large
		// error page and must not flood logs/errors.
		return "", fmt.Errorf("mint aqp key: HTTP %d: %s", resp.StatusCode, display.Truncate(string(rb), 200))
	}
	var parsed struct {
		Retcode int `json:"retcode"`
		Data    struct {
			APIKey    string `json:"api_key"`
			ProjectID string `json:"project_id"`
			Email     string `json:"employee_email"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(rb, &parsed); err != nil {
		return "", fmt.Errorf("parse aqp response: %w", err)
	}
	if parsed.Retcode != 0 || parsed.Data.APIKey == "" {
		return "", fmt.Errorf("mint aqp key: retcode=%d msg=%s", parsed.Retcode, parsed.Message)
	}
	p.cached = parsed.Data.APIKey
	p.mintedAt = time.Now()
	p.projectID = parsed.Data.ProjectID
	return p.cached, nil
}

// ReadSSOCookie reads sso_session_cookie from the configured store file.
func ReadSSOCookie(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("sso_cookie_file not set")
	}
	data, err := credstore.NewRef(path).Load()
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

// ---- codex OAuth provider (chatgpt.com backend) ----

const (
	// CodexOAuthTokenURL is the OpenAI token endpoint for codex OAuth refresh.
	CodexOAuthTokenURL = "https://auth.openai.com/oauth/token"
	// CodexOAuthClientID is the codex CLI's OAuth client id.
	CodexOAuthClientID = "app_EMoamEEZ73f0CkXaXp7hrann"
)

// CodexOAuthProvider reads the proxy's OWN codex OAuth tokens (from the
// codex_oauth_auth.json store - obtained via `model-proxy login codex`, NOT
// shared with codex CLI's ~/.codex/auth.json) and injects the access_token as
// Bearer for the chatgpt.com/backend-api/codex backend. On 401 it refreshes via
// refresh_token and writes new tokens back.
type CodexOAuthProvider struct {
	authFile string
	tokenURL string // override for tests; defaults to CodexOAuthTokenURL

	mu        sync.Mutex
	cached    string    // access_token
	exp       time.Time // access_token expiry (parsed from JWT)
	accountID string    // chatgpt account_id (parsed from id_token JWT)

	// Negative cache for failed refreshes (refreshFailureTTL): while now is
	// before refreshFailedUntil, the refresh path returns lastRefreshErr
	// without touching the network, so a down auth endpoint cannot serialize
	// every request on a fresh 30s timeout under p.mu. A successful refresh
	// clears both.
	lastRefreshErr     error
	refreshFailedUntil time.Time
	// now is the clock seam for the failure TTL (tests inject a fake clock;
	// nil = time.Now). Struct-literal construction stays valid via nowTime.
	now func() time.Time
}

// nowTime returns the injected clock, or the wall clock when unset.
func (p *CodexOAuthProvider) nowTime() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// NewCodexOAuthProvider builds a CodexOAuthProvider reading the given auth file.
func NewCodexOAuthProvider(authFile string) *CodexOAuthProvider {
	return &CodexOAuthProvider{authFile: authFile, tokenURL: CodexOAuthTokenURL}
}

// codexCred derives the credential store ref from authFile at call time so
// struct-literal construction (tests, ad-hoc instances) stays correct.
func (p *CodexOAuthProvider) codexCred() credstore.Ref { return credstore.NewRef(p.authFile) }

// CodexAuthFile is the on-disk format of codex_oauth_auth.json.
type CodexAuthFile struct {
	AuthMode string `json:"auth_mode"`
	Tokens   struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		AccountID    string `json:"account_id"`
	} `json:"tokens"`
	LastRefresh string `json:"last_refresh"`
}

func (p *CodexOAuthProvider) Inject(req *http.Request) error {
	tok, acct, err := p.token()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Del("x-api-key")
	// codex backend requires these headers (matches codex CLI's originator +
	// account scoping). Without originator the backend returns 403.
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	return nil
}

// Token returns a valid access_token and the chatgpt account_id, refreshing if
// cached is missing/expired.
func (p *CodexOAuthProvider) Token() (string, string, error) {
	return p.token()
}

// ReportSecrets implements SecretReporter: the in-memory cached access_token,
// or nothing when no token has been loaded/refreshed yet. The on-disk OAuth
// collection already covers the file's tokens, but this cache is fresher than
// the file in the window between an in-process refresh landing and the next
// file re-collection. Memory only, guard scanning only — never serialize or
// log the returned value.
func (p *CodexOAuthProvider) ReportSecrets() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cached == "" {
		return nil
	}
	return []string{p.cached}
}

func (p *CodexOAuthProvider) token() (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Use cached if present and not near expiry (5 min skew).
	if p.cached != "" && (p.exp.IsZero() || time.Until(p.exp) > 5*time.Minute) {
		return p.cached, p.accountID, nil
	}
	af, err := p.load()
	if err != nil {
		return "", "", fmt.Errorf("read codex auth: %w", err)
	}
	exp := JwtExpiry(af.Tokens.AccessToken)
	acct := AccountIDFromTokens(af.Tokens.IDToken, af.Tokens.AccountID)
	if af.Tokens.AccessToken != "" && (exp.IsZero() || time.Until(exp) > 5*time.Minute) {
		p.cached, p.exp, p.accountID = af.Tokens.AccessToken, exp, acct
		return p.cached, p.accountID, nil
	}
	if af.Tokens.RefreshToken == "" {
		return "", "", fmt.Errorf("codex auth has no refresh_token; run `model-proxy login codex`")
	}
	if err := p.refreshGuardedLocked(af); err != nil {
		return "", "", err
	}
	return p.cached, p.accountID, nil
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
		return fmt.Errorf("codex auth has no refresh_token; run `model-proxy login codex`")
	}
	return p.refreshGuardedLocked(af)
}

// refreshGuardedLocked wraps refreshLocked with the failure negative cache
// (caller holds p.mu): inside the refreshFailureTTL window the cached error is
// returned without a network attempt; a success clears the cache so the next
// failure starts a fresh window.
func (p *CodexOAuthProvider) refreshGuardedLocked(af *CodexAuthFile) error {
	if p.lastRefreshErr != nil && p.nowTime().Before(p.refreshFailedUntil) {
		return p.lastRefreshErr
	}
	if err := p.refreshLocked(af); err != nil {
		p.lastRefreshErr, p.refreshFailedUntil = err, p.nowTime().Add(refreshFailureTTL)
		return err
	}
	p.lastRefreshErr, p.refreshFailedUntil = nil, time.Time{}
	return nil
}

// AccountIDFromTokens returns the chatgpt account_id: prefer the stored field,
// else parse it from the id_token JWT's https://api.openai.com/auth.chatgpt_account_id claim.
func AccountIDFromTokens(idToken, stored string) string {
	if stored != "" {
		return stored
	}
	b := decodeJWTPayload(idToken)
	if b == nil {
		return ""
	}
	var c struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return ""
	}
	return c.Auth.AccountID
}

// emailFromIDToken parses the `email` claim from an id_token JWT (best-effort,
// no validation). The codex device flow requests the `openid profile email`
// scope, so the id_token carries the user's email - used as the account label
// in the Web UI (codex does not persist email at the top level like aqp does).
func emailFromIDToken(idToken string) string {
	b := decodeJWTPayload(idToken)
	if b == nil {
		return ""
	}
	var c struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return ""
	}
	return c.Email
}

func (p *CodexOAuthProvider) load() (*CodexAuthFile, error) {
	data, err := p.codexCred().Load()
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) {
			// Preserve the historical os.ReadFile-style sentinel so callers
			// can still distinguish "never logged in" from parse errors.
			return nil, &fsNotFoundError{path: p.authFile}
		}
		return nil, err
	}
	var af CodexAuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p.authFile, err)
	}
	return &af, nil
}

// LoadCodexAuthFile reads and parses one codex OAuth store through
// credstore, without instantiating a provider. Callers that collect
// credential material for the guard known-secret set use this so
// keychain-mode credentials reach them too (the plaintext file is archived
// as .migrated.bak there — a direct os.ReadFile would see nothing).
// credstore.ErrNotFound means "not logged in".
func LoadCodexAuthFile(path string) (*CodexAuthFile, error) {
	data, err := credstore.NewRef(path).Load()
	if err != nil {
		return nil, err
	}
	var af CodexAuthFile
	if err := json.Unmarshal(data, &af); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &af, nil
}

// refreshLocked exchanges refresh_token for a new access_token and writes it
// back to auth.json. Caller holds p.mu.
func (p *CodexOAuthProvider) refreshLocked(af *CodexAuthFile) error {
	body := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {af.Tokens.RefreshToken},
		"client_id":     {CodexOAuthClientID},
		"scope":         {"openid profile email"},
	}.Encode()
	u := p.tokenURL
	if u == "" {
		u = CodexOAuthTokenURL
	}
	req, _ := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := (&http.Client{Timeout: 30 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		return fmt.Errorf("codex oauth refresh: %w", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("codex oauth refresh: HTTP %d: %s", resp.StatusCode, display.Truncate(string(rb), 200))
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
	af.Tokens.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		af.Tokens.RefreshToken = tok.RefreshToken
	}
	if tok.IDToken != "" {
		af.Tokens.IDToken = tok.IDToken
	}
	af.LastRefresh = time.Now().UTC().Format(time.RFC3339Nano)
	if err := p.save(af); err != nil {
		return fmt.Errorf("write codex auth: %w", err)
	}
	p.cached, p.exp = tok.AccessToken, JwtExpiry(tok.AccessToken)
	p.accountID = AccountIDFromTokens(af.Tokens.IDToken, af.Tokens.AccountID)
	return nil
}

// fsNotFoundError mimics os.IsNotExist semantics over credstore.ErrNotFound
// for callers that inspect the error with os.IsNotExist.
type fsNotFoundError struct{ path string }

func (e *fsNotFoundError) Error() string {
	return "open " + e.path + ": no such credential store entry"
}

func (e *fsNotFoundError) Is(target error) bool { return target == os.ErrNotExist }

// WriteCodexAuthFile atomically persists the codex OAuth store through
// credstore: the CLI's initial device-flow login and the provider's
// token-rotation refresh both funnel through here so a crash mid-write can
// never truncate the only copy of a rotated refresh token.
func WriteCodexAuthFile(path string, af *CodexAuthFile) error {
	b, err := json.MarshalIndent(af, "", "  ")
	if err != nil {
		return err
	}
	return credstore.NewRef(path).Save(b)
}

// ClearCodexAccount removes the codex OAuth store through credstore. This is
// the shared logout/delete boundary for file and keychain modes; absence is
// success so CLI and Web account deletion remain idempotent.
func ClearCodexAccount(path string) error {
	return removeAuthFile(path)
}

func (p *CodexOAuthProvider) save(af *CodexAuthFile) error {
	return WriteCodexAuthFile(p.authFile, af)
}

// CodexAccountInfo is the Web-UI display projection of a codex OAuth auth
// file: the chatgpt account_id (the identifier the UI sends back on remove)
// and the email parsed from the id_token JWT (codex's account label). Unlike
// aqp, codex does not persist email or a created_at timestamp at the top
// level - the email lives in the id_token's `email` claim, and there is no
// added-at timestamp (only last_refresh, a different semantic, which is not
// surfaced here). Only these two fields are projected; the access/refresh/id
// tokens never leave the provider package in serializable or loggable form.
// The two explicit exceptions, both memory-only and never logged, persisted,
// or serialized: internal/app reads the raw token values once per generation
// (build) into the guard known-secret scanner, and SecretReporter exposes the
// in-memory cached access_token to the same scanner on the refresh beat.
type CodexAccountInfo struct {
	AccountID string
	Email     string
}

// LoadCodexAccount reads the codex OAuth auth file (<name>_oauth_auth.json) and
// projects it to CodexAccountInfo for the Web UI account list. Returns nil, nil
// if the file is absent. The account_id is tokens.account_id (stored) else
// parsed from the id_token JWT; the email is parsed from the id_token JWT.
func LoadCodexAccount(path string) (*CodexAccountInfo, error) {
	b, err := credstore.NewRef(path).Load()
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var af CodexAuthFile
	if err := json.Unmarshal(b, &af); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &CodexAccountInfo{
		AccountID: AccountIDFromTokens(af.Tokens.IDToken, af.Tokens.AccountID),
		Email:     emailFromIDToken(af.Tokens.IDToken),
	}, nil
}

// decodeJWTPayload base64-decodes the middle segment of a JWT (no validation).
// Returns nil on any error (malformed, missing segment, bad base64). Shared by
// JwtExpiry, AccountIDFromTokens, and emailFromIDToken so the payload-decode
// logic isn't triplicated.
func decodeJWTPayload(jwt string) []byte {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return nil
	}
	payload := parts[1]
	if pad := len(payload) % 4; pad != 0 {
		payload += strings.Repeat("=", 4-pad)
	}
	b, err := base64.URLEncoding.DecodeString(payload)
	if err != nil {
		return nil
	}
	return b
}

// JwtExpiry extracts the `exp` claim from a JWT without validating it.
// Returns zero time on any error (treated as "unknown expiry" -> use token).
func JwtExpiry(jwt string) time.Time {
	b := decodeJWTPayload(jwt)
	if b == nil {
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
