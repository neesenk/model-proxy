package provider

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"model-proxy/internal/credstore"
)

// aqp_store.go holds the aqp (compass) account-store layer: the persisted SSO
// account (google_oauth_auth.json / <name>_oauth_auth.json), its load/save/clear
// helpers, the SSO cookie-header builder, and the aqp backend base URL + quota
// endpoint path. Moved from main's gateway.go so the provider package owns the
// aqp auth store end-to-end (it already read the SSO cookie via ReadSSOCookie).
//
// main's AqpClient (the SSO login / web-login orchestration with its cookie jar)
// stays in gateway.go and uses these exported helpers for store access. The
// monthly_usage quota fetch lives on AqpProvider directly (aqp.go).

// AqpBase is the compass backend base URL (production). Overridable via
// Config.AqpBaseURL for tests (httptest mock).
const AqpBase = "https://compass.llm.shopee.io"

// aqpMonthlyUsagePath is the monthly_usage quota endpoint (POST, SSO-cookie
// authed, project_id input). Provider-internal: only AqpProvider.fetchMonthlyUsage
// calls it.
const aqpMonthlyUsagePath = "/api/v1/cqp/ccswitch/monthly_usage"

// SsoCookieName is the SSO_C session cookie name (set by auth/info after login).
// Shared by the provider's CookieHeader and main's SSO login flow.
const SsoCookieName = "SSO_C"

// AqpAccountData mirrors <name>_oauth_auth.json. 6 fields; the managed AQP key
// is NOT persisted (fetched on demand, cached in memory only).
type AqpAccountData struct {
	AccountID        string `json:"account_id"`
	CreatedAt        int64  `json:"created_at"`
	Email            string `json:"email"`
	LastRefreshAt    int64  `json:"last_refresh_at"`
	ProjectID        string `json:"project_id"`
	SSOSessionCookie string `json:"sso_session_cookie"` // full "SSO_C=<value>" or raw value
}

// LoadAqpAccount reads account data; returns nil, nil if the store is absent.
func LoadAqpAccount(path string) (*AqpAccountData, error) {
	b, err := credstore.NewRef(path).Load()
	if err != nil {
		if errors.Is(err, credstore.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	var a AqpAccountData
	if err := json.Unmarshal(b, &a); err != nil {
		// Name the actual store file (<name>_oauth_auth.json), not the legacy
		// default — a wrong filename misleads account debugging.
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &a, nil
}

// SaveAqpAccount writes account data through credstore (0600 file mode,
// keychain entry in keychain mode), filling in timestamps.
func SaveAqpAccount(path string, a *AqpAccountData) error {
	if a.CreatedAt == 0 {
		a.CreatedAt = time.Now().Unix()
	}
	if a.LastRefreshAt == 0 {
		a.LastRefreshAt = time.Now().Unix()
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return credstore.NewRef(path).Save(b)
}

// ClearAqpAccount removes the account store (logout). Treating "absent" as
// success (idempotent).
func ClearAqpAccount(path string) error {
	return credstore.NewRef(path).Delete()
}

// CookieHeader builds a Cookie header value from the stored sso_session_cookie.
// The stored value may be the full "SSO_C=<value>" pair or just the raw value.
func CookieHeader(stored string) string {
	if stored == "" {
		return ""
	}
	if strings.Contains(stored, "=") {
		return stored
	}
	return fmt.Sprintf("%s=%s", SsoCookieName, stored)
}
