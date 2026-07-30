package models

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"model-proxy/internal/app"
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/provider"
)

// models_check.go implements the endpoint probe used by `models refresh`: after
// fetching a provider's live model list (and applying any static regex filter),
// each candidate id is probed against the provider's OWN configured base_url +
// call rules with a minimal request. A 2xx response means the model is callable
// on that endpoint; anything else is dropped (with a recorded reason).
//
// The probe replicates proxy.forward's request build (base+path selection by
// protocol, RewriteRequest, AuthHeaders, prov.Headers, aqp's special headers)
// WITHOUT the streaming/circuit/failover/metrics machinery - it only inspects
// the HTTP status code. This keeps it usable from the offline CLI refresh path
// (no *Proxy, no daemon) and per-provider accurate (codex's /responses +
// store:false, aqp's /v1/messages + ?beta=true are all applied via the same
// code paths the real forward uses).

// probeConcurrency bounds the number of concurrent model probes per refresh.
// Probes are real upstream calls; a small bound avoids bursting a provider.
const probeConcurrency = 5

// DropReason is one model dropped by the endpoint probe.
type DropReason struct {
	Model  string `json:"model"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// checkProviderModels probes each id in `ids` against the provider's endpoint
// and returns the callable subset (kept, preserving input order) plus the
// dropped ones with reasons. It resolves the provider implementation via
// providerImplFor (so auth/rewrite/probe-shape are wired exactly as in the live
// proxy) and probes concurrently (bounded by probeConcurrency). A build failure
// (e.g. not logged in) returns an error - the caller should fall back to keeping
// all ids rather than silently dropping them.
func CheckProviderModels(cfg *configdomain.Config, provName string, ids []string) (kept []string, dropped []DropReason, err error) {
	provCfg, ok := cfg.Providers[provName]
	if !ok {
		return nil, nil, fmt.Errorf("unknown provider %q", provName)
	}
	impl, err := ProviderImplFor(cfg, provName)
	if err != nil {
		return nil, nil, err
	}

	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}

	type result struct {
		idx    int
		id     string
		ok     bool
		status int
		reason string
	}
	results := make([]result, len(ids))

	sem := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ok, status, reason := probe.Callable(context.Background(), client, provCfg, impl, id)
			results[i] = result{i, id, ok, status, reason}
		}(i, id)
	}
	wg.Wait()

	for _, r := range results {
		if r.ok {
			kept = append(kept, r.id)
		} else {
			dropped = append(dropped, DropReason{Model: r.id, Status: r.status, Reason: r.reason})
		}
	}
	return kept, dropped, nil
}

// providerImplFor resolves the provider implementation for `provName`, picking
// the first pooled virtual (its bound cred) when pooled - mirroring the forward
// path's binding. Shared by applyProviderModelFilter and checkProviderModels so
// the filter and probe use the SAME impl. Returns nil + error when the provider
// is unknown or not built (e.g. not logged in).
func ProviderImplFor(cfg *configdomain.Config, provName string) (provider.Provider, error) {
	if _, ok := cfg.Providers[provName]; !ok {
		return nil, fmt.Errorf("unknown provider %q", provName)
	}
	provMap := app.BuildProviders(cfg, accountStore(), buildOpts()).Providers
	target := provName
	if vids, pooled := PoolVirtuals(cfg, provName); pooled {
		target = vids[0]
	}
	impl := provMap[target]
	if impl == nil {
		return nil, fmt.Errorf("provider %q not available (not logged in?)", provName)
	}
	return impl, nil
}

// applyProviderModelFilter applies provider-specific static policy filters via
// the provider implementation's FilterModelIDs (volcengine drops *-latest /
// doubao-seed-1-* / lite / mini; others pass through). This is the "policy"
// pass; the endpoint probe (checkProviderModels) is the separate "callability"
// pass. Applied to the merged config+fetched set so stale config ids are cleaned
// up too. A build failure (not logged in) returns the input unchanged.
func ApplyProviderModelFilter(cfg *configdomain.Config, provName string, ids []string) (kept, dropped []string) {
	impl, err := ProviderImplFor(cfg, provName)
	if err != nil || impl == nil {
		return ids, nil // can't build impl -> skip policy filter (probe will report)
	}
	return impl.FilterModelIDs(ids)
}

// mergeModelIDs returns existing first, then any fetched ids not already
// present (in fetch order), deduped. Used by `models refresh` to build the set
// the endpoint probe validates - so pre-existing ids get re-validated too.
func MergeModelIDs(existing []string, entries []ModelEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return MergeStringIDs(existing, ids)
}

// mergeStringIDs returns a first, then any ids from b not already present (in b
// order), deduped. Shared by `models refresh` (existing config models + fetched
// ids) and the route-probe fallback (existing config models + route-target
// models) to build the candidate set the endpoint probe validates.
func MergeStringIDs(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	merged := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	return merged
}

// routeModelsForProvider returns the model ids targeting `provName` in `routes`
// - the `model` field of each RouteTarget whose provider is `provName` - deduped
// and sorted. These are the candidate ids for the route-probe fallback used when
// a provider has no /models endpoint: the operator has wired these models in
// routes, so probing them discovers which the provider actually serves. Sorted
// because `routes` is a map (random iteration order); a deterministic candidate
// order keeps probe/display/drop output stable across runs.
func RouteModelsForProvider(cfg *configdomain.Config, provName string) []string {
	seen := map[string]bool{}
	var out []string
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			if t.Provider == provName && !seen[t.Model] {
				seen[t.Model] = true
				out = append(out, t.Model)
			}
		}
	}
	sort.Strings(out)
	return out
}

// printKeptModels prints the final (post-filter) model list to stdout as a
// table with models.dev metadata (context/output/input modalities/source) -
// matching the `model-proxy models` display. Shown BEFORE the filter summary.
// `meta`/`sources` come from hydrateModels (keyed by provider -> model id).
func PrintKeptModels(provName string, kept []string, meta map[string]map[string]catalog.Model, sources map[string]map[string]app.ModelSource) {
	if len(kept) == 0 {
		fmt.Println(cYellow("(no models)"))
		return
	}
	fmt.Printf("%s  %s  %s  %s  %s  %s\n",
		cDim(pad("MODEL ID", 26)), cDim(pad("NAME", 20)),
		cDim(pad("CTX", 10)), cDim(pad("OUTPUT", 8)),
		cDim(pad("INPUT MODALITIES", 18)), cDim(pad("SRC", 10)))
	for _, id := range kept {
		var m catalog.Model
		if meta != nil && meta[provName] != nil {
			m = meta[provName][id]
		}
		name := id
		ctx := "-"
		if m.Context > 0 {
			ctx = fmt.Sprintf("%d", m.Context)
		}
		out := "-"
		if m.Output > 0 {
			out = fmt.Sprintf("%d", m.Output)
		}
		mod := "text"
		if len(m.Modalities.Input) > 0 {
			mod = strings.Join(m.Modalities.Input, "/")
		}
		src := ""
		if sources != nil {
			switch sources[provName][id] {
			case app.SrcModelsDev:
				src = "models.dev"
			case app.SrcDefault:
				src = "default"
			}
		}
		fmt.Printf("%s  %s  %s  %s  %s  %s\n",
			cCyan(pad(id, 26)), cGreen(pad(name, 20)),
			cGray(pad(ctx, 10)), cGray(pad(out, 8)),
			cGray(pad(mod, 18)), cGray(pad(src, 10)))
	}
	fmt.Printf("\n%s %s: %d models\n", cDim("provider:"), provName, len(kept))
}

// printFilterSummary prints the filter summary to stderr AFTER the final list.
// It reports two kinds of drops with a short reason each:
//   - policyDropped: ids removed by the static regex rules (e.g. "*-latest")
//   - probeDropped:  ids removed because they are not callable on the provider's
//     base_url (HTTP non-2xx, with the upstream's error code/message)
//
// allFailed indicates every probed model failed (likely a login/network issue),
// in which case the probe drops are surfaced as a warning rather than a verdict.
func PrintFilterSummary(policyDropped []string, probeDropped []DropReason, perr error, allFailed bool) {
	if perr != nil {
		fmt.Fprintf(os.Stderr, "endpoint probe skipped (%v); list written unvalidated\n", perr)
		return
	}
	if len(policyDropped) == 0 && len(probeDropped) == 0 {
		return
	}
	fmt.Fprintln(os.Stderr) // blank line separating the list from the summary
	if allFailed {
		fmt.Fprintf(os.Stderr, "filtered out %d model(s) - probe failed for ALL (likely not logged in / network):\n", len(probeDropped))
	} else {
		fmt.Fprintf(os.Stderr, "filtered out %d model(s):\n", len(policyDropped)+len(probeDropped))
	}
	for _, id := range policyDropped {
		fmt.Fprintf(os.Stderr, "  %-30s %s\n", id, "excluded by filter rule")
	}
	for _, d := range probeDropped {
		reason := ProbeDropReason(d)
		if allFailed {
			reason = "probe failed (login/network?) - " + reason
		}
		fmt.Fprintf(os.Stderr, "  %-30s %s\n", d.Model, reason)
	}
}

// probeDropReason renders one probe drop as a short, human-readable cause:
// the upstream's error code + message when available, else the HTTP status,
// else a network/build-error label.
func ProbeDropReason(d DropReason) string {
	if d.Reason != "" {
		return "not callable on base_url - " + d.Reason
	}
	if d.Status != 0 {
		return fmt.Sprintf("not callable on base_url - HTTP %d", d.Status)
	}
	return "not callable on base_url - network/build error"
}

// sameStringSet reports whether a and b contain the same set of strings
// (order-independent). Used to skip a config rewrite when refresh produces no
// net change.
func SameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, s := range a {
		set[s]++
	}
	for _, s := range b {
		set[s]--
		if set[s] < 0 {
			return false
		}
	}
	return true
}

// diffStringSets returns (inAnotB, inBnotA) - added/removed relative to a -> b.
// `added` = ids now in b that weren't in a; `removed` = ids that were in a but
// not in b. Both sorted for stable display.
func DiffStringSets(a, b []string) (added, removed []string) {
	as := make(map[string]bool, len(a))
	for _, s := range a {
		as[s] = true
	}
	bs := make(map[string]bool, len(b))
	for _, s := range b {
		bs[s] = true
	}
	for s := range bs {
		if !as[s] {
			added = append(added, s)
		}
	}
	for s := range as {
		if !bs[s] {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
