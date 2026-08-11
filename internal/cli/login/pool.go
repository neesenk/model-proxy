package login

import (
	"fmt"
	"io"
	"net/http"
	"time"

	"model-proxy/internal/accounts"
	displaypkg "model-proxy/provider"
)

// Account-store seams, resolved lazily so tests can isolate HOME via
// accountsStoreOverride (root wrapper and tests set it through
// accountStoreEnv). Production resolves accounts.NewStore(HomeDir()).
var accountStoreEnv = func() accounts.Store { return accounts.NewStore(HomeDir()) }

func loadPool(name, providerID string) (accounts.Pool, error) {
	return accountStoreEnv().Load(name, providerID)
}

func savePool(name, providerID string, pool accounts.Pool) error {
	return accountStoreEnv().Save(name, providerID, pool)
}

func withPoolLock(name string, fn func() error) error {
	return accountStoreEnv().WithLock(name, fn)
}

func accountIDFor(providerID string, cred accounts.Credentials) string {
	return accounts.AccountID(providerID, cred)
}

func nowTS() string { return accounts.Timestamp(time.Now()) }

// accountCred/poolAccount mirror the root-package aliases used by login flows.
type accountCred = accounts.Credentials
type poolAccount = accounts.Account

// ValidateKeyBearerGET rejects a key when the validation endpoint answers
// 401/403 (or is unreachable). No-op when url is empty.
func ValidateKeyBearerGET(url, key string) error {
	if url == "" {
		return nil
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return fmt.Errorf("validation failed: HTTP %d: %s", resp.StatusCode, displaypkg.Truncate(string(body), 200))
	}
	return nil
}

// labelFor returns the label of the pool entry with the given id, or the id
// itself when not found (the pool may have been re-sorted by savePool).
func labelFor(pool accounts.Pool, id string) string {
	for _, a := range pool.Accounts {
		if a.ID == id {
			return a.Label
		}
	}
	return id
}
