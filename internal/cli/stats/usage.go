// usage.go implements the `usage` command: per-provider quota/credit reads.
package stats

import (
	"fmt"
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/internal/display"
	"model-proxy/internal/providerbuild"
	"os"
	"sort"
	"strings"

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
	// A plural pool keys usage by account (virtual runtime keys) regardless of
	// size; a legacy singular file / not-logged-in provider keeps the plain
	// name. The snapshot source — not the account count — decides.
	snapshot, _ := accountStore().LoadSnapshot(provName, prov.Provider)
	if snapshot.Source == accounts.SourcePlural && len(snapshot.Pool.Accounts) > 0 {
		for ai, a := range snapshot.Pool.Accounts {
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
	// Legacy singular / non-pooled / aqp / codex: build one provider + call Usage.
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

// accountStoreLazy is the shared CLI seam (cli/framework): the credential
// store is cached keyed by the current home directory so tests can isolate
// HOME (t.Setenv) before first use.
var accountStoreLazy cliframework.LazyAccountStore

func accountStore() accounts.Store {
	return accountStoreLazy.Get()
}
