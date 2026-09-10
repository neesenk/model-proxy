package models

import (
	"context"
	"encoding/json"
	"fmt"
	"model-proxy/internal/display"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/upstreamproxy"
)

// models_check.go implements the endpoint probe used by `models refresh`: after
// fetching a provider's live model list (and applying any static regex filter),
// each candidate id is probed against the provider's OWN configured bases with
// the three-protocol matrix (chat / anthropic / responses) from
// probe.ProbeModelProtocols, each leg classified via
// wirecap.ClassifyModelStatus. A model is kept when ANY leg concludes Yes;
// otherwise it is dropped with a per-leg reason summary. The fresh matrix is
// also persisted (best-effort) to model_caps.json so the daemon's verdict-based
// routing and the PROTOCOLS display column can reuse it without re-probing.
//
// The probe shares proxy.forward's request build (probe.Do: base+path selection
// by protocol, RewriteRequest, AuthHeaders, prov.Headers, aqp's special
// headers) WITHOUT the streaming/circuit/failover/metrics machinery. This keeps
// it usable from the offline CLI refresh path (no *Proxy, no daemon) and
// per-provider accurate (codex's /responses + store:false, aqp's /v1/messages +
// ?beta=true are all applied via the same code paths the real forward uses).

// probeConcurrency bounds the number of concurrent model probes per refresh.
// Probes are real upstream calls; a small bound avoids bursting a provider.
const probeConcurrency = 5

// DropReason is one model dropped by the endpoint probe.
type DropReason struct {
	Model  string `json:"model"`
	Status int    `json:"status"`
	Reason string `json:"reason"`
}

// checkProviderModels probes each id in `ids` against the provider's endpoints
// with the 3-protocol matrix and returns the callable subset (kept, preserving
// input order) plus the dropped ones with per-leg reasons. It resolves the
// provider implementation via providerImplFor (so auth/rewrite/probe-shape are
// wired exactly as in the live proxy) and probes concurrently (bounded by
// probeConcurrency). A model is kept when ANY protocol leg classifies Yes. The
// freshly-probed matrix is returned AND persisted (best-effort) to
// model_caps.json, replacing this provider's entry. A build failure (e.g. not
// logged in) returns an error - the caller should fall back to keeping all ids
// rather than silently dropping them (nothing is persisted in that case).
func CheckProviderModels(cfg *configdomain.Config, provName string, ids []string) (kept []string, dropped []DropReason, protocols map[string]runtimewire.ModelProtocols, err error) {
	provCfg, ok := cfg.Providers[provName]
	if !ok {
		return nil, nil, nil, fmt.Errorf("unknown provider %q", provName)
	}
	impl, err := ProviderImplFor(cfg, provName)
	if err != nil {
		return nil, nil, nil, err
	}

	client := &http.Client{Timeout: cfg.Scheduling.Timeout(), Transport: upstreamproxy.AutoTransport()}

	probed := probe.ProbeModels(context.Background(), client, provCfg, impl, ids, probeConcurrency)
	protocols = make(map[string]runtimewire.ModelProtocols, len(probed))
	for _, r := range probed {
		mp := runtimewire.ModelProtocols{}
		for _, leg := range r.Legs {
			v := runtimewire.ClassifyModelStatus(leg.Probed, leg.Status, leg.Err, leg.Body)
			switch leg.Leg {
			case probe.LegChat:
				mp.Chat = v
			case probe.LegAnthropic:
				mp.Anthropic = v
			case probe.LegResponses:
				mp.Responses = v
			}
		}
		protocols[r.ID] = mp
		if mp.Chat == runtimewire.Yes || mp.Anthropic == runtimewire.Yes || mp.Responses == runtimewire.Yes {
			kept = append(kept, r.ID)
		} else {
			dropped = append(dropped, DropReason{Model: r.ID, Status: legStatus(r.Legs), Reason: legsSummary(r.Legs)})
		}
	}
	persistModelCaps(provName, provCfg, protocols)
	return kept, dropped, protocols, nil
}

