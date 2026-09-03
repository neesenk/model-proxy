package models

import (
	"bytes"
	"fmt"
	"log"
	"model-proxy/internal/display"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// Model entry as returned by the gateway's /models endpoint (OpenAI-style).
type ModelEntry struct {
	ID            string `json:"id"`
	Object        string `json:"object"`
	OwnedBy       string `json:"owned_by"`
	ContextWindow int64  `json:"context_window"`
}

// cmdModels handles:
//
//	models              — list all models from all providers (config + models.dev supplement)
//	models <provider>   — list models for one provider
//	models pull         — force-refresh the models.dev catalog cache
//	models refresh <provider> — fetch live model list from a provider's server
func CmdModels(args []string, cfg *configdomain.Config, configFile string) {
	rest := NonFlagArgs(args)
	if len(rest) > 0 && rest[0] == "pull" {
		// models pull — force-refresh the global models.dev catalog cache.
		cat, ferr := configdomain.LoadModelsCatalog(homeDir(), true)
		if ferr != nil {
			log.Fatal(ferr)
		}
		fmt.Printf("models.dev catalog refreshed: %d unique models, etag %s\n", cat.Count(), cat.ETag())
		return
	}
	if len(rest) > 0 && rest[0] == "refresh" {
		// models refresh <provider>
		if len(rest) < 2 {
			fmt.Println("usage: model-proxy models refresh <provider>")
			fmt.Println("available providers:")
			for name := range cfg.Providers {
				fmt.Printf("  %s\n", name)
			}
			return
		}
		provName := rest[1]
		if _, ok := cfg.Providers[provName]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provName, cfg.ProviderNames())
		}
		fmt.Fprintf(os.Stderr, "Refreshing models from %s...\n", provName)
		existing := cfg.Providers[provName].Models
		entries, err := FetchProviderModels(cfg, provName)
		var merged []string
		if err != nil {
			// FetchModels unavailable (the provider has no /models endpoint, is
			// not logged in, or the network is down). Fall back to route-based
			// probing: candidates = route models targeting this provider + the
			// existing config models. The endpoint probe then validates each
			// candidate with the 3-protocol matrix (chat/anthropic/responses),
			// and the callable subset (ANY leg Yes) is written back via the
			// same tail as the FetchModels path. Safety nets (all-probe-failed,
			// probe-infra-unavailable) keep config intact on a total outage,
			// so a down/not-logged-in provider never wipes models:.
			fmt.Fprintf(os.Stderr, "models endpoint unavailable for %s (%v); probing route-configured models instead\n", provName, err)
			merged = MergeStringIDs(existing, routing.RouteModelsForProvider(cfg, provName))
			if len(merged) == 0 {
				fmt.Fprintf(os.Stderr, "no models to probe for %s (no /models endpoint and no routes target it); add routes targeting %s first\n", provName, provName)
			}
		} else {
			// Merge the config's existing model list with the freshly-fetched ids
			// (existing first, then new ids in fetch order, deduped). The endpoint
			// probe below validates the merged set and writes only the callable
			// subset back - so refresh both adds newly-discovered models AND
			// removes ids no protocol leg classifies Yes (e.g. non-chat models
			// the upstream's /models lists but its endpoints reject).
			merged = MergeModelIDs(existing, entries)
		}
		ProbeAndWriteModels(cfg, provName, merged, existing, args, configFile)
		return
	}
	// models [provider] — config + models.dev supplement
	provFilter := ""
	if len(rest) > 0 {
		provFilter = rest[0]
		if _, ok := cfg.Providers[provFilter]; !ok {
			log.Fatalf("unknown provider %q; available: %s", provFilter, cfg.ProviderNames())
		}
	}
	cat, _ := configdomain.LoadModelsCatalog(homeDir(), false)
	meta, sources := routing.HydrateModels(cfg, cat)
	PrintAllModels(cfg, provFilter, meta, sources, loadModelCapsProjection(cfg))
}

