package login

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"model-proxy/internal/provider"
)

// Core of AQP SSO: a single cookie jar carried across the whole login flow. Both the
// SSO_A bootstrap cookie and the SSO_C session cookie (set by the auth/info poll's 200)
// are retained in the jar.

// ---- Endpoints / constants ----

const (
	AqpAuthLoginPath    = "/compass-api/v1/auth/login" // bootstrap: 401 + SSO_A + result URL
	AqpAuthInfoPath     = "/compass-api/v1/auth/info"  // session poll: 200 + SSO_C when authed
	AqpAPIKeyGetGenPath = "/api/v1/cqp/ccswitch/api_key/get_or_generate"

	LoginCompletePath = "/company-gateway/login-complete"
)

// ---- Account persistence (google_oauth_auth.json) ----
// AccountData / loadAccount / saveAccount / clearAccount / cookieHeader /
// ssoCookieName / aqpBase / aqpMonthlyUsagePath moved to the provider package
// (provider/aqp_store.go) in Stage 1 of the aqp-fetch migration. The provider
// owns the aqp auth store end-to-end; this file uses provider.* for store access.

// ---- AQP client (with cookie jar) ----

// AqpClient talks to the AQP backend. It keeps a cookie jar across the SSO
// flow so the SSO_A bootstrap cookie and the SSO_C session cookie (set by the
// auth/info poll's 200 after login) are retained.
type AqpClient struct {
	HTTP      *http.Client
	Jar       http.CookieJar
	StorePath string
	Base      string // base URL (aqpBase in production; overridable for tests)

	mu        sync.Mutex
	cachedKey string
}

// newAqpClient builds a AQP client backed by the given store file.
func NewAqpClient(storePath string) *AqpClient {
	jar, _ := cookiejar.New(nil)
	return &AqpClient{
		HTTP:      &http.Client{Timeout: 30 * time.Second, Jar: jar},
		Jar:       jar,
		StorePath: storePath,
		Base:      provider.AqpBase,
	}
}

// newAqpClientWithBase builds an AQP client pointing at an arbitrary base URL.
// Used by the web login flow's test seam (httptest mock); production callers use
// newAqpClient (base = provider.AqpBase, identical to pre-seam behavior).
func NewAqpClientWithBase(storePath, base string) *AqpClient {
	c := NewAqpClient(storePath)
	c.Base = base
	return c
}

// cookieHeader moved to the provider package (provider.CookieHeader).

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
func (c *AqpClient) BootstrapLoginURL() (string, error) {
	return c.BootstrapLoginURLContext(context.Background())
}

// BootstrapLoginURLContext is BootstrapLoginURL with caller-controlled
// cancellation of the bootstrap HTTP request.
func (c *AqpClient) BootstrapLoginURLContext(ctx context.Context) (string, error) {
	return c.BootstrapAtContext(ctx, c.Base+AqpAuthLoginPath)
}

// bootstrapAt is the URL-parametrized core, used by tests with a mock server.
func (c *AqpClient) BootstrapAt(endpoint string) (string, error) {
	return c.BootstrapAtContext(context.Background(), endpoint)
}

func (c *AqpClient) BootstrapAtContext(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("aqp sso bootstrap request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("aqp sso bootstrap: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// The bootstrap returns 401 with a `result` field carrying the soup.shopee.io
	// login URL (and sets the SSO_A cookie, retained by the jar).
	loginURL := ExtractLoginURL(string(body))
	if loginURL == "" {
		return "", fmt.Errorf("aqp sso bootstrap missing login URL: status=%d, body=%s",
			resp.StatusCode, provider.Truncate(string(body), 300))
	}
	for _, ck := range c.PublicCookies() {
		logf("[GoogleGateway] bootstrap jar cookie: %s=%s", ck.Name, cliframework.Mask(ck.Value))
	}
	return loginURL, nil
}

// PollSession polls auth/info (using the jar's cookies) until retcode==0 && hasAccess.
// After a successful login the response sets the SSO_C cookie, captured by the jar.
func (c *AqpClient) PollSession(timeout time.Duration) (*AuthInfoData, error) {
	return c.PollSessionContext(context.Background(), timeout)
}

// PollSessionContext is PollSession with caller-controlled cancellation. The
// context covers both each auth/info HTTP request and the wait between polls.
func (c *AqpClient) PollSessionContext(ctx context.Context, timeout time.Duration) (*AuthInfoData, error) {
	return c.PollAtContext(ctx, c.Base+AqpAuthInfoPath, timeout)
}

func (c *AqpClient) PollAt(endpoint string, timeout time.Duration) (*AuthInfoData, error) {
	return c.PollAtContext(context.Background(), endpoint, timeout)
}

func (c *AqpClient) PollAtContext(ctx context.Context, endpoint string, timeout time.Duration) (*AuthInfoData, error) {
	pollCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr error
	for {
		data, err := c.CheckSessionAtContext(pollCtx, endpoint)
		if err == nil && data != nil {
			return data, nil
		}
		lastErr = err

		timer := time.NewTimer(2 * time.Second)
		select {
		case <-pollCtx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("aqp sso session check canceled: %w", ctxErr)
			}
			if lastErr != nil {
				return nil, fmt.Errorf("aqp sso session check failed: %w", lastErr)
			}
			return nil, fmt.Errorf("aqp sso session check timed out")
		case <-timer.C:
		}
	}
}