// legStatus picks the most relevant HTTP status for a dropped model: the chat
// leg's when it reached HTTP, else the first probed leg's, else 0 when nothing
// reached an HTTP exchange.
func legStatus(legs []probe.LegResult) int {
	for _, leg := range legs {
		if leg.Leg == probe.LegChat && leg.Probed && leg.Status != 0 {
			return leg.Status
		}
	}
	for _, leg := range legs {
		if leg.Probed && leg.Status != 0 {
			return leg.Status
		}
	}
	return 0
}

// legsSummary renders the per-leg probe outcomes joined by " / ", e.g.
// "chat HTTP 404 / anthropic not probed (no base) / responses HTTP 400: invalid model".
func legsSummary(legs []probe.LegResult) string {
	parts := make([]string, 0, len(legs))
	for _, leg := range legs {
		switch {
		case !leg.Probed:
			parts = append(parts, string(leg.Leg)+" not probed (no base)")
		case leg.Err != nil:
			parts = append(parts, fmt.Sprintf("%s %v", leg.Leg, leg.Err))
		default:
			if reason := legErrorReason(leg.Body); reason != "" {
				parts = append(parts, fmt.Sprintf("%s HTTP %d: %s", leg.Leg, leg.Status, reason))
			} else {
				parts = append(parts, fmt.Sprintf("%s HTTP %d", leg.Leg, leg.Status))
			}
		}
	}
	return strings.Join(parts, " / ")
}

// legErrorReason extracts a short "code: message" from an OpenAI-style error
// body. probe's own extractor is unexported, so the CLI keeps this local twin
// (same shape, request-id stripped via the exported helper).
func legErrorReason(body []byte) string {
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
		msg := strings.TrimSpace(probe.StripRequestID(ej.Error.Message))
		switch {
		case code != "" && msg != "":
			return code + ": " + msg
		case code != "":
			return code
		case msg != "":
			return msg
		}
	}
	return ""
}

// modelCapsFilePath is the CLI-side model_caps.json location (sibling of the
// daemon's quota_state.json under ~/.model-proxy).
func modelCapsFilePath() string {
	return runtimewire.ModelCapsPath(filepath.Join(homeDir(), ".model-proxy", "quota_state.json"))
}

// persistModelCaps best-effort persists one provider's freshly-probed protocol
// matrix to model_caps.json, REPLACING the provider's Models map (refresh
// validates the full candidate set). Other providers' entries are preserved; a
// malformed existing file starts fresh. Failures are a stderr warning only -
// the refresh itself never fails on this.
func persistModelCaps(provName string, provCfg configdomain.Provider, protocols map[string]runtimewire.ModelProtocols) {
	if len(protocols) == 0 {
		return
	}
	path := modelCapsFilePath()
	loaded, err := runtimewire.LoadModelCapsFile(path)
	if err != nil || loaded == nil {
		loaded = map[string]runtimewire.ProviderModelCaps{}
	}
	loaded[provName] = runtimewire.ProviderModelCaps{
		Fingerprint: providerbuild.ProtocolConfigFingerprint(provCfg),
		ProbedAt:    time.Now(),
		Models:      protocols,
	}
	if err := runtimewire.SaveModelCapsFile(path, loaded); err != nil {
		fmt.Fprintf(os.Stderr, "warning: persisting model capabilities to %s: %v\n", path, err)
	}
}

// loadModelCapsProjection reads model_caps.json and returns the protocol
// matrices (provider -> model -> verdicts) for providers whose stored
// fingerprint matches their CURRENT config; stale entries are dropped. A
// missing or malformed file yields nil (silently - the PROTOCOLS column
// degrades to "-", the loadPersistedQuotaBaseline pattern).
func loadModelCapsProjection(cfg *configdomain.Config) map[string]map[string]runtimewire.ModelProtocols {
	loaded, err := runtimewire.LoadModelCapsFile(modelCapsFilePath())
	if err != nil || loaded == nil {
		return nil
	}
	out := map[string]map[string]runtimewire.ModelProtocols{}
	for name, entry := range loaded {
		provCfg, ok := cfg.Providers[name]
		if !ok || entry.Fingerprint != providerbuild.ProtocolConfigFingerprint(provCfg) {
			continue
		}
		out[name] = entry.Models
	}
	return out
}

