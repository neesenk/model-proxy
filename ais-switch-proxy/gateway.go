package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// This file is ported from ais-switch-cli/internal/gateway (client.go + store.go + consts.go).
// Core of Compass SSO: a single cookie jar carried across the whole login flow. Both the
// SSO_A bootstrap cookie and the SSO_C session cookie (set by the auth/info poll's 200)
// are retained in the jar.

// ---- Endpoints / constants (match ais-switch-cli/internal/consts) ----

const (
	compassBase      = "https://compass.llm.shopee.io"
	compassAuthLogin = compassBase + "/compass-api/v1/auth/login" // bootstrap: 401 + SSO_A + result URL
	compassAuthInfo  = compassBase + "/compass-api/v1/auth/info"  // session poll: 200 + SSO_C when authed
	compassAPIKeyGetGen = compassBase + "/api/v1/cqp/ccswitch/api_key/get_or_generate"
	compassMonthlyUsage = compassBase + "/api/v1/cqp/ccswitch/monthly_usage"

	ssoCookieName      = "SSO_C"                       // actual cookie name (verified against the real store)
	loginCompletePath  = "/company-gateway/login-complete"
	googleOAuthAuthFile = "google_oauth_auth.json"
)

// ---- Account persistence (google_oauth_auth.json, same format as AIS Switch desktop v0.1.8) ----

// AccountData mirrors google_oauth_auth.json. 6 fields; the managed CQP key is NOT persisted
// (fetched on demand, cached in memory only).
type AccountData struct {
	AccountID        string `json:"account_id"`
	CreatedAt        int64  `json:"created_at"`
	Email            string `json:"email"`
	LastRefreshAt    int64  `json:"last_refresh_at"`
	ProjectID        string `json:"project_id"`
	SSOSessionCookie string `json:"sso_session_cookie"` // full "SSO_C=<value>" or raw value
}

// loadAccount reads account data; returns nil, nil if the file is absent.
func loadAccount(path string) (*AccountData, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var a AccountData
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("parse %s: %w", googleOAuthAuthFile, err)
	}
	return &a, nil
}

