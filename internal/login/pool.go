package login

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/upstreamproxy"
)

// Account-store seams, resolved lazily so tests can isolate HOME via
// accountStoreOverride (root wrapper and tests set it through
// accountStoreEnv). Production resolves accounts.NewStore(accounts.HomeDir()).
var accountStoreEnv = func() accounts.Store { return accounts.NewStore(accounts.HomeDir()) }

// LoadPool reads the named provider's credential pool (read-only; callers that
// mutate must go through the Add*/Remove* cores, which take the cross-process
// lock). The interactive shell uses it to resolve the replace confirmation
// BEFORE the lock so stdin never blocks other logins.
func LoadPool(name, providerID string) (accounts.Pool, error) {
	return accountStoreEnv().Load(name, providerID)
}

func savePool(name, providerID string, pool accounts.Pool) error {
	return accountStoreEnv().Save(name, providerID, pool)
}

func withPoolLock(name string, fn func() error) error {
	return accountStoreEnv().WithLock(name, fn)
}

func nowTS() string { return accounts.Timestamp(time.Now()) }

// accountCred/poolAccount mirror the root-package aliases used by login flows.
type accountCred = accounts.Credentials
type poolAccount = accounts.Account

// ValidateKeyBearerGET rejects a key when the validation endpoint answers
// 401/403 (or is unreachable). It ALSO rejects a BigModel-style HTTP 200
// envelope that carries a business-level auth failure (success:false + 4xx
// code): open.bigmodel.cn's quota/usage endpoints answer 200 OK with
// {"code":401,"msg":"令牌已过期或验证不正确","success":false} for an expired or
// incorrect key, so a status-code-only check waves the key into the pool and
// the failure only surfaces later as a runtime 401 on every model call. No-op
// when url is empty.
func ValidateKeyBearerGET(url, key string) error {
	if url == "" {
		return nil
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 15 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	// Cap the read at 16KB (probe parity): only a short excerpt is used below,
	// and a hostile/huge validation response must not be buffered in full.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("validation failed: HTTP %d: %s", resp.StatusCode, display.Truncate(string(body), 200))
	}
	if code, ok := envelopeAuthFailure(body); ok {
		return fmt.Errorf("validation failed: HTTP %d (envelope code %d): %s",
			resp.StatusCode, code, display.Truncate(string(body), 200))
	}
	return nil
}

// envelopeAuthFailure reports whether body is a JSON envelope carrying a
// business-level auth failure: success:false together with a 4xx code. Only
// that combination counts — success:false with a non-4xx code is an ordinary
// business error that says nothing about the key, and bodies without the
// envelope shape (plain 200s, HTML, empty, truncated) never match.
func envelopeAuthFailure(body []byte) (int, bool) {
	var env struct {
		Code    any   `json:"code"`
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return 0, false
	}
	if env.Success == nil || *env.Success {
		return 0, false
	}
	switch c := env.Code.(type) {
	case float64: // JSON number
		if c >= 400 && c < 500 {
			return int(c), true
		}
	case string: // some deployments quote the code
		if n, err := strconv.Atoi(c); err == nil && n >= 400 && n < 500 {
			return n, true
		}
	}
	return 0, false
}

// AccountLabel returns the label of the pool entry with the given id, or the
// id itself when not found (the pool may have been re-sorted by savePool).
func AccountLabel(pool accounts.Pool, id string) string {
	for _, a := range pool.Accounts {
		if a.ID == id {
			return a.Label
		}
	}
	return id
}

// FetchVisibleModels lists model ids the key can see at the provider's
// OpenAI-style /models endpoint (Bearer GET, same probe semantics as
// ValidateKeyBearerGET). Used by `add` to cross-check a preset's default model
// list against what the account actually serves. Errors are non-fatal: the
// caller degrades to "unknown" rather than failing the command (an endpoint
// that 404s says nothing about the key).
func FetchVisibleModels(prov configdomain.Provider, key string) ([]string, error) {
	base := strings.TrimRight(prov.OpenAIBaseURL, "/")
	if base == "" || key == "" {
		return nil, fmt.Errorf("no base URL or key")
	}
	req, err := http.NewRequest(http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 15 * time.Second, Transport: upstreamproxy.AutoTransport()}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// Cap the read at 256KB: model lists are small; a hostile response must
	// not be buffered in full.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// LatestAPIKey returns the most recently added pool account's API key for the
// provider (best-effort, for the `add` model cross-check immediately after a
// fresh login). Empty when there is no pool.
func LatestAPIKey(provName, providerID string) string {
	pool, err := LoadPool(provName, providerID)
	if err != nil || len(pool.Accounts) == 0 {
		return ""
	}
	latest := pool.Accounts[0]
	for _, a := range pool.Accounts[1:] {
		if a.AddedAt > latest.AddedAt {
			latest = a
		}
	}
	return latest.APIKey
}
