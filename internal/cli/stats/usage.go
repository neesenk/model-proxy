// usage.go implements the `usage` command: per-provider quota/credit reads.
package stats

import (
	"fmt"
	"model-proxy/internal/accounts"
	"model-proxy/internal/display"
	"model-proxy/internal/providerbuild"
	"os"
	"sort"
	"strings"
	"sync"

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
				fmt.Println(display.Dim(UsageDivider))
			}
			PrintProviderUsage(cfg, n)
		}
		return
	}
	if _, ok := cfg.Providers[provName]; !ok {
		fmt.Fprintf(os.Stderr, "unknown provider %q; available: %s\n", provName, cfg.ProviderNames())
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
				fmt.Println(display.Dim(UsageDivider))
			}
			fmt.Printf("%s (%s)\n", display.Bold(display.Cyan(a.Label)), accounts.Mask(a.ID))
			cred := a.Credentials()
			if p := providerbuild.BuildOne(cfg, providerbuild.BuildOpts(), provName, prov, cred); p != nil {
				if err := p.Usage(); err != nil {
					fmt.Println(display.Yellow("  (usage unavailable: " + err.Error() + ")"))
				}
			}
		}
		return
	}
	// Single-account / non-pooled / aqp / codex: build one provider + call Usage.
	provMap := providerbuild.BuildProviders(cfg, accountStore(), providerbuild.BuildOpts()).Providers
	p := provMap[provName]
	if p == nil {
		return
	}
	if err := p.Usage(); err != nil {
		fmt.Println(display.Yellow("  (usage unavailable: " + err.Error() + ")"))
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

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
