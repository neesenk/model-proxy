package admin

// models_refresh.go — the daemon twin of `model-proxy models refresh
// <provider>` (POST /api/models/refresh): fetch the provider's live model
// list, probe every candidate with the 3-protocol matrix, overwrite
// providers.<name>.models with the callable subset (comment-preserving),
// hot-reload, and replace the provider's cached verdicts with the fresh
// matrix. The safety nets mirror the CLI (docs/backend-contracts.md): a
// fetch/probe outage never wipes models: — the merged/policy-filtered list is
// written unvalidated with a warning instead. One deliberate hardening over
// the CLI: the verdict cache is replaced ONLY from a healthy probe — an
// all-failed probe keeps the previous matrix (fail-closed), where the CLI's
// best-effort file persist also lands failed verdicts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"

	"gopkg.in/yaml.v3"
)

// refreshProbeConcurrency bounds concurrent model probes per refresh. Probes
// are real upstream calls; a small bound avoids bursting a provider (same
// value as the CLI's probeConcurrency).
const refreshProbeConcurrency = 5

// RefreshModels runs the full fetch → filter → probe → write → reload cycle
// for one provider against the live daemon state.
func (s *Service) RefreshModels(ctx context.Context, name string) (appapi.ModelsRefreshResult, error) {
	if err := ctx.Err(); err != nil {
		return appapi.ModelsRefreshResult{}, err
	}
	runtime := s.ports.ModelRefreshRuntime(name)
	cfg := runtime.Config
	provCfg, ok := cfg.Providers[name]
	if !ok {
		return appapi.ModelsRefreshResult{}, appapi.NewHTTPError(http.StatusNotFound, "unknown provider: "+name)
	}
	result := appapi.ModelsRefreshResult{Provider: name}
	existing := provCfg.Models
	impl := runtime.Provider

	// Candidate set: existing config models merged with the live fetch. When
	// the impl or its /models endpoint is unavailable, fall back to the
	// route-target models — the CLI's fallback, so refresh still validates
	// (and never silently empties) the configured set.
	var merged []string
	switch {
	case impl == nil:
		merged = mergeIDs(existing, routing.RouteModelsForProvider(cfg, name))
		result.Warning = name + " is not available (not logged in?); model list written unvalidated"
	default:
		fetched, err := provider.FetchModelsContext(ctx, impl)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if err != nil {
			merged = mergeIDs(existing, routing.RouteModelsForProvider(cfg, name))
			result.Warning = fmt.Sprintf("models endpoint unavailable (%v); probed route-configured models instead", err)
		} else {
			merged = mergeIDs(existing, fetched)
		}
	}
	if len(merged) == 0 {
		return result, appapi.NewHTTPError(http.StatusBadRequest,
			"no models to refresh for "+name+" (provider not logged in and no routes target it)")
	}

	// Policy filter (provider-specific static rules, e.g. volcengine's
	// *-latest drop) applied to BOTH existing and freshly-fetched ids, so a
	// stale config is cleaned up too.
	policyKept := merged
	if impl != nil {
		policyKept, result.PolicyDropped = impl.FilterModelIDs(merged)
	}

	// Endpoint probe: keep a model when ANY protocol leg classifies Yes.
	kept := policyKept
	var matrix map[string]runtimewire.ModelProtocols
	probeHealthy := false
	if impl != nil {
		client := runtime.Client
		if client == nil {
			return result, fmt.Errorf("model refresh probe client is unavailable")
		}
		var drops []appapi.ModelsRefreshDrop
		kept, drops, matrix = probeRefreshModels(ctx, client, provCfg, impl, policyKept)
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.ProbeDropped = drops
		if len(policyKept) > 0 && len(kept) == 0 {
			// Every probe failed — likely auth/network, not genuinely
			// uncallable models. Keep the policy-filtered set unvalidated
			// (never wipe models:) and DO NOT replace the verdict cache
			// with the failed matrix.
			kept = policyKept
			result.Warning = "endpoint probe failed for ALL models (likely not logged in / network); list written unvalidated"
		} else {
			probeHealthy = true
		}
	}
	sort.Strings(kept)
	result.Kept = kept
	result.Added, result.Removed = diffIDs(existing, kept)
	// The JSON surface uses empty arrays, never nulls — the frontend iterates
	// these lists directly.
	if result.Kept == nil {
		result.Kept = []string{}
	}
	if result.Added == nil {
		result.Added = []string{}
	}
	if result.Removed == nil {
		result.Removed = []string{}
	}
	if result.PolicyDropped == nil {
		result.PolicyDropped = []string{}
	}
	if result.ProbeDropped == nil {
		result.ProbeDropped = []appapi.ModelsRefreshDrop{}
	}

	// Persist + hot-reload only on a net change (the CLI's skip-if-unchanged).
	if !sameIDs(existing, kept) {
		configFile := s.currentConfigFile()
		if configFile == "" {
			return result, fmt.Errorf("config file path is unavailable; cannot write the refreshed model list")
		}
		reloadWarning, err := s.writeProviderModelsAndReload(ctx, configFile, name, provCfg, kept)
		if err != nil {
			return result, err
		}
		if reloadWarning != "" {
			result.Warning = joinWarning(result.Warning, "reload applied with warning: "+reloadWarning)
		}
		result.ConfigUpdated = true
	}
	if !result.ConfigUpdated && ctx.Err() != nil {
		return result, ctx.Err()
	}
	if probeHealthy && s.ports.ModelCapsReplace != nil {
		if !s.ports.ModelCapsReplace(name, runtime.Fingerprint, matrix) {
			return result, appapi.NewHTTPError(http.StatusConflict, "provider changed during model refresh; retry")
		}
	}
	return result, nil
}

