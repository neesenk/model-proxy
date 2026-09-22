package login

import (
	"fmt"
	"strings"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// apiKeyValidationURL returns the endpoint used to validate an API key at login:
// prov.UsageURL when set, otherwise openai_base_url + "/models" (the natural
// Bearer-GET probe), or "" when neither is set (login skips validation). It is
// field-based (not provider_id-based): providers with a real usage API set
// usage_url (zhipu/deepseek/volcengine/kimi-code → unchanged); providers without
// one validate against openai_base_url/models (qwen-plan) or, for pure-decisions
// providers (typesafe), decisions_base_url/models. Shared by the
// "Validating…" message gate and AddApikeyAccount's ValidateKeyBearerGET call.
func ApiKeyValidationURL(prov configdomain.Provider) string {
	if prov.UsageURL != "" {
		return prov.UsageURL
	}
	if prov.OpenAIBaseURL != "" {
		return strings.TrimRight(prov.OpenAIBaseURL, "/") + "/models"
	}
	if prov.DecisionsBaseURL != "" {
		return strings.TrimRight(prov.DecisionsBaseURL, "/") + "/models"
	}
	return ""
}

// AddApikeyAccount is the non-printing apikey login core: it validates the key
// against usage_url (if set), dedups by id under the cross-process lock, and
// writes the pool. Returns the account id. No stdin, no stdout — the CLI shell
// (or the web layer) handles UX. Callers decide replace semantics: the CLI
// resolves it via an interactive prompt BEFORE calling this; the web layer
// passes the client's choice.
//
// replace=false on an existing id returns "login cancelled" without modifying
// the pool — callers surface that error as appropriate (CLI prints, the web
// layer maps it to a 400).
func AddApikeyAccount(cfg *configdomain.Config, name string, prov configdomain.Provider, cred accountCred, label string, replace bool) (string, error) {
	key := strings.TrimSpace(cred.APIKey)
	if key == "" {
		return "", fmt.Errorf("empty API key")
	}
	// Validate against the usage endpoint if configured. 401/403 = key invalid;
	// anything else (200, 404, etc.) = key accepted (the endpoint may not exist,
	// but the key itself was not rejected).
	if err := ValidateKeyBearerGET(ApiKeyValidationURL(prov), key); err != nil {
		return "", err
	}
	id := accounts.AccountID(prov.Provider, accountCred{APIKey: key})
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
			pool.Accounts[idx].APIKey = key
			if label != "" {
				pool.Accounts[idx].Label = label
			}
			pool.Accounts[idx].AddedAt = now
		} else {
			lbl := label
			if lbl == "" {
				lbl = id
			}
			pool.Accounts = append(pool.Accounts, poolAccount{ID: id, Label: lbl, APIKey: key, AddedAt: now})
		}
		return savePool(name, prov.Provider, pool)
	})
}

// RemoveApikeyAccount removes the account with the given id from the named
// pool under the cross-process lock. No-op if the id is absent (no error). No
// stdin, no stdout — symmetric with AddApikeyAccount, reused by the web layer.
func RemoveApikeyAccount(name, providerID, id string) error {
	return accountStoreEnv().RemoveAccount(name, providerID, id)
}

// ApiKeyLike reports whether the provider's login flow is apikey-pool based
// (vs OAuth/SSO device flows), which is exactly the set whose keys work for a
// Bearer GET /models cross-check after login. Mirrors the login dispatch:
// volcengine is excluded (its /models needs V4 signing for plan endpoints).
func ApiKeyLike(providerID string) bool {
	switch providerID {
	case "aqp", "codex", "volcengine":
		return false
	default:
		return true
	}
}
