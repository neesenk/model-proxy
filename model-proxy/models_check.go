package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"

	"model-proxy/internal/catalog"
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

// dropReason is one model dropped by the endpoint probe.
type dropReason struct {
	Model  string `json:"model"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// probeModelCallable sends a minimal request for `modelID` to the provider's
// configured base_url, using that provider's call rules (protocol/path/auth/
// rewrite/extra-headers via the provider implementation), and returns whether
// the upstream answered 2xx. `prov` is the provider CONFIG (base urls, headers);
// `impl` is the provider IMPLEMENTATION (ProbeRequest, RewriteRequest,
// AuthHeaders, ExtraHeaders). Returns (ok, httpStatus, reason) where reason is
// "" on 2xx, else a short "code: message" or status line. A build/auth/network
// error is reported as ok=false with reason set (status 0) - such a model is
// conservatively dropped.
func probeModelCallable(client *http.Client, prov Provider, impl provider.Provider, modelID string) (ok bool, status int, reason string) {
	return probeModelCallableContext(context.Background(), client, prov, impl, modelID)
}

// probeModelCallableContext is the cancellable form used by request-scoped Web
// probes. The context is attached before auth/header hooks so cancellation
// covers the complete outbound round trip while the legacy CLI wrapper keeps
// its existing signature.
func probeModelCallableContext(ctx context.Context, client *http.Client, prov Provider, impl provider.Provider, modelID string) (ok bool, status int, reason string) {
	// Ask the provider implementation for its probe request shape (path + body).
	// This replaces the old `if prov.Provider == "codex"/"aqp"` branches - each
	// provider now owns its probe path/body in its own file.
	pr := impl.ProbeRequest(modelID)
	if pr.Method == "" {
		pr.Method = http.MethodPost
	}

	// Select the base URL by protocol, mirroring forward. A provider with an
	// anthropic_base_url is probed over the anthropic protocol (its primary chat
	// path for Claude Code); otherwise the openai protocol.
	baseURL := prov.OpenAIBaseURL
	path, body := pr.Path, pr.Body
	if prov.AnthropicBaseURL != "" {
		baseURL = prov.AnthropicBaseURL
		// The impl's ProbeRequest is openai-shaped; on the anthropic base the
		// probe must speak anthropic (path + body), otherwise strict bases 404
		// (deepseek) and lenient ones get tested with the wrong protocol shape
		// (zhipu's gateway accepts /chat/completions on the anthropic base).
		path = "/v1/messages"
		body = []byte(`{"model":` + strconv.Quote(modelID) + `,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	}

	targetURL := strings.TrimRight(baseURL, "/") + path
	// Provider-specific URL/body tweaks (aqp ?beta=true, codex store:false).
	targetURL, body = impl.RewriteRequest(targetURL, body, path)

	req, err := http.NewRequestWithContext(ctx, pr.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		return false, 0, "build request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	// Minimal whitelist from forward's copyHeaderWhitelist - the probe has no
	// client request to copy from, so just set the ones the upstream expects.
	req.Header.Set("Accept", "application/json")
	if path == "/v1/messages" {
		// Anthropic endpoints require the version header (set BEFORE
		// ExtraHeaders so a provider impl can still override it).
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	if err := impl.AuthHeaders(req); err != nil {
		return false, 0, "auth: " + err.Error()
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	// Provider-specific per-request headers (aqp: anthropic-version +
	// x-compass-request-id). Same method the forward path calls - one impl.
	impl.ExtraHeaders(req, path)

	resp, err := client.Do(req)
	if err != nil {
		return false, 0, "request: " + err.Error()
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14)) // 16KB cap for the reason excerpt

	status = resp.StatusCode
	if status >= 200 && status < 300 {
		return true, status, ""
	}
	return false, status, probeReason(rb)
}

// probeReason extracts a short "code: message" from an OpenAI-style error body,
// falling back to the raw body (truncated) when the shape doesn't parse.
func probeReason(body []byte) string {
	var ej struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &ej) == nil && ej.Error != nil {
		code := ej.Error.Code
		if code == "" {
			code = ej.Error.Type
		}
		// The upstream appends a long, useless "Request id: <hex>" to the
		// message - strip it so the drop summary stays readable.
		msg := strings.TrimSpace(stripRequestID(ej.Error.Message))
		if code != "" && msg != "" {
			return code + ": " + msg
		}
		if code != "" {
			return code
		}
		if msg != "" {
			return msg
		}
	}
	s := strings.TrimSpace(stripRequestID(string(body)))
	return truncate(s, 160)
}

// stripRequestID removes a trailing " Request id: <token>" (case-insensitive)
// from an upstream error message. Volcengine appends this to every error body;
// it's noise in the filter summary.
func stripRequestID(s string) string {
	if i := strings.LastIndex(strings.ToLower(s), " request id:"); i >= 0 {
		return strings.TrimRight(s[:i], " ")
	}
	return s
}

// checkProviderModels probes each id in `ids` against the provider's endpoint
// and returns the callable subset (kept, preserving input order) plus the
// dropped ones with reasons. It resolves the provider implementation via
// providerImplFor (so auth/rewrite/probe-shape are wired exactly as in the live
// proxy) and probes concurrently (bounded by probeConcurrency). A build failure
// (e.g. not logged in) returns an error - the caller should fall back to keeping
// all ids rather than silently dropping them.
func checkProviderModels(cfg *Config, provName string, ids []string) (kept []string, dropped []dropReason, err error) {
	provCfg, ok := cfg.Providers[provName]
	if !ok {
		return nil, nil, fmt.Errorf("unknown provider %q", provName)
	}
	impl, err := providerImplFor(cfg, provName)
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
			ok, status, reason := probeModelCallable(client, provCfg, impl, id)
			results[i] = result{i, id, ok, status, reason}
		}(i, id)
	}
	wg.Wait()

	for _, r := range results {
		if r.ok {
			kept = append(kept, r.id)
		} else {
			dropped = append(dropped, dropReason{Model: r.id, Status: r.status, Reason: r.reason})
		}
	}
	return kept, dropped, nil
}