// printAllModels prints all models with their hydrated metadata plus a trailing
// PROTOCOLS column (read-only projection of model_caps.json). `meta` maps
// provider→model→metadata (nil in legacy callers → names shown without ctx/out).
// `sources` drives a SRC tag: models.dev / default. `protocols` maps
// provider→model→the 3-protocol verdict matrix (nil/stale entries → "-").
func PrintAllModels(cfg *configdomain.Config, provFilter string, meta map[string]map[string]catalog.Model, sources map[string]map[string]routing.ModelSource, protocols map[string]map[string]runtimewire.ModelProtocols) {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		if provFilter != "" && n != provFilter {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("%s  %s  %s  %s  %s  %s  %s  %s\n",
		display.Dim(display.Pad("PROVIDER", 12)), display.Dim(display.Pad("MODEL ID", 22)),
		display.Dim(display.Pad("NAME", 20)), display.Dim(display.Pad("CTX", 10)),
		display.Dim(display.Pad("OUTPUT", 8)), display.Dim(display.Pad("MODALITIES", 16)),
		display.Dim(display.Pad("SRC", 10)), display.Dim(display.Pad("PROTOCOLS", 12)))
	for _, pn := range names {
		// Effective model set: hydrated metadata keys ∪ the config name list
		// (config names show even when meta is nil — e.g. legacy callers).
		idSet := map[string]bool{}
		if meta != nil && meta[pn] != nil {
			for mid := range meta[pn] {
				idSet[mid] = true
			}
		}
		for _, mid := range cfg.Providers[pn].Models {
			idSet[mid] = true
		}
		modelIDs := make([]string, 0, len(idSet))
		for mid := range idSet {
			modelIDs = append(modelIDs, mid)
		}
		sort.Strings(modelIDs)
		for _, mid := range modelIDs {
			var m catalog.Model
			if meta != nil && meta[pn] != nil {
				m = meta[pn][mid]
			}
			name := mid
			ctx := "—"
			if m.Context > 0 {
				ctx = fmt.Sprintf("%d", m.Context)
			}
			out := "—"
			if m.Output > 0 {
				out = fmt.Sprintf("%d", m.Output)
			}
			mod := "text"
			if len(m.Modalities.Input) > 0 {
				mod = strings.Join(m.Modalities.Input, "/")
			}
			src := ""
			if sources != nil {
				switch sources[pn][mid] {
				case routing.SrcModelsDev:
					src = "models.dev"
				case routing.SrcDefault:
					src = "default"
				}
			}
			mp, pok := protocols[pn][mid]
			fmt.Printf("%s  %s  %s  %s  %s  %s  %s  %s\n",
				display.Blue(display.Pad(pn, 12)), display.Cyan(display.Pad(mid, 22)),
				display.Green(display.Pad(name, 20)), display.Gray(display.Pad(ctx, 10)),
				display.Gray(display.Pad(out, 8)), display.Gray(display.Pad(mod, 16)),
				display.Gray(display.Pad(src, 10)), display.Gray(display.Pad(protocolsCell(mp, pok), 12)))
		}
	}
}

// fetchProviderModels fetches the live model list from a provider. Delegates to
// the provider's FetchModels() implementation (which lives in the provider/ layer).
func FetchProviderModels(cfg *configdomain.Config, provName string) ([]ModelEntry, error) {
	ids, err := RefreshProviderModels(cfg, provName)
	if err != nil {
		return nil, err
	}
	entries := make([]ModelEntry, 0, len(ids))
	for _, id := range ids {
		entries = append(entries, ModelEntry{ID: id, Object: "model", OwnedBy: provName})
	}
	return entries, nil
}

// probeAndWriteModels runs the shared policy-filter + endpoint-probe + display +
// write-back tail used by `models refresh`. `merged` is the candidate id list
// (already deduped with the provider's existing config ids - from FetchModels on
// the normal path, or from routes on the no-/models fallback); `existing` is the
// provider's current config models (for the change-diff + skip-write-if-unchanged).
//
// It policy-filters the candidates, probes each with the 3-protocol matrix
// (chat/anthropic/responses, keeping a model when ANY leg classifies Yes),
// prints the kept list + a drop summary, and overwrites `models:` with the
// callable subset (hot-reloading a running daemon) when it changed.
//
// Safety nets: if the probe infra is unavailable (perr != nil) or EVERY probe
// failed (allProbeFailed - likely not-logged-in / network), the candidate set is
// kept unvalidated rather than wiping `models:`; the latter still surfaces the
// failures as a warning via printFilterSummary.
func ProbeAndWriteModels(cfg *configdomain.Config, provName string, merged, existing []string, args []string, configFile string) {
	if err := probeAndWriteModels(cfg, provName, merged, existing, args, configFile, productionProbeAndWriteModelsOps()); err != nil {
		log.Fatal(err)
	}
}

// probeAndWriteModelsOps is deliberately package-private: the refresh state
// machine can be tested with deterministic policy/probe/write collaborators
// without exposing mutable production hooks or reaching a real provider.
type probeAndWriteModelsOps struct {
	filter  func(*configdomain.Config, string, []string) ([]string, []string)
	probe   func(*configdomain.Config, string, []string) ([]string, []DropReason, map[string]runtimewire.ModelProtocols, error)
	display func(*configdomain.Config, string, []string, []DropReason, map[string]runtimewire.ModelProtocols, error, bool)
	write   func(string, string, []string) error
	reload  func([]string, *configdomain.Config)
}

func productionProbeAndWriteModelsOps() probeAndWriteModelsOps {
	return probeAndWriteModelsOps{
		filter: ApplyProviderModelFilter,
		probe:  CheckProviderModels,
		display: func(cfg *configdomain.Config, provName string, policyDropped []string, dropped []DropReason, protocols map[string]runtimewire.ModelProtocols, perr error, allProbeFailed bool) {
			cat, _ := configdomain.LoadModelsCatalog(homeDir(), false)
			meta, sources := routing.HydrateModels(cfg, cat)
			// Refresh just computed the matrix — pass it directly (no file
			// round-trip; the PROTOCOLS column shows the fresh verdicts).
			byProvider := map[string]map[string]runtimewire.ModelProtocols{provName: protocols}
			PrintKeptModels(provName, cfg.Providers[provName].Models, meta, sources, byProvider)
			PrintFilterSummary(policyDropped, dropped, perr, allProbeFailed)
		},
		write:  WriteProviderModels,
		reload: maybeReloadDaemon,
	}
}

func probeAndWriteModels(cfg *configdomain.Config, provName string, merged, existing []string, args []string, configFile string, ops probeAndWriteModelsOps) error {
	// Policy filter (provider-specific static rules via the provider impl's
	// FilterModelIDs, applied to BOTH pre-existing config ids and freshly-fetched
	// ones so a stale config is cleaned up too). Currently volcengine drops
	// *-latest / doubao-seed-1-* / lite / mini by policy regardless of
	// callability. The endpoint probe below is the second, general pass
	// (callable on ANY of the provider's protocol legs?).
	policyKept, policyDropped := ops.filter(cfg, provName, merged)
	kept, dropped, protocols, perr := ops.probe(cfg, provName, policyKept)
	allProbeFailed := perr == nil && len(policyKept) > 0 && len(kept) == 0
	if perr != nil {
		// Probe infra unavailable (e.g. provider not logged in). Fall back to
		// the policy-filtered set unvalidated rather than silently dropping
		// everything.
		kept = policyKept
		dropped = nil
	} else if allProbeFailed {
		// Every model failed the probe. This usually means the provider is
		// not logged in (auth fails on every request) or the network is down
		// - not that all models are genuinely uncallable. Don't wipe config:
		// fall back to the policy-filtered set unvalidated. `dropped` is kept
		// so printFilterSummary can surface the failures as a warning.
		kept = policyKept
	}
	sort.Strings(kept)

	// Hydrate metadata (context/output/modalities) from models.dev for the
	// kept list, so the refresh table matches `model-proxy models`. Update
	// the in-memory cfg's model list first - hydrateModels keys off it.
	provCfg := cfg.Providers[provName]
	provCfg.Models = kept
	cfg.Providers[provName] = provCfg

	// Output order: final list FIRST, then the filter summary with reasons.
	ops.display(cfg, provName, policyDropped, dropped, protocols, perr, allProbeFailed)

	// Write the validated list (overwrite, not append-only). writeProviderModels
	// re-encodes the whole models: sequence, so ids absent from `kept` (both
	// pre-existing uncallable ones and freshly-fetched failures) are removed.
	if !SameStringSet(kept, existing) {
		if err := ops.write(configFile, provName, kept); err != nil {
			return fmt.Errorf("writing models to config: %w", err)
		}
		added, removed := DiffStringSets(existing, kept)
		if len(added) > 0 {
			fmt.Fprintf(os.Stderr, "config: added %d -> %v\n", len(added), added)
		}
		if len(removed) > 0 {
			fmt.Fprintf(os.Stderr, "config: removed %d -> %v\n", len(removed), removed)
		}
		// Hot-reload a running daemon so the new model set takes effect for
		// implicit routing (and refresh the display) without a manual
		// `serve reload`. No-op if no daemon is running. Mirrors login/logout.
		ops.reload(args, cfg)
	}
	return nil
}

// writeProviderModels rewrites providers.<provName>.models to `names` in
// configFile, preserving comments/order elsewhere via a yaml.Node round-trip
// (only the models sequence is re-encoded). Validates the result before writing
// and takes a best-effort .bak. The caller (cmdModels refresh) hot-reloads a
// running daemon via maybeReloadDaemon so the new names take effect for implicit
// routing without a manual `serve reload`.
func WriteProviderModels(configFile, provName string, names []string) error {
	// Locked load→mutate→write: the daemon's web config editor and preset
	// merges RMW the same config.yaml concurrently.
	return configedit.WithConfigLock(configFile, func() error {
		root, err := loadConfigNode(configFile)
		if err != nil {
			return err
		}
		p := childMap(childMap(root, "providers"), provName)
		if p == nil {
			return fmt.Errorf("provider %q not found in %s", provName, configFile)
		}
		setChildNode(p, "models", mustEncode(names))
		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(root); err != nil {
			return err
		}
		enc.Close()
		if _, err := writeConfigValidated(configFile, buf.String()); err != nil {
			return fmt.Errorf("writing config: %w", err)
		}
		return nil
	})
}

// refreshProviderModels fetches the live model list for a provider exactly once.
// If `provName` is a pooled parent (≥2 accounts in its credential pool) it uses
// the pool's first virtual by account-id order; else it uses the plain provider
// name. The model list is per-upstream, not per-account, so one fetch is correct
// and sufficient — fanning out across the pool would multiply upstream calls
// without changing the result.
//
// Returns the raw model IDs; callers that need ModelEntry wrapping (e.g. the
// `models refresh` CLI display) use fetchProviderModels, which delegates here.
func RefreshProviderModels(cfg *configdomain.Config, provName string) ([]string, error) {
	if _, ok := cfg.Providers[provName]; !ok {
		return nil, fmt.Errorf("unknown provider %q", provName)
	}
	provMap := providerbuild.BuildProviders(cfg, accountStore(), buildOpts()).Providers
	target := provName
	if vids, pooled := PoolVirtuals(cfg, provName); pooled {
		target = vids[0] // first virtual by account-id order
	}
	impl, ok := provMap[target]
	if !ok || impl == nil {
		return nil, fmt.Errorf("provider %q not available (not logged in?)", provName)
	}
	return impl.FetchModels()
}

// poolVirtuals returns the sorted virtual ids ("name#<accountID>") for a pooled
// parent and true when the provider has ≥2 accounts in its credential pool; or
// (nil, false) for a single-account / not-logged-in / unknown provider. It reads
// the pool file directly via loadPool (no Proxy required) so `models refresh`
// and `doctor` can resolve the pool without a running daemon. The returned ids
// are sorted so callers can deterministically pick the "first" virtual.
func PoolVirtuals(cfg *configdomain.Config, name string) ([]string, bool) {
	prov, ok := cfg.Providers[name]
	if !ok {
		return nil, false
	}
	pool, _ := accountStore().Load(name, prov.Provider)
	if len(pool.Accounts) < 2 {
		return nil, false
	}
	vids := make([]string, 0, len(pool.Accounts))
	for _, a := range pool.Accounts {
		vids = append(vids, name+"#"+a.ID)
	}
	sort.Strings(vids)
	return vids, true
}

// volcengineModelFilterRegexps and isVolcengineModelFiltered moved to
// provider/volcengine.go (FilterModelIDs override) - provider-specific policy
// rules live with the provider implementation, not in the main package.

// nonFlagArgs returns positional args (skipping --config and its value).
func NonFlagArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--config" || a == "-config" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--config=") {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		out = append(out, a)
	}
	return out
}
