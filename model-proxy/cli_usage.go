package main

import (
	"fmt"
	"log"
	"sort"
)

func cmdUsage(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	provName := positional(args)
	if provName == "" {
		// No provider specified → show usage for all configured providers.
		// printProviderUsage handles pool iteration for pooled parents (which
		// are absent from the buildProviders map under their plain name).
		names := make([]string, 0, len(cfg.Providers))
		for n := range cfg.Providers {
			names = append(names, n)
		}
		sort.Strings(names)
		for i, n := range names {
			if i > 0 {
				fmt.Println(cDim(usageDivider))
			}
			printProviderUsage(cfg, n)
		}
		return
	}
	if _, ok := cfg.Providers[provName]; !ok {
		log.Fatalf("unknown provider %q; available: %s", provName, providerNames(cfg))
	}
	printProviderUsage(cfg, provName)
}

// printProviderUsage prints the usage for one provider entry, handling the
// credential-pool case (≥2 accounts): each account gets its own divider +
// per-account header (label + masked id) and the per-account usage fetch is
// dispatched with THAT account's cred. Single-account / non-pooled / aqp /
// codex go through the existing path (buildProviders → p.Usage).
//
// This does NOT use NewProxy — that would start the quota tracker (goroutines
// + HTTP polls), wasteful for a one-shot CLI. Pool accounts are iterated
// directly via loadPool, and the bound usage function is called per account.
func printProviderUsage(cfg *Config, provName string) {
	prov, ok := cfg.Providers[provName]
	if !ok {
		return
	}
	pool, _ := loadPool(provName, prov.Provider)
	if len(pool.Accounts) >= 2 {
		for ai, a := range pool.Accounts {
			if ai > 0 {
				fmt.Println(cDim(usageDivider))
			}
			fmt.Printf("%s (%s)\n", cBold(cCyan(a.Label)), mask(a.ID))
			cred := a.Credentials()
			if p := buildOne(cfg, provName, prov, cred); p != nil {
				if err := p.Usage(); err != nil {
					fmt.Println(cYellow("  (usage unavailable: " + err.Error() + ")"))
				}
			}
		}
		return
	}
	// Single-account / non-pooled / aqp / codex: build one provider + call Usage.
	provMap := buildProviders(cfg).providers
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
const usageDivider = "────────────────────────────────────────"
