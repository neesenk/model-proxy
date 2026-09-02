package login

import (
	"fmt"
	"strings"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// VolcengineAKSKValidator validates a Volcengine AccessKey/SecretKey pair. It
// defaults to provider.ValidateVolcengineAKSK (the signed GetAFPUsage control-
// plane call); login tests override it to assert AddVolcengineAccount's
// decision logic without hitting the real Volcengine API. Package-level var
// (process-wide invariant, not per-provider config).
var VolcengineAKSKValidator = provider.ValidateVolcengineAKSK

// AddVolcengineAccount is the non-printing core for volcengine's AK/SK triple:
// it validates the Ark API Key (via usage_url, a Bearer GET to /models) and, when
// both AK and SK are present, the AK/SK pair (via the signed GetAFPUsage); then
// dedups by id (= AccessKey for volcengine) under the cross-process lock and
// writes the pool. Returns the account id. No stdin, no stdout — symmetric with
// AddApikeyAccount; reused by the web layer. Validation happens BEFORE
// the lock (a slow probe must not hold the cross-process lock). AK/SK are
// optional — chat-only accounts skip that check. replace=false on an existing id
// returns "login cancelled" without modifying the pool.
func AddVolcengineAccount(cfg *configdomain.Config, name string, prov configdomain.Provider, cred accountCred, label string, replace bool) (string, error) {
	apiKey := strings.TrimSpace(cred.APIKey)
	ak := strings.TrimSpace(cred.AccessKey)
	sk := strings.TrimSpace(cred.SecretKey)
	if apiKey == "" {
		return "", fmt.Errorf("API key is required")
	}
	// Validate the Ark API Key via usage_url (GET /models with Bearer), mirroring
	// AddApikeyAccount's usage_url gate: 401/403 or a network error = bad key →
	// reject before save. No-op when usage_url is unset.
	if err := ValidateKeyBearerGET(prov.UsageURL, apiKey); err != nil {
		return "", err
	}
	// AK/SK are optional (chat-only accounts omit them entirely), but they must
	// be BOTH set or BOTH empty: a lone AK or SK can't sign GetAFPUsage
	// (resolveAKSK requires both), yet before this guard it saved silently and
	// degraded to BillingUnknown at runtime with no login-time signal. Reject
	// the partial pair up front.
	if (ak == "") != (sk == "") {
		return "", fmt.Errorf("AccessKey and SecretKey must both be set, or both be empty for a chat-only account")
	}
	if ak != "" && sk != "" {
		if err := VolcengineAKSKValidator(ak, sk); err != nil {
			return "", fmt.Errorf("validation failed: %w", err)
		}
	}
	id := accounts.AccountID(prov.Provider, accountCred{APIKey: apiKey, AccessKey: ak})
	return id, withPoolLock(name, func() error {
		pool, err := LoadPool(name, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		now := nowTS()
		idx := -1
		for i, a := range pool.Accounts {
			if a.ID == id {
				idx = i
				break
			}
		}
		if idx >= 0 {
			if !replace {
				return fmt.Errorf("login cancelled")
			}
			pool.Accounts[idx].APIKey = apiKey
			pool.Accounts[idx].AccessKey = ak
			pool.Accounts[idx].SecretKey = sk
			if label != "" {
				pool.Accounts[idx].Label = label
			}
			pool.Accounts[idx].AddedAt = now
		} else {
			lbl := label
			if lbl == "" {
				lbl = id
			}
			pool.Accounts = append(pool.Accounts, poolAccount{
				ID: id, Label: lbl, APIKey: apiKey, AccessKey: ak, SecretKey: sk, AddedAt: now,
			})
		}
		return savePool(name, prov.Provider, pool)
	})
}
