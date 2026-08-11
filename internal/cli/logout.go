package cli

import (
	"bufio"
	"fmt"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	displaypkg "model-proxy/provider"
	"os"
	"strconv"
	"strings"

	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
	clilogin "model-proxy/internal/cli/login"
	configdomain "model-proxy/internal/config"
)

// hasPoolFile reports whether the plural credential pool file
// (<name>_apikeys.json) exists. Used to dispatch logout between the pool-aware
// path (operate on the pool) and the singular path (legacy single-file removal,
// including aqp/codex oauth files).
func hasPoolFile(name string) bool {
	_, err := os.Stat(clilogin.PoolPath(name))
	return err == nil
}

func CmdLogout(args []string, cfg *configdomain.Config) {
	provName := positional(args)
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
		fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, cliframework.ProviderNames(cfg))
		os.Exit(1)
	}
	providerID := prov.Provider

	// aqp/codex keep their existing single-file logout (oauth_auth.json). An
	// apikey provider with NO plural pool file but a legacy singular file also
	// goes through the singular removal path (backward compat: the legacy
	// singular <name>_apikey.json is removed by clearApiKey).
	if providerID == "aqp" || providerID == "codex" || !hasPoolFile(provName) {
		provMap := app.BuildProviders(cfg, accountStore(), buildOpts()).Providers
		p := provMap[provName]
		if p == nil {
			fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, cliframework.ProviderNames(cfg))
			os.Exit(1)
		}
		if err := p.Logout(); err != nil {
			fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(displaypkg.Green("✓ Logged out"))
		return
	}

	// Pool-aware path: the plural pool file (<name>_apikeys.json) exists.
	//
	// Locking: the interactive/label/all SELECTION (read pool, list accounts,
	// prompt for a number) runs OUTSIDE the cross-process lock; it captures the
	// selected account's ID (not index) from the displayed list. The mutation
	// (re-load under the lock → remove by id → save / os.Remove) runs INSIDE
	// withPoolLock. Removing by id re-resolved under the lock is correct even if
	// the pool changed between display and lock: a concurrently-removed target is
	// a no-op save; a concurrently-added account is preserved.
	pool, err := accountStore().Load(provName, providerID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
		os.Exit(1)
	}
	if len(pool.Accounts) == 0 {
		// Pool file exists but is empty — remove it and report not-logged-in.
		// Re-resolve under the cross-process lock so a concurrent `login` can't
		// append an account between the unlocked read above and the remove; if
		// the pool is still empty under the lock, drop the file. Mirrors the
		// in-lock remove-by-id path below.
		if err := accountStore().WithLock(provName, func() error {
			cur, err := accountStore().Load(provName, providerID)
			if err != nil {
				return err
			}
			if len(cur.Accounts) == 0 {
				if err := os.Remove(clilogin.PoolPath(provName)); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove pool file: %w", err)
				}
			}
			return nil
		}); err != nil {
			fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(displaypkg.Yellow("Not logged in."))
		return
	}

	label := flagStringValue(args, "--label")
	all := hasFlagValue(args, "--all")
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
			fmt.Printf("  %d) %s  (#%s  added %s)\n", i+1, a.Label, cliframework.Mask(a.ID), a.AddedAt)
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

	if err := accountStore().WithLock(provName, func() error {
		cur, err := accountStore().Load(provName, providerID)
		if err != nil {
			return err
		}
		if removeAll {
			cur.Accounts = nil
		} else {
			out := make([]accounts.Account, 0, len(cur.Accounts))
			for _, a := range cur.Accounts {
				if a.ID == rmID {
					continue // drop the selected id
				}
				out = append(out, a)
			}
			cur.Accounts = out
		}
		if len(cur.Accounts) == 0 {
			if err := os.Remove(clilogin.PoolPath(provName)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove pool file: %w", err)
			}
		} else {
			if err := accountStore().Save(provName, providerID, cur); err != nil {
				return fmt.Errorf("save pool: %w", err)
			}
		}
		return nil
	}); err != nil {
		fmt.Fprintf(os.Stderr, "logout failed: %v\n", err)
		os.Exit(1)
	}

	if all {
		fmt.Println(displaypkg.Green("✓ Removed all accounts from " + provName))
	} else {
		fmt.Println(displaypkg.Green("✓ Removed account " + cliframework.Mask(rmID)))
	}
	cliserve.MaybeReloadDaemon(cfg)
}

func flagStringValue(args []string, flag string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

func hasFlagValue(args []string, flag string) bool {
	for _, a := range args {
		if a == flag || strings.HasPrefix(a, flag+"=") {
			return true
		}
	}
	return false
}