// saveAccount writes account data (0600, parent dir 0700), filling in timestamps.
func saveAccount(path string, a *AccountData) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.LastRefreshAt == 0 {
		a.LastRefreshAt = time.Now().Unix()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// clearAccount removes the account file (logout).
func clearAccount(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---- Compass client (with cookie jar) ----

// CompassClient talks to the Compass backend. It keeps a cookie jar across the SSO
// flow so the SSO_A bootstrap cookie and the SSO_C session cookie (set by the
// auth/info poll's 200 after login) are retained.
type CompassClient struct {
	HTTP  *http.Client
	Jar   http.CookieJar
	storePath string

	mu        sync.Mutex
	cachedKey string
}

// newCompassClient builds a Compass client backed by the given store file.
func newCompassClient(storePath string) *CompassClient {
	jar, _ := cookiejar.New(nil)
	return &CompassClient{
		HTTP:      &http.Client{Timeout: 30 * time.Second, Jar: jar},
		Jar:       jar,
		storePath: storePath,
	}
}

// GetManagedKey returns the cached managed key; fetches via get_or_generate if none is cached.
func (c *CompassClient) GetManagedKey() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cachedKey != "" {
		return c.cachedKey, nil
	}
	key, err := c.fetchAPIKey()
	if err != nil {
		return "", err
	}
	c.cachedKey = key.APIKey
	return c.cachedKey, nil
}

// InvalidateKey clears the in-memory managed key (called before a 401 retry).
func (c *CompassClient) InvalidateKey() {
	c.mu.Lock()
	c.cachedKey = ""
	c.mu.Unlock()
}

// cookieHeader builds a Cookie header value from the stored sso_session_cookie.
// The stored value may be the full "SSO_C=<value>" pair or just the raw value.
func cookieHeader(stored string) string {
	if stored == "" {
		return ""
	}
	if strings.Contains(stored, "=") {
		return stored
	}
	return fmt.Sprintf("%s=%s", ssoCookieName, stored)
}

// AuthInfoResponse mirrors compass-api/v1/auth/info.
type AuthInfoResponse struct {
	Retcode int             `json:"retcode"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// AuthInfoData is the data payload of auth/info. The real response nests a `user`
// object (email, userid, is_active). project_id/employee_* come from
// get_or_generate, not auth/info.
type AuthInfoData struct {
	User struct {
		UserID int    `json:"userid"`
		Email  string `json:"email"`
		Active bool   `json:"is_active"`
	} `json:"user"`
	HasAccess        bool
	EmployeeEmail    string
	EmployeeUserID   string
	ProjectID        string // filled later by get_or_generate
	SSOSessionCookie string
}

// BootstrapLoginURL hits auth/login expecting 401, extracts the login URL from
// the `result` field, and retains the SSO_A cookie in the jar.
func (c *CompassClient) BootstrapLoginURL() (string, error) {
	return c.bootstrapAt(compassAuthLogin)
}

// bootstrapAt is the URL-parametrized core, used by tests with a mock server.
func (c *CompassClient) bootstrapAt(endpoint string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("compass sso bootstrap: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// The bootstrap returns 401 with a `result` field carrying the soup.shopee.io
	// login URL (and sets the SSO_A cookie, retained by the jar).
	loginURL := extractLoginURL(string(body))
	if loginURL == "" {
		return "", fmt.Errorf("compass sso bootstrap missing login URL: status=%d, body=%s",
			resp.StatusCode, truncate(string(body), 300))
	}
	for _, ck := range c.PublicCookies() {
		logf("[GoogleGateway] bootstrap jar cookie: %s=%s", ck.Name, ck.Value)
	}
	return loginURL, nil
}

// PollSession polls auth/info (using the jar's cookies) until retcode==0 && hasAccess.
// After a successful login the response sets the SSO_C cookie, captured by the jar.
func (c *CompassClient) PollSession(timeout time.Duration) (*AuthInfoData, error) {
	return c.pollAt(compassAuthInfo, timeout)
}

func (c *CompassClient) pollAt(endpoint string, timeout time.Duration) (*AuthInfoData, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		data, err := c.checkSessionAt(endpoint)
		if err == nil && data != nil {
			return data, nil
		}
		lastErr = err
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return nil, fmt.Errorf("compass sso session check failed: %w", lastErr)
	}
	return nil, fmt.Errorf("compass sso session check timed out")
}

func (c *CompassClient) checkSessionAt(endpoint string) (*AuthInfoData, error) {
	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Accept", "application/json")
	if c.Jar != nil {
		u, _ := url.Parse(endpoint)
		var names []string
		for _, ck := range c.Jar.Cookies(u) {
			names = append(names, ck.Name+"="+ck.Value)
		}
		logf("[GoogleGateway] auth/info jar cookies: %s", strings.Join(names, "; "))
	}
	resp, err := c.HTTP.Do(req) // jar sends SSO_A/SSO_C automatically
	if err != nil {
		return nil, fmt.Errorf("compass sso session check failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	logf("[GoogleGateway] auth/info status=%d body=%s", resp.StatusCode, truncate(string(body), 150))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("compass sso session check failed: status=%d body=%s",
			resp.StatusCode, truncate(string(body), 200))
	}
	var air AuthInfoResponse
	if err := json.Unmarshal(body, &air); err != nil {
		return nil, fmt.Errorf("compass auth info response parse failed: %w", err)
	}
	if air.Retcode != 0 {
		return nil, fmt.Errorf("compass sso session rejected: retcode=%d message=%s", air.Retcode, air.Message)
	}
	var d AuthInfoData
	if err := json.Unmarshal(air.Data, &d); err != nil {
		return nil, fmt.Errorf("compass auth info data parse failed: %w", err)
	}
	d.HasAccess = d.User.Active
	d.EmployeeEmail = d.User.Email
	d.EmployeeUserID = fmt.Sprintf("%d", d.User.UserID)
	if !d.HasAccess {
		return nil, fmt.Errorf("google account is not allowed for this gateway")
	}
	return &d, nil
}

// SessionCookie returns the SSO_C cookie value from the jar (set by auth/info
// after a successful login), or "" if absent.
func (c *CompassClient) SessionCookie() string {
	if c.Jar == nil {
		return ""
	}
	u, _ := url.Parse(compassBase)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == ssoCookieName {
			return fmt.Sprintf("%s=%s", ck.Name, ck.Value)
		}
	}
	return ""
}

// PublicCookies returns the jar's cookies for the compass host (for debugging).
func (c *CompassClient) PublicCookies() []*http.Cookie {
	if c.Jar == nil {
		return nil
	}
	u, _ := url.Parse(compassBase)
	return c.Jar.Cookies(u)
}

// APIKeyResponse mirrors api_key/get_or_generate.
type APIKeyResponse struct {
	Retcode int             `json:"retcode"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// APIKeyData is the data payload of get_or_generate. It returns the full identity
// (api_key + project_id + employee_*) without needing a project_id input.
type APIKeyData struct {
	Generated      bool   `json:"generated"`
	APIKey         string `json:"api_key"`
	QuotaType      string `json:"quota_type"`
	ProjectID      string `json:"project_id"`
	EmployeeEmail  string `json:"employee_email"`
	EmployeeUserID string `json:"employee_user_id"`
	EmployeeRole   string `json:"employee_role"`
	BusinessName   string `json:"business_name"`
}

// fetchAPIKey calls get_or_generate with the persisted/jar SSO cookie.
// Returns the full APIKeyData (api_key + project_id + employee identity).
func (c *CompassClient) fetchAPIKey() (*APIKeyData, error) {
	return c.fetchAPIKeyAt(compassAPIKeyGetGen)
}

// fetchAPIKeyAt is the URL-parametrized core, used by tests with a mock server.
func (c *CompassClient) fetchAPIKeyAt(endpoint string) (*APIKeyData, error) {
	a, _ := loadAccount(c.storePath) // absent file is non-fatal: post-login uses the jar
	cookie := c.SessionCookie()
	if a == nil {
		a = &AccountData{}
	}
	if cookie == "" {
		cookie = a.SSOSessionCookie
	}
	if cookie == "" {
		return nil, fmt.Errorf("not logged in. Please login with your company Google account first.")
	}
	// The Compass endpoint returns the full identity without needing project_id
	// input; send an empty JSON object (form-encoded is rejected).
	payload, _ := json.Marshal(map[string]string{})
	req, _ := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(payload))
	req.Header.Set("Cookie", cookieHeader(cookie))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cqp api key request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ar APIKeyResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("cqp api key response parse failed: %w", err)
	}
	if ar.Retcode != 0 {
		return nil, fmt.Errorf("cqp api key response retcode=%d message=%s", ar.Retcode, ar.Message)
	}
	var d APIKeyData
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		return nil, fmt.Errorf("cqp api key response missing data")
	}
	if d.APIKey == "" {
		return nil, fmt.Errorf("cqp api key response missing api_key")
	}
	// Managed key is cached in memory only (the desktop app does not persist it).
	c.cachedKey = d.APIKey
	return &d, nil
}