// probeRefreshModels probes each candidate id with the 3-protocol matrix on
// the provider's OWN configured bases (chat/responses on the openai base,
// anthropic only when anthropic_base_url is set) and returns the callable
// subset (input order), the dropped ids with per-leg reasons, and the full
// fresh matrix. The bounded fan-out lives in probe.ProbeModels; this function
// owns only verdict classification and the keep/drop policy.
func probeRefreshModels(ctx context.Context, client *http.Client, provCfg configdomain.Provider, impl provider.Provider, ids []string) (kept []string, dropped []appapi.ModelsRefreshDrop, matrix map[string]runtimewire.ModelProtocols) {
	outcomes := probe.ProbeModels(ctx, client, provCfg, impl, ids, refreshProbeConcurrency)
	matrix = make(map[string]runtimewire.ModelProtocols, len(outcomes))
	for _, o := range outcomes {
		mp := runtimewire.ModelProtocols{}
		for _, leg := range o.Legs {
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
		matrix[o.ID] = mp
		if mp.Chat == runtimewire.Yes || mp.Anthropic == runtimewire.Yes || mp.Responses == runtimewire.Yes {
			kept = append(kept, o.ID)
		} else {
			dropped = append(dropped, appapi.ModelsRefreshDrop{Model: o.ID, Reason: refreshLegsSummary(o.Legs)})
		}
	}
	return kept, dropped, matrix
}

// refreshLegsSummary renders the per-leg probe outcomes joined by " / ", e.g.
// "chat HTTP 404 / anthropic not probed (no base) / responses HTTP 400:
// invalid model" (the CLI drop-summary rendering).
func refreshLegsSummary(legs []probe.LegResult) string {
	parts := make([]string, 0, len(legs))
	for _, leg := range legs {
		switch {
		case !leg.Probed:
			parts = append(parts, string(leg.Leg)+" not probed (no base)")
		case leg.Err != nil:
			parts = append(parts, fmt.Sprintf("%s %v", leg.Leg, leg.Err))
		default:
			if reason := refreshLegErrorReason(leg.Body); reason != "" {
				parts = append(parts, fmt.Sprintf("%s HTTP %d: %s", leg.Leg, leg.Status, reason))
			} else {
				parts = append(parts, fmt.Sprintf("%s HTTP %d", leg.Leg, leg.Status))
			}
		}
	}
	return strings.Join(parts, " / ")
}

// refreshLegErrorReason extracts a short "code: message" from an OpenAI-style
// error body. probe's own extractor is unexported, so this keeps a local twin
// (same shape, request-id stripped via the exported helper) — mirroring the
// CLI's twin in internal/cli/models.
func refreshLegErrorReason(body []byte) string {
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

// writeProviderModelsAndReload rewrites providers.<name>.models under the
// shared config lock (comment-preserving yaml.Node round-trip, validated
// atomic write), then hot-reloads. A reload that failed to apply restores the
// backup; an applied-with-warning reload keeps the new file and reports the
// warning — the saveAndReloadUnderLock contract.
func (s *Service) writeProviderModelsAndReload(ctx context.Context, configFile, name string, expected configdomain.Provider, models []string) (reloadWarning string, err error) {
	err = configedit.WithConfigLockContext(ctx, configFile, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := configdomain.LoadConfig(configFile)
		if err != nil {
			return err
		}
		if actual, ok := current.Providers[name]; !ok || !reflect.DeepEqual(actual, expected) {
			return appapi.NewHTTPError(http.StatusConflict, "provider changed during model refresh; retry")
		}
		root, err := configedit.LoadNode(configFile)
		if err != nil {
			return err
		}
		prov := configedit.LookupChildMap(configedit.LookupChildMap(root, "providers"), name)
		if prov == nil || prov.Kind != yaml.MappingNode {
			return fmt.Errorf("provider %q not found in %s", name, configFile)
		}
		configedit.SetChildNode(prov, "models", configedit.MustEncode(models))
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(root); err != nil {
			return err
		}
		enc.Close()
		// Cancellation is honored up to this commit boundary. Once writing starts,
		// finish save + reload (or rollback), even if the client disconnects.
		if err := ctx.Err(); err != nil {
			return err
		}
		backup, err := configedit.WriteConfigValidated(configFile, buf.String(), func(path string, raw []byte) error {
			_, err := configdomain.LoadConfigFromBytes(path, raw)
			return err
		})
		if err != nil {
			return err
		}
		if err := s.ports.Reload(configFile); err != nil {
			var applied *ReloadAppliedWarning
			if errors.As(err, &applied) {
				reloadWarning = applied.Err.Error()
				return nil
			}
			if original, readErr := os.ReadFile(backup); readErr == nil {
				_ = configedit.AtomicWrite(configFile, original)
			}
			return err
		}
		return nil
	})
	return reloadWarning, err
}

// mergeIDs returns existing first, then any fetched ids not already present
// (in fetch order), deduped — the candidate set the refresh validates, so
// pre-existing ids get re-validated too.
func mergeIDs(existing, fetched []string) []string {
	seen := make(map[string]bool, len(existing)+len(fetched))
	merged := make([]string, 0, len(existing)+len(fetched))
	for _, s := range existing {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	for _, s := range fetched {
		if !seen[s] {
			seen[s] = true
			merged = append(merged, s)
		}
	}
	return merged
}

// sameIDs reports whether a and b hold the same set of strings
// (order-independent), so a no-change refresh skips the config rewrite.
func sameIDs(a, b []string) bool {
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

// diffIDs returns (inBnotA, inAnotB) — added/removed relative to a -> b, both
// sorted for stable output.
func diffIDs(a, b []string) (added, removed []string) {
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

func joinWarning(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
