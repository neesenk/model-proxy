package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	displaypkg "model-proxy/internal/provider"
	"model-proxy/internal/routing"
)

// CmdRoutes prints the effective route table: derived routes (aggregated from
// provider model lists, priority inherited per provider, aliases applied) with
// explicit routes overriding. `routes` lists every exposed model; `routes
// <model>` prints one model's ordered targets in detail.
func CmdRoutes(args []string, cfg *configdomain.Config) {
	table := routing.RouteTable(cfg)
	model := cliframework.Positional(args)

	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)

	if model == "" {
		if len(names) == 0 {
			fmt.Println("(no routes)")
			return
		}
		fmt.Printf("%-28s %s\n", "MODEL", "TARGETS (in scheduling order)")
		for _, name := range names {
			targets := table[name]
			parts := make([]string, 0, len(targets))
			for _, t := range targets {
				parts = append(parts, formatRouteTarget(t))
			}
			fmt.Printf("%-28s %s\n", name, strings.Join(parts, " → "))
		}
		return
	}

	targets, ok := table[model]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s no route for model %q; available: %s\n",
			displaypkg.Red("✗"), model, cfg.RouteNames())
		os.Exit(1)
	}
	fmt.Printf("%s %s — %d target(s)\n", displaypkg.Bold("route"), model, len(targets))
	if origin := routeNameOrigin(cfg, model); origin != "" {
		fmt.Println(origin)
	}
	for _, t := range targets {
		prov := cfg.Providers[t.Provider]
		fmt.Printf("  %s %-12s model=%-24s priority=%d tier=%s\n",
			displaypkg.Dim("•"), t.Provider, t.Model, t.Priority, billingTier(prov))
		if t.Protocol != "" {
			fmt.Printf("    protocol: %s (declared)\n", t.Protocol)
		}
	}
}

// formatRouteTarget renders one target as provider/upstream-model; a differing
// upstream name (alias) is what makes the model field visible in the list.
func formatRouteTarget(t configdomain.RouteTarget) string {
	return fmt.Sprintf("%s/%s", t.Provider, t.Model)
}

// routeNameOrigin explains where an exposed name comes from: a provider alias
// or an explicit routes: entry overriding the derived aggregation.
func routeNameOrigin(cfg *configdomain.Config, model string) string {
	var aliased []string
	for name, prov := range cfg.Providers {
		for real, exposed := range prov.Alias {
			if exposed == model {
				aliased = append(aliased, fmt.Sprintf("%s/%s", name, real))
			}
		}
	}
	sort.Strings(aliased)
	if _, explicit := cfg.Routes[model]; explicit {
		return "  explicit routes: entry (overrides derivation)"
	}
	if len(aliased) > 0 {
		return fmt.Sprintf("  alias of %s", strings.Join(aliased, ", "))
	}
	return "  derived from provider model lists"
}

func billingTier(prov configdomain.Provider) string {
	if prov.Billing == "pay-as-you-go" {
		return "pay-as-you-go"
	}
	return "plan"
}

func RunRoutes(args []string) { CmdRoutes(args, LoadCmdConfig(args)) }
