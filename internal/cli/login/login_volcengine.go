package login

import (
	"bufio"
	"fmt"
	cliframework "model-proxy/internal/cli/framework"
	"os"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// runVolcengineLoginWithInput performs a pool-aware volcengine login. The
// triple (api_key + access_key + secret_key) may be passed directly (tests) or,
// when any is empty, prompted on stdin. The account is deduped by id
// (accountIDFor → AccessKey for volcengine): a new id appends; an existing id
// with replace=true (or an interactive `y` on stdin when replace=false)
// overwrites the entry's triple/label in place; an existing id without
// confirmation aborts with "login cancelled". The entry's label defaults to the
// id when not supplied. The pool is written to ~/.model-proxy/<name>_apikeys.json
// via savePool — the legacy singular <name>_apikey.json is no longer written.
//
// Locking: ALL stdin (triple prompts + replace confirmation) happens BEFORE the
// cross-process lock — same invariant as runApiKeyLoginWithInput. The replace
// confirmation is resolved with a read-only loadPool; the authoritative
// load→dedup→save then runs under withPoolLock (in addVolcengineAccount).
//
// This is the CLI wrapper: it owns stdin prompting + stdout printing, then
// delegates the dedup→save core to addVolcengineAccount (reused by the web
// layer, Task 12).
func RunVolcengineLoginWithInput(cfg *configdomain.Config, provName string, prov configdomain.Provider, inKey, inAK, inSK, label string, replace bool) error {
	// === BEFORE LOCK: apikey + AK + SK prompts ===
	apiKey := strings.TrimSpace(inKey)
	if apiKey == "" {
		fmt.Printf("Ark API Key (对话用，控制台创建): ")
		fmt.Scanln(&apiKey)
	}
	ak := strings.TrimSpace(inAK)
	if ak == "" {
		fmt.Printf("Volcengine Access Key ID (GetAFPUsage 用，IAM 密钥): ")
		fmt.Scanln(&ak)
	}
	sk := strings.TrimSpace(inSK)
	if sk == "" {
		fmt.Printf("Volcengine Secret Access Key: ")
		fmt.Scanln(&sk)
	}
	if apiKey == "" {
		return fmt.Errorf("API key is required")
	}

	id := accountIDFor(prov.Provider, accountCred{APIKey: apiKey, AccessKey: ak})

	// Resolve replace confirmation BEFORE the lock (stdin must never block the
	// cross-process lock).
	if !replace {
		existing, err := loadPool(provName, prov.Provider)
		if err != nil {
			return fmt.Errorf("load pool: %w", err)
		}
		for _, a := range existing.Accounts {
			if a.ID == id {
				fmt.Printf("Account %q is already logged in. Replace its key? [y/N] ", a.Label)
				reader := bufio.NewReader(os.Stdin)
				ans, _ := reader.ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
					return fmt.Errorf("login cancelled")
				}
				break
			}
		}
		replace = true // user confirmed; tell the core to overwrite
	}

	// Announce validation (UX parity with the apikey login's "Validating API
	// key..." line). One line covering whatever addVolcengineAccount will probe
	// (Ark key via usage_url and/or the AK/SK pair) — the AK/SK step is not
	// pre-announced separately because it only runs if the Ark-key probe passes.
	if prov.UsageURL != "" || (ak != "" && sk != "") {
		fmt.Fprintf(os.Stderr, "Validating credentials...\n")
	}

	if _, err := AddVolcengineAccount(cfg, provName, prov, accountCred{APIKey: apiKey, AccessKey: ak, SecretKey: sk}, label, replace); err != nil {
		return err
	}
	// Print the confirmation line (label resolved from the freshly-saved pool,
	// which may have been re-sorted by savePool).
	pool, _ := loadPool(provName, prov.Provider)
	fmt.Println(provider.Green("✓ Saved account ") + provider.Gray(cliframework.Mask(id)+" ("+labelFor(pool, id)+")"))
	return nil
}

// volcengineAKSKValidator validates a Volcengine AccessKey/SecretKey pair. It
// defaults to provider.ValidateVolcengineAKSK (the signed GetAFPUsage control-
// plane call); login_cmd tests override it to assert addVolcengineAccount's
// decision logic without hitting the real Volcengine API. Package-level var
// (process-wide invariant, not per-provider config).
var VolcengineAKSKValidator = provider.ValidateVolcengineAKSK

// addVolcengineAccount is the non-printing core for volcengine's AK/SK triple:
// it validates the Ark API Key (via usage_url, a Bearer GET to /models) and, when
// both AK and SK are present, the AK/SK pair (via the signed GetAFPUsage); then
// dedups by id (= AccessKey for volcengine) under the cross-process lock and
// writes the pool. Returns the account id. No stdin, no stdout — symmetric with
// addApikeyAccount; reused by the web layer (Task 12). Validation happens BEFORE
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
	// addApikeyAccount's usage_url gate: 401/403 or a network error = bad key →
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
	id := accountIDFor(prov.Provider, accountCred{APIKey: apiKey, AccessKey: ak})
	return id, withPoolLock(name, func() error {
		pool, err := loadPool(name, prov.Provider)
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