// extractLoginURL finds the login URL in the auth/login 401 body. The Compass
// backend returns it in the `result` field (and may also set a Location header).
func extractLoginURL(body string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err == nil {
		for _, k := range []string{"result", "login_url", "loginUrl", "url", "redirect", "redirect_url", "location"} {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		if d, ok := m["data"].(map[string]any); ok {
			for _, k := range []string{"result", "login_url", "loginUrl", "url"} {
				if v, ok := d[k].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// MonthlyProjectUsage mirrors monthly_usage's data payload (7 fields, matching the
// binary's struct MonthlyProjectUsage). Method POST, requires project_id input.
type MonthlyProjectUsage struct {
	ProjectID    string  `json:"project_id"`
	SelectedYear int     `json:"selected_year"`
	SelectedMonth int    `json:"selected_month"`
	TotalAmount  float64 `json:"total_amount"`
	Usage        float64 `json:"usage"`
	Balance      float64 `json:"balance"`
	Plan         string  `json:"plan"`
}

// MonthlyUsage fetches monthly_usage with the persisted SSO cookie (cookie-authed,
// not the managed key). NOTE: the endpoint is POST and requires project_id input
// (taken from the store's AccountData.ProjectID).
func (c *CompassClient) MonthlyUsage() (*MonthlyProjectUsage, error) {
	a, err := loadAccount(c.storePath)
	if err != nil || a == nil || a.SSOSessionCookie == "" {
		return nil, fmt.Errorf("not logged in")
	}
	if a.ProjectID == "" {
		return nil, fmt.Errorf("no project_id in store; run `ais-switch-proxy login` (or --import) to populate it")
	}
	payload, _ := json.Marshal(map[string]string{"project_id": a.ProjectID})
	req, _ := http.NewRequest(http.MethodPost, compassMonthlyUsage, bytes.NewReader(payload))
	req.Header.Set("Cookie", cookieHeader(a.SSOSessionCookie))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("monthly usage request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var wrap struct {
		Retcode int             `json:"retcode"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("monthly usage response parse failed: %w", err)
	}
	if wrap.Retcode != 0 {
		return nil, fmt.Errorf("monthly usage retcode=%d message=%s", wrap.Retcode, wrap.Message)
	}
	if len(wrap.Data) == 0 {
		return nil, fmt.Errorf("monthly usage response missing data")
	}
	var mu MonthlyProjectUsage
	if err := json.Unmarshal(wrap.Data, &mu); err != nil {
		return nil, fmt.Errorf("monthly usage data parse failed: %w", err)
	}
	return &mu, nil
}

// ---- helpers ----

func logf(format string, args ...any) { log.Printf(format, args...) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
