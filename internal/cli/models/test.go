package models

import (
	"context"
	"fmt"
	"model-proxy/internal/display"
	"net/http"
	"os"
	"sort"
	"time"

	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/internal/routing"
	"model-proxy/internal/upstreamproxy"
)

// CmdTest implements `model-proxy test <model>`: probe each route target once.
func CmdTest(args []string) {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", display.Red("✗"), err)
		os.Exit(1)
	}
	model := cliframework.Positional(args)
	if model == "" {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy test <model> [--config PATH]\n", display.Red("✗"))
		os.Exit(1)
	}
	// claude_mapping translates a claude alias to the exposed model name first,
	// mirroring forward's routing order.
	if mapped, ok := cfg.ClaudeMapping[model]; ok {
		fmt.Printf("%s %s → %s\n", display.Dim("claude_mapping:"), model, mapped)
		model = mapped
	}
	targets := testTargetsFor(cfg, model)
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "%s no route for model %q; available routes: %s\n", display.Red("✗"), model, cfg.RouteNames())
		os.Exit(1)
	}
	client := &http.Client{Timeout: cfg.Scheduling.Timeout(), Transport: upstreamproxy.AutoTransport()}
	anyOK := false
	for _, t := range targets {
		ok, status, reason, latency := probeRouteTarget(client, cfg, t)
		lat := latency.Round(time.Millisecond)
		if ok {
			anyOK = true
			fmt.Printf("%s %s → %s (%s) — HTTP %d (%s)\n", display.Green("✓"), model, t.Provider, t.Model, status, lat)
			continue
		}
		// status 0 = build/auth/network error (no upstream answer) — print the
		// reason without a bogus "HTTP 0".
		if status != 0 {
			fmt.Printf("%s %s → %s (%s) — HTTP %d: %s (%s)\n", display.Red("✗"), model, t.Provider, t.Model, status, display.Truncate(reason, 120), lat)
		} else {
			fmt.Printf("%s %s → %s (%s) — %s (%s)\n", display.Red("✗"), model, t.Provider, t.Model, display.Truncate(reason, 120), lat)
		}
	}
	if !anyOK {
		os.Exit(1)
	}
}

// testTargetsFor resolves the probe target list for an exposed model from the
// complete route table (derived routes aggregated from provider model lists,
// explicit routes overriding), sorted by priority asc (lower = tried first).
// Nil when no route covers the model.
func testTargetsFor(cfg *configdomain.Config, model string) []configdomain.RouteTarget {
	targets, ok := routing.RouteTable(cfg)[model]
	if !ok {
		return nil
	}
	out := append([]configdomain.RouteTarget(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// probeRouteTarget probes one route target once and reports
// (ok, httpStatus, reason, latency). The implementation comes from
// providerImplFor — the first pooled virtual for a pooled provider, mirroring
// the forward path's credential binding.
func probeRouteTarget(client *http.Client, cfg *configdomain.Config, t configdomain.RouteTarget) (ok bool, status int, reason string, latency time.Duration) {
	provCfg, ok := cfg.Providers[t.Provider]
	if !ok {
		return false, 0, "provider not in config", 0
	}
	impl, err := ProviderImplFor(cfg, t.Provider)
	if err != nil {
		return false, 0, err.Error(), 0
	}
	r := probe.Exchange(context.Background(), client, provCfg, impl, t.Model)
	return r.OK, r.Status, r.Reason, r.Latency
}
