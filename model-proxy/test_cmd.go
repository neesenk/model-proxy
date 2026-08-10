package main

import (
	"context"
	"fmt"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	"model-proxy/provider"
	"net/http"
	"os"
	"sort"
	"time"

	"model-proxy/internal/probe"
)

// test_cmd.go implements `model-proxy test <model>` — an end-to-end link test:
// resolve the model's route targets (explicit routes, then the implicit-route
// fallback) and probe EACH once with probeModelCallable, the same minimal real
// upstream call `models refresh` uses (per-provider base/path/auth wiring).
// Exit status is 0 when at least one target answers 2xx, 1 when every target
// fails (or the model has no route at all).

func cmdTest(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", provider.Red("✗"), err)
		os.Exit(1)
	}
	model := cliframework.Positional(args)
	if model == "" {
		fmt.Fprintf(os.Stderr, "%s usage: model-proxy test <model> [--config PATH]\n", provider.Red("✗"))
		os.Exit(1)
	}
	// claude_mapping translates a claude alias to the exposed model name first,
	// mirroring forward's routing order.
	if mapped, ok := cfg.ClaudeMapping[model]; ok {
		fmt.Printf("%s %s → %s\n", provider.Dim("claude_mapping:"), model, mapped)
		model = mapped
	}
	targets := testTargetsFor(cfg, model)
	if len(targets) == 0 {
		fmt.Fprintf(os.Stderr, "%s no route for model %q; available routes: %s\n", provider.Red("✗"), model, cliframework.RouteNames(cfg))
		os.Exit(1)
	}
	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}
	anyOK := false
	for _, t := range targets {
		ok, status, reason, latency := probeRouteTarget(client, cfg, t)
		lat := latency.Round(time.Millisecond)
		if ok {
			anyOK = true
			fmt.Printf("%s %s → %s (%s) — HTTP %d (%s)\n", provider.Green("✓"), model, t.Provider, t.Model, status, lat)
			continue
		}
		// status 0 = build/auth/network error (no upstream answer) — print the
		// reason without a bogus "HTTP 0".
		if status != 0 {
			fmt.Printf("%s %s → %s (%s) — HTTP %d: %s (%s)\n", provider.Red("✗"), model, t.Provider, t.Model, status, provider.Truncate(reason, 120), lat)
		} else {
			fmt.Printf("%s %s → %s (%s) — %s (%s)\n", provider.Red("✗"), model, t.Provider, t.Model, provider.Truncate(reason, 120), lat)
		}
	}
	if !anyOK {
		os.Exit(1)
	}
}

// testTargetsFor resolves the probe target list for an exposed model: explicit
// routes sorted by priority asc (lower = tried first); when the model has no
// explicit route, the implicit-route fallback (auto-derived from logged-in
// providers' model lists, same as forward). Nil when no route covers the model.
func testTargetsFor(cfg *Config, model string) []RouteTarget {
	targets, ok := cfg.Routes[model]
	if !ok {
		implicit, _ := app.SynthesizeImplicitRoutes(cfg, accountStore())
		t, found := implicit[model]
		if !found {
			return nil
		}
		targets = []RouteTarget{t}
	}
	out := append([]RouteTarget(nil), targets...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out
}

// probeRouteTarget probes one route target once and reports
// (ok, httpStatus, reason, latency). The implementation comes from
// providerImplFor — the first pooled virtual for a pooled provider, mirroring
// the forward path's credential binding.
func probeRouteTarget(client *http.Client, cfg *Config, t RouteTarget) (ok bool, status int, reason string, latency time.Duration) {
	provCfg, ok := cfg.Providers[t.Provider]
	if !ok {
		return false, 0, "provider not in config", 0
	}
	impl, err := climodels.ProviderImplFor(cfg, t.Provider)
	if err != nil {
		return false, 0, err.Error(), 0
	}
	r := probe.Exchange(context.Background(), client, provCfg, impl, t.Model)
	return r.OK, r.Status, r.Reason, r.Latency
}
