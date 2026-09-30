package login

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	"model-proxy/internal/upstreamproxy"
)

// apiKeyValidationURL returns the endpoint used to validate an API key at login:
// prov.UsageURL when set, otherwise openai_base_url + "/models" (the natural
// Bearer-GET probe), or "" when neither is set (login skips validation). It is
// field-based (not provider_id-based): providers with a real usage API set
// usage_url (zhipu/deepseek/volcengine/kimi-code → unchanged); providers without
// one validate against openai_base_url/models (qwen-plan) or, for pure-decisions
// providers (typesafe), decisions_base_url/models. Shared by the
// "Validating…" message gate and AddApikeyAccount's ValidateKeyBearerGET call.
// Providers whose resolved endpoint is public and auth-ignoring
// (keyProbeRequired) do NOT use it — AddApikeyAccount validates those with a
// real model request instead; the gate still holds (validation happens, just
// not as a GET on this URL).
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

// openAIModelsURL returns the provider's OpenAI-style /models endpoint — the
// natural Bearer-GET key probe — or "" when no OpenAI base is configured. Used
// as the second-chance validation target when the usage endpoint rejects a key
// (see AddApikeyAccount).
func openAIModelsURL(prov configdomain.Provider) string {
	if prov.OpenAIBaseURL == "" {
		return ""
	}
	return strings.TrimRight(prov.OpenAIBaseURL, "/") + "/models"
}

// AddApikeyAccount is the non-printing apikey login core: it validates the key
// (usage_url Bearer GET when set, otherwise the provider's key-rejecting
// surface — /models normally, a minimal real ProbeRequest when /models is
// public), dedups by id under the cross-process lock, and writes the pool.
// Returns the account id. No stdin, no stdout — the CLI shell (or the web
// layer) handles UX. Callers decide replace semantics: the CLI resolves it via
// an interactive prompt BEFORE calling this; the web layer passes the client's
// choice.
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
	// but the key itself was not rejected). Providers whose resolved validation
	// endpoint can NEVER reject a key (ModelsAuthless && no usage_url — e.g.
	// opencode-go's public /models) skip the Bearer GET entirely and validate
	// with one minimal real model request instead (validateKeyByRealProbe).
	if keyProbeRequired(prov) {
		if err := validateKeyByRealProbe(name, prov, key); err != nil {
			return "", err
		}
	} else if err := ValidateKeyBearerGET(ApiKeyValidationURL(prov), key); err != nil {
		// The usage endpoint rejected the key. Before giving up, retry against
		// the provider's OpenAI /models surface: some BigModel-family usage
		// endpoints (notably the coding-plan quota envelope) reject key shapes
		// the model endpoints accept, and model calls are what the key is for.
		// Only when BOTH reject is the key actually invalid — the /models error
		// is the clearer one to surface. An authless /models (openrouter:
		// public, ignores the Bearer) can never reject anything, so for those
		// providers the usage endpoint's verdict is final — falling back would
		// wave the garbage key the usage endpoint just rejected into the pool.
		if fb := openAIModelsURL(prov); fb != "" && fb != ApiKeyValidationURL(prov) &&
			!provider.ModelsAuthless(prov.Provider) {
			if fbErr := ValidateKeyBearerGET(fb, key); fbErr != nil {
				return "", fbErr
			}
		} else {
			return "", err
		}
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

// keyProbeRequired reports whether the login key validation must send a real
// model request instead of the cheap Bearer GET: the provider's resolved
// validation endpoint is public and ignores Authorization (ModelsAuthless —
// it answers 200 to any Bearer, so it can never reject a bad key) AND there is
// no usage_url to validate against instead. Driven by the property "the
// validation endpoint cannot reject a key", not by provider id: if such an
// upstream later adds auth to /models (ModelsAuthless entry removed) or gains
// a usage_url, validation returns to the GET path with no code change here.
func keyProbeRequired(prov configdomain.Provider) bool {
	return prov.UsageURL == "" && provider.ModelsAuthless(prov.Provider)
}

// ErrKeyUnverifiable marks a validation outcome where the upstream could not
// decide the key's validity: the real-request probe was answered with 429 or a
// 5xx, so the key was neither rejected nor accepted — yet a real (billable)
// model request was already spent. Callers must keep this distinct from a
// "key invalid" verdict: the key does not enter the pool (fail-closed), but
// the operator message says "could not verify, retry" instead of "bad key".
var ErrKeyUnverifiable = errors.New("key validation inconclusive")

// validateKeyByRealProbe validates a key by sending ONE minimal real model
// request — the provider's own ProbeRequest shape against openai_base_url —
// with the candidate key bound as the credential. Verdicts: 401/403 or a
// business-envelope auth failure = key rejected; a build/auth/network error =
// rejected (fail-closed, same as the GET path); 429 or any 5xx = the probe
// could not decide (the request may already have been billed, and a rate
// limit or upstream outage says nothing about the key) — rejected with
// ErrKeyUnverifiable so "unable to verify" is never waved through as
// "valid"; any other status (200, 400, 402, …) = accepted — the key was not
// rejected, which is all login validation can claim. Fail-closed with a
// clear error when the config has no model to probe with or no openai base.
func validateKeyByRealProbe(name string, prov configdomain.Provider, key string) error {
	if len(prov.Models) == 0 {
		return fmt.Errorf("validation failed: provider %q has no models configured to probe the key against", name)
	}
	if prov.OpenAIBaseURL == "" {
		return fmt.Errorf("validation failed: provider %q has no openai_base_url to probe the key against", name)
	}
	impl, err := provider.New(&provider.Config{
		ProviderID:    prov.Provider,
		ProviderName:  name,
		OpenAIBaseURL: prov.OpenAIBaseURL,
		Headers:       prov.Headers,
		UsageURL:      prov.UsageURL,
		BoundAPIKey:   key, // in-memory binding: the candidate key, never a stored one
		Models:        prov.Models,
	}, name)
	if err != nil || impl == nil {
		return fmt.Errorf("validation failed: build provider %q: %w", name, err)
	}
	pr := impl.ProbeRequest(prov.Models[0])
	client := &http.Client{Timeout: 15 * time.Second, Transport: upstreamproxy.AutoTransport()}
	rep, err := probe.Do(context.Background(), client, prov, impl, probe.Request{
		BaseURL: prov.OpenAIBaseURL,
		Method:  pr.Method,
		Path:    pr.Path,
		Body:    pr.Body,
	})
	if err != nil {
		return fmt.Errorf("validation failed: %w", err)
	}
	if rep.Status == http.StatusUnauthorized || rep.Status == http.StatusForbidden {
		return fmt.Errorf("validation failed: HTTP %d: %s", rep.Status, display.Truncate(string(rep.Body), 200))
	}
	if code, ok := envelopeAuthFailure(rep.Body); ok {
		return fmt.Errorf("validation failed: HTTP %d (envelope code %d): %s",
			rep.Status, code, display.Truncate(string(rep.Body), 200))
	}
	if rep.Status == http.StatusTooManyRequests || rep.Status >= 500 {
		return fmt.Errorf("%w: HTTP %d: %s — the probe was rate-limited or the upstream errored, so the key could not be verified (retry later)",
			ErrKeyUnverifiable, rep.Status, display.Truncate(string(rep.Body), 200))
	}
	return nil
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
