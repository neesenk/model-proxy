package main

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

// hasPoolFile reports whether the plural credential pool file
// (<name>_apikeys.json) exists. Used to dispatch logout between the pool-aware
// path (operate on the pool) and the singular path (legacy single-file removal,
// including aqp/codex oauth files).
func hasPoolFile(name string) bool {
	_, err := os.Stat(poolPath(name))
	return err == nil
}

func cmdLogout(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
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
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	providerID := prov.Provider

	// aqp/codex keep their existing single-file logout (oauth_auth.json). An
	// apikey provider with NO plural pool file but a legacy singular file also
	// goes through the singular removal path (backward compat: the legacy
	// singular <name>_apikey.json is removed by clearApiKey).
	if providerID == "aqp" || providerID == "codex" || !hasPoolFile(provName) {
		provMap := buildProviders(cfg).Providers
		p := provMap[provName]
		if p == nil {
			log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
		}
		if err := p.Logout(); err != nil {
			log.Fatalf("logout failed: %v", err)
		}
		fmt.Println(cGreen("✓ Logged out"))
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
	pool, err := loadPool(provName, providerID)
	if err != nil {
		log.Fatalf("logout failed: %v", err)
	}
	if len(pool.Accounts) == 0 {
		// Pool file exists but is empty — remove it and report not-logged-in.
		// Re-resolve under the cross-process lock so a concurrent `login` can't
		// append an account between the unlocked read above and the remove; if
		// the pool is still empty under the lock, drop the file. Mirrors the
		// in-lock remove-by-id path below.
		if err := withPoolLock(provName, func() error {
			cur, err := loadPool(provName, providerID)
			if err != nil {
				return err
			}
			if len(cur.Accounts) == 0 {
				if err := os.Remove(poolPath(provName)); err != nil && !os.IsNotExist(err) {
					return fmt.Errorf("remove pool file: %w", err)
				}
			}
			return nil
		}); err != nil {
			log.Fatalf("logout failed: %v", err)
		}
		fmt.Println(cYellow("Not logged in."))
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
			log.Fatalf("no account labeled %q in %s", label, provName)
		}
		rmID = pool.Accounts[idx].ID
	default:
		// Interactive: list + pick a number.
		fmt.Printf("Accounts for %s:\n", provName)
		for i, a := range pool.Accounts {
			fmt.Printf("  %d) %s  (#%s  added %s)\n", i+1, a.Label, mask(a.ID), a.AddedAt)
		}
		fmt.Print("Remove which (number)? ")
		reader := bufio.NewReader(os.Stdin)
		line, _ := reader.ReadString('\n')
		n, err := strconv.Atoi(strings.TrimSpace(line))
		if err != nil || n < 1 || n > len(pool.Accounts) {
			log.Fatal("invalid selection")
		}
		rmID = pool.Accounts[n-1].ID
	}

	if err := withPoolLock(provName, func() error {
		cur, err := loadPool(provName, providerID)
		if err != nil {
			return err
		}
		if removeAll {
			cur.Accounts = nil
		} else {
			out := make([]poolAccount, 0, len(cur.Accounts))
			for _, a := range cur.Accounts {
				if a.ID == rmID {
					continue // drop the selected id
				}
				out = append(out, a)
			}
			cur.Accounts = out
		}
		if len(cur.Accounts) == 0 {
			if err := os.Remove(poolPath(provName)); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("remove pool file: %w", err)
			}
		} else {
			if err := savePool(provName, providerID, cur); err != nil {
				return fmt.Errorf("save pool: %w", err)
			}
		}
		return nil
	}); err != nil {
		log.Fatalf("logout failed: %v", err)
	}

	if all {
		fmt.Println(cGreen("✓ Removed all accounts from " + provName))
	} else {
		fmt.Println(cGreen("✓ Removed account " + mask(rmID)))
	}
	maybeReloadDaemon(args)
}