// protocolsCell renders one model's protocol matrix for the PROTOCOLS column:
// the Yes legs joined by "/" in chat/ant/resp order; "none" when every leg is
// a concluded No; "-" when there's no entry or no leg concluded either way.
func protocolsCell(mp runtimewire.ModelProtocols, ok bool) string {
	if !ok {
		return "-"
	}
	var yes []string
	if mp.Chat == runtimewire.Yes {
		yes = append(yes, "chat")
	}
	if mp.Anthropic == runtimewire.Yes {
		yes = append(yes, "ant")
	}
	if mp.Responses == runtimewire.Yes {
		yes = append(yes, "resp")
	}
	if len(yes) > 0 {
		return strings.Join(yes, "/")
	}
	if mp.Chat == runtimewire.No && mp.Anthropic == runtimewire.No && mp.Responses == runtimewire.No {
		return "none"
	}
	return "-"
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
	provMap := providerbuild.BuildProviders(cfg, accountStore(), buildOpts()).Providers
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

// printKeptModels prints the final (post-filter) model list to stdout as a
// table with models.dev metadata (context/output/input modalities/source) and a
// trailing PROTOCOLS column (from the probe matrix) - matching the
// `model-proxy models` display. Shown BEFORE the filter summary.
// `meta`/`sources` come from hydrateModels (keyed by provider -> model id);
// `protocols` is provider -> model -> the 3-protocol verdict matrix (nil → "-").
func PrintKeptModels(provName string, kept []string, meta map[string]map[string]catalog.Model, sources map[string]map[string]routing.ModelSource, protocols map[string]map[string]runtimewire.ModelProtocols) {
	if len(kept) == 0 {
		fmt.Println(display.Yellow("(no models)"))
		return
	}
	fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
		display.Dim(display.Pad("MODEL ID", 26)), display.Dim(display.Pad("NAME", 20)),
		display.Dim(display.Pad("CTX", 10)), display.Dim(display.Pad("OUTPUT", 8)),
		display.Dim(display.Pad("INPUT MODALITIES", 18)), display.Dim(display.Pad("SRC", 10)),
		display.Dim(display.Pad("PROTOCOLS", 12)))
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
			case routing.SrcModelsDev:
				src = "models.dev"
			case routing.SrcDefault:
				src = "default"
			}
		}
		mp, pok := protocols[provName][id]
		fmt.Printf("%s  %s  %s  %s  %s  %s  %s\n",
			display.Cyan(display.Pad(id, 26)), display.Green(display.Pad(name, 20)),
			display.Gray(display.Pad(ctx, 10)), display.Gray(display.Pad(out, 8)),
			display.Gray(display.Pad(mod, 18)), display.Gray(display.Pad(src, 10)),
			display.Gray(display.Pad(protocolsCell(mp, pok), 12)))
	}
	fmt.Printf("\n%s %s: %d models\n", display.Dim("provider:"), provName, len(kept))
}

// printFilterSummary prints the filter summary to stderr AFTER the final list.
// It reports two kinds of drops with a short reason each:
//   - policyDropped: ids removed by the static regex rules (e.g. "*-latest")
//   - probeDropped:  ids removed because no protocol leg classified them Yes
//     (per-leg summary: status + upstream error code/message where available)
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

// probeDropReason renders one probe drop as a short, human-readable cause: the
// per-leg protocol summary ("chat HTTP 404 / anthropic not probed (no base) /
// ...") when available, else the HTTP status, else a network/build-error label.
func ProbeDropReason(d DropReason) string {
	if d.Reason != "" {
		return "not callable on any protocol - " + d.Reason
	}
	if d.Status != 0 {
		return fmt.Sprintf("not callable on any protocol - HTTP %d", d.Status)
	}
	return "not callable on any protocol - network/build error"
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
