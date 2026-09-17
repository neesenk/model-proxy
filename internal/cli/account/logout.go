// Package account owns the `logout` command: clearing provider credentials
// (pool-aware, legacy singular, and oauth paths).
package account

import (
	"bufio"
	"fmt"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	"model-proxy/internal/display"
	"os"
	"strconv"
	"strings"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/login"
	"model-proxy/internal/providerbuild"
)

// hasPoolFile reports whether the plural credential pool file
// (<name>_apikeys.json) exists. Used to dispatch logout between the pool-aware
// path (operate on the pool) and the singular path (legacy single-file removal,
// including aqp/codex oauth files).
func hasPoolFile(name string) bool {
	_, err := os.Stat(login.PoolPath(name))
	return err == nil
}

func CmdLogout(args []string, cfg *configdomain.Config) {
	provName := cliframework.Positional(args)
	if provName == "" {
		fmt.Println("usage: model-proxy logout <provider> [--label <name>] [--all]")
		fmt.Println("available providers:")
		for name, p := range cfg.Providers {
			fmt.Printf("  %s (provider=%s)\n", name, p.Provider)
		}
		return
	}
	prov, ok := cfg.Providers[provName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, cfg.ProviderNames())
		os.Exit(1)
	}
	providerID := prov.Provider

	// aqp/codex keep their existing single-file logout (oauth_auth.json). An
	// apikey provider with NO plural pool file but a legacy singular file also
	// goes through the singular removal path (backward compat: the legacy
	// singular <name>_apikey.json is removed by clearApiKey).
	if providerID == "aqp" || providerID == "codex" || !hasPoolFile(provName) {
		provMap := providerbuild.BuildProviders(cfg, accountStore(), providerbuild.BuildOpts()).Providers
		p := provMap[provName]
		if p == nil {
			fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, cfg.ProviderNames())
			os.Exit(1)
		}
		if err := p.Logout(); err != nil {
			fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(display.Green("✓ Logged out"))
		return
	}

	// Pool-aware path: the plural pool file (<name>_apikeys.json) exists.
	//
	// Locking: the interactive/label/all SELECTION (read pool, list accounts,
	// prompt for a number) runs OUTSIDE the cross-process lock; it captures the
	// selected account's ID (not index) from the displayed list. The mutation
	// (re-load under the Store lock → remove by id → save) runs inside the
	// Store-owned removal method. Removing by id re-resolved under the lock is
	// correct even if the pool changed between display and lock: a concurrently
	// removed target is a no-op save; a concurrently-added account is preserved.
	pool, err := accountStore().Load(provName, providerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
		os.Exit(1)
	}
	all := cliframework.HasFlagValue(args, "--all")
	if len(pool.Accounts) == 0 {
		// Pool file exists but is empty: an authoritative credential
		// TOMBSTONE (provider-pools.md) — it must stay on disk. Removing it
		// would re-open the legacy singular fallback and resurrect an old key
		// after "removed all accounts". --all still enters the Store removal
		// boundary so stale restore provenance from an older writer is cleaned.
		// The pool is already empty, so the user IS logged out: a cleanup
		// failure here (e.g. keychain backend unreachable) must not turn an
		// idempotent "not logged in" into a hard error — warn and stay
		// successful. Provenance cleanup is retried on the next logout.
		if all {
			if err := accountStore().RemoveAllAccounts(provName, providerID); err != nil {
				fmt.Fprintf(os.Stderr, "warning: stale credential cleanup failed (already logged out): %v\n", err)
			}
		}
		fmt.Println(display.Yellow("Not logged in."))
		return
	}

	label := cliframework.FlagStringValue(args, "--label")
	// removeAll: --all clears every account. rmID: specific account id to drop.
	var rmID string
	removeAll := false
	switch {
	case all:
		removeAll = true
	case label != "":
		idx := -1
		for i, a := range pool.Accounts {
			if a.Label == label {
				idx = i
				break
			}
		}
		if idx < 0 {
			fmt.Fprintf(os.Stderr, "no account labeled %q in %s\n", label, provName)
			os.Exit(1)
		}
		rmID = pool.Accounts[idx].ID
	default:
		// Interactive: list + pick a number.
		fmt.Printf("Accounts for %s:\n", provName)
		for i, a := range pool.Accounts {
			fmt.Printf("  %d) %s  (#%s  added %s)\n", i+1, a.Label, accounts.Mask(a.ID), a.AddedAt)
		}
		fmt.Print("Remove which (number)? ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(pool.Accounts) {
			fmt.Fprintln(os.Stderr, "invalid selection")
			os.Exit(1)
		}
		rmID = pool.Accounts[n-1].ID
	}

	store := accountStore()
	var removeErr error
	if removeAll {
		removeErr = store.RemoveAllAccounts(provName, providerID)
	} else {
		removeErr = store.RemoveAccount(provName, providerID, rmID)
	}
	if removeErr != nil {
		fmt.Fprintf(os.Stderr, "logout failed: %v\n", removeErr)
		os.Exit(1)
	}

	if all {
		fmt.Println(display.Green("✓ Removed all accounts from " + provName))
	} else {
		fmt.Println(display.Green("✓ Removed account " + accounts.Mask(rmID)))
	}
	cliserve.MaybeReloadDaemon(args, cfg)
}