// providerImplFor resolves the provider implementation for `provName`, picking
// the first pooled virtual (its bound cred) when pooled - mirroring the forward
// path's binding. Shared by applyProviderModelFilter and checkProviderModels so
// the filter and probe use the SAME impl. Returns nil + error when the provider
// is unknown or not built (e.g. not logged in).
func providerImplFor(cfg *Config, provName string) (provider.Provider, error) {
	if _, ok := cfg.Providers[provName]; !ok {
		return nil, fmt.Errorf("unknown provider %q", provName)
	}
	provMap := buildProviders(cfg).providers
	target := provName
	if vids, pooled := poolVirtuals(cfg, provName); pooled {
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
func applyProviderModelFilter(cfg *Config, provName string, ids []string) (kept, dropped []string) {
	impl, err := providerImplFor(cfg, provName)
	if err != nil || impl == nil {
		return ids, nil // can't build impl -> skip policy filter (probe will report)
	}
	return impl.FilterModelIDs(ids)
}

// mergeModelIDs returns existing first, then any fetched ids not already
// present (in fetch order), deduped. Used by `models refresh` to build the set
// the endpoint probe validates - so pre-existing ids get re-validated too.
func mergeModelIDs(existing []string, entries []ModelEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.ID
	}
	return mergeStringIDs(existing, ids)
}

// mergeStringIDs returns a first, then any ids from b not already present (in b
// order), deduped. Shared by `models refresh` (existing config models + fetched
// ids) and the route-probe fallback (existing config models + route-target
// models) to build the candidate set the endpoint probe validates.
func mergeStringIDs(a, b []string) []string {
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
func routeModelsForProvider(cfg *Config, provName string) []string {
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
func printKeptModels(provName string, kept []string, meta map[string]map[string]catalog.Model, sources map[string]map[string]modelSource) {
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
			case srcModelsDev:
				src = "models.dev"
			case srcDefault:
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
func printFilterSummary(policyDropped []string, probeDropped []dropReason, perr error, allFailed bool) {
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
		reason := probeDropReason(d)
		if allFailed {
			reason = "probe failed (login/network?) - " + reason
		}
		fmt.Fprintf(os.Stderr, "  %-30s %s\n", d.Model, reason)
	}
}

// probeDropReason renders one probe drop as a short, human-readable cause:
// the upstream's error code + message when available, else the HTTP status,
// else a network/build-error label.
func probeDropReason(d dropReason) string {
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
func sameStringSet(a, b []string) bool {
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
func diffStringSets(a, b []string) (added, removed []string) {
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
