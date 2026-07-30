// Package cli hosts migrated CLI commands (stats, models, usage, ...).
// usage command implementation.
package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
)

func CmdUsage(args []string, cfg *configdomain.Config) {
	provName := positional(args)
	if provName == "" {
		// No provider specified → show usage for all configured providers.
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for i, n := range names {
			if i > 0 {
				fmt.Println(cDim(UsageDivider))
			}
			PrintProviderUsage(cfg, n)
		}
		return
	}
	if _, ok := cfg.Providers[provName]; !ok {
		fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, providerNames(cfg))
		os.Exit(1)
	}
	PrintProviderUsage(cfg, provName)
}

// positional returns the first non-flag argument (same shape as the root CLI
// helper: skips --config and its value).
func positional(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			i++
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return a
	}
	return ""
}

func PrintProviderUsage(cfg *configdomain.Config, provName string) {
	prov, ok := cfg.Providers[provName]
	if !ok {
		return
	}
	pool, _ := accountStore().Load(provName, prov.Provider)
	if len(pool.Accounts) >= 2 {
		for ai, a := range pool.Accounts {
			if ai > 0 {
				fmt.Println(cDim(UsageDivider))
			}
			fmt.Printf("%s (%s)\n", cBold(cCyan(a.Label)), mask(a.ID))
			cred := a.Credentials()
			if p := app.BuildOne(cfg, buildOpts(), provName, prov, cred); p != nil {
				if err := p.Usage(); err != nil {
					fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
				}
			}
		}
		return
	}
	// Single-account / non-pooled / aqp / codex: build one provider + call Usage.
	provMap := app.BuildProviders(cfg, accountStore(), buildOpts()).Providers
	p := provMap[provName]
	if p == nil {
		return
	}
	if err := p.Usage(); err != nil {
		fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
	}
}

// usageDivider separates multiple usage blocks (providers in `usage` with no
// arg, or accounts within a pooled provider). Printed between blocks only —
// never before the first or after the last.
const UsageDivider = "────────────────────────────────────────"

// --- usage-local environment seams and display helpers ---

var (
	usageStoreMu  sync.Mutex
	usageStoreKey string
	usageStore    accounts.Store
)

func accountStore() accounts.Store {
	home := homeDir()
	usageStoreMu.Lock()
	defer usageStoreMu.Unlock()
	if home != usageStoreKey {
		usageStore = accounts.NewStore(home)
		usageStoreKey = home
	}
	return usageStore
}

func buildOpts() app.BuildOptions {
	return app.BuildOptions{
		HomeDir:                  homeDir(),
		CodexCLIVersion:          app.CodexCLIVersion,
		CodexCacheVersion:        app.CodexCacheVersion,
		ListArkAgentPlanModelIDs: climodels.ListArkAgentPlanModelIDs,
	}
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func providerNames(cfg *configdomain.Config) string {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func mask(s string) string {
	if s == "" {
		return "(empty)"
	}
	const minReveal = 8
	if len(s) < minReveal {
		return "****"
	}
	return s[:2] + "…" + s[len(s)-2:]
}