func (c *AqpClient) CheckSessionAt(endpoint string) (*AuthInfoData, error) {
	return c.CheckSessionAtContext(context.Background(), endpoint)
}

func (c *AqpClient) CheckSessionAtContext(ctx context.Context, endpoint string) (*AuthInfoData, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("aqp sso session check request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.Jar != nil {
		u, _ := url.Parse(endpoint)
		var names []string
		for _, ck := range c.Jar.Cookies(u) {
			names = append(names, ck.Name+"="+cliframework.Mask(ck.Value))
		}
		logf("[GoogleGateway] auth/info jar cookies: %s", strings.Join(names, "; "))
	}
	resp, err := c.HTTP.Do(req) // jar sends SSO_A/SSO_C automatically
	if err != nil {
		return nil, fmt.Errorf("aqp sso session check failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Log status + body length only — the auth/info body carries identity (email,
	// userid) that shouldn't land in logs. The error path below keeps a truncated
	// body for diagnosis since it surfaces to the caller on failure, not routine logs.
	logf("[GoogleGateway] auth/info status=%d body=%dB", resp.StatusCode, len(body))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aqp sso session check failed: status=%d body=%s",
			resp.StatusCode, provider.Truncate(string(body), 200))
	}
	var air AuthInfoResponse
	if err := json.Unmarshal(body, &air); err != nil {
		return nil, fmt.Errorf("aqp auth info response parse failed: %w", err)
	}
	if air.Retcode != 0 {
		return nil, fmt.Errorf("aqp sso session rejected: retcode=%d message=%s", air.Retcode, air.Message)
	}
	var d AuthInfoData
	if err := json.Unmarshal(air.Data, &d); err != nil {
		return nil, fmt.Errorf("aqp auth info data parse failed: %w", err)
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
func (c *AqpClient) SessionCookie() string {
	if c.Jar == nil {
		return ""
	}
	u, _ := url.Parse(c.Base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == provider.SsoCookieName {
			return fmt.Sprintf("%s=%s", ck.Name, ck.Value)
		}
	}
	return ""
}

// PublicCookies returns the jar's cookies for the aqp host (for debugging).
func (c *AqpClient) PublicCookies() []*http.Cookie {
	if c.Jar == nil {
		return nil
	}
	u, _ := url.Parse(c.Base)
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
func (c *AqpClient) FetchAPIKey() (*APIKeyData, error) {
	return c.FetchAPIKeyContext(context.Background())
}

// fetchAPIKeyContext is fetchAPIKey with caller-controlled cancellation.
func (c *AqpClient) FetchAPIKeyContext(ctx context.Context) (*APIKeyData, error) {
	return c.FetchAPIKeyAtContext(ctx, c.Base+AqpAPIKeyGetGenPath)
}

// fetchAPIKeyAt is the URL-parametrized core, used by tests with a mock server.
func (c *AqpClient) FetchAPIKeyAt(endpoint string) (*APIKeyData, error) {
	return c.FetchAPIKeyAtContext(context.Background(), endpoint)
}

func (c *AqpClient) FetchAPIKeyAtContext(ctx context.Context, endpoint string) (*APIKeyData, error) {
	a, _ := provider.LoadAqpAccount(c.StorePath) // absent file is non-fatal: post-login uses the jar
	cookie := c.SessionCookie()
	if a == nil {
		a = &provider.AqpAccountData{}
	}
	if cookie == "" {
		cookie = a.SSOSessionCookie
	}
	if cookie == "" {
		return nil, fmt.Errorf("not logged in. Please login with your company Google account first.")
	}
	// The AQP endpoint returns the full identity without needing project_id
	// input; send an empty JSON object (form-encoded is rejected).
	payload, _ := json.Marshal(map[string]string{})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("aqp api key request: %w", err)
	}
	req.Header.Set("Cookie", provider.CookieHeader(cookie))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aqp api key request failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var ar APIKeyResponse
	if err := json.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("aqp api key response parse failed: %w", err)
	}
	if ar.Retcode != 0 {
		return nil, fmt.Errorf("aqp api key response retcode=%d message=%s", ar.Retcode, ar.Message)
	}
	var d APIKeyData
	if err := json.Unmarshal(ar.Data, &d); err != nil {
		return nil, fmt.Errorf("aqp api key response missing data")
	}
	if d.APIKey == "" {
		return nil, fmt.Errorf("aqp api key response missing api_key")
	}
	// Managed key is cached in memory only (the desktop app does not persist it).
	c.cachedKey = d.APIKey
	return &d, nil
}

// extractLoginURL finds the login URL in the auth/login 401 body. The Aqp
// backend returns it in the `result` field (and may also set a Location header).
func ExtractLoginURL(body string) string {
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

// ---- helpers ----

func logf(format string, args ...any) { log.Printf(format, args...) }
