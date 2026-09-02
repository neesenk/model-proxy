package login

import (
	"bufio"
	"fmt"
	"model-proxy/internal/accounts"
	"model-proxy/internal/display"
	logincore "model-proxy/internal/login"
	"os"
	"strings"

	configdomain "model-proxy/internal/config"
)

// runVolcengineLoginWithInput performs a pool-aware volcengine login. The
// triple (api_key + access_key + secret_key) may be passed directly (tests) or,
// when any is empty, prompted on stdin. The account is deduped by id
// (accounts.AccountID → AccessKey for volcengine): a new id appends; an
// existing id with replace=true (or an interactive `y` on stdin when
// replace=false) overwrites the entry's triple/label in place; an existing id
// without confirmation aborts with "login cancelled". The entry's label
// defaults to the id when not supplied. The pool is written to
// ~/.model-proxy/<name>_apikeys.json via the login core — the legacy singular
// <name>_apikey.json is no longer written.
//
// Locking: ALL stdin (triple prompts + replace confirmation) happens BEFORE the
// cross-process lock — same invariant as runApiKeyLoginWithInput. The replace
// confirmation is resolved with a read-only LoadPool; the authoritative
// load→dedup→save then runs under the pool lock (in
// logincore.AddVolcengineAccount).
//
// This is the CLI wrapper: it owns stdin prompting + stdout printing, then
// delegates the dedup→save core to logincore.AddVolcengineAccount (reused by
// the web layer, Task 12).
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

	id := accounts.AccountID(prov.Provider, accounts.Credentials{APIKey: apiKey, AccessKey: ak})

	// Resolve replace confirmation BEFORE the lock (stdin must never block the
	// cross-process lock).
	if !replace {
		existing, err := logincore.LoadPool(provName, prov.Provider)
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
	// key..." line). One line covering whatever AddVolcengineAccount will probe
	// (Ark key via usage_url and/or the AK/SK pair) — the AK/SK step is not
	// pre-announced separately because it only runs if the Ark-key probe passes.
	if prov.UsageURL != "" || (ak != "" && sk != "") {
		fmt.Fprintf(os.Stderr, "Validating credentials...\n")
	}

	if _, err := logincore.AddVolcengineAccount(cfg, provName, prov, accounts.Credentials{APIKey: apiKey, AccessKey: ak, SecretKey: sk}, label, replace); err != nil {
		return err
	}
	// Print the confirmation line (label resolved from the freshly-saved pool,
	// which may have been re-sorted by the pool save).
	pool, _ := logincore.LoadPool(provName, prov.Provider)
	fmt.Println(display.Green("✓ Saved account ") + display.Gray(accounts.Mask(id)+" ("+logincore.AccountLabel(pool, id)+")"))
	return nil
}
