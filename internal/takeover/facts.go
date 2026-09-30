package takeover

import (
	"fmt"
	"sort"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	"model-proxy/internal/runtime/wirecap"
)

// DisabledModelsForHome loads the operator disabled-model override the
// daemon persists (disabled_models.json under <home>/.model-proxy) — the
// offline CLI consumer's source of the live set (the composition root
// rewrites the file after every toggle, so disk mirrors memory). The WebUI
// path does NOT use this: its Service passes the live port projection
// instead. A missing file is no override; a malformed file degrades to
// unfiltered — a takeover must not fail over bad state. The degradation is
// NOT silent: ModelFactsFor re-checks the store on the no-override path and
// records DisabledStoreWarning, which surfaces in TakeoverReport.Warnings
// and the stderr warnings of the run.
func DisabledModelsForHome(homeDir string) map[string][]string {
	entries, err := wirecap.LoadDisabledModelsFile(wirecap.DisabledModelsPathForHome(homeDir))
	if err != nil {
		return nil
	}
	return entries
}

// PruneDisabledRoutes drops routes whose EVERY target is operator-disabled
// (the disabled_models.json override): a fully-disabled route is hidden from
// /v1/models and cannot be scheduled, so offering it as a takeover model or
// writing it into a client config hands the agent a dead entry. A route with
// ANY live target survives (partially disabled multi-provider models still
// fail over to the rest); an empty target list is never "fully disabled"
// (mirrors ChatReachableRoutes' abstain rule). Dropped exposed names are
// returned sorted for the run log. A nil/empty override returns routes
// unchanged.
func PruneDisabledRoutes(routes map[string][]configdomain.RouteTarget, disabled map[string][]string) (map[string][]configdomain.RouteTarget, []string) {
	if len(disabled) == 0 {
		return routes, nil
	}
	set := make(map[string]map[string]bool, len(disabled))
	for provider, models := range disabled {
		m := make(map[string]bool, len(models))
		for _, model := range models {
			m[model] = true
		}
		set[provider] = m
	}
	kept := make(map[string][]configdomain.RouteTarget, len(routes))
	var dropped []string
	for exposed, targets := range routes {
		live := false
		for _, t := range targets {
			if !set[t.Provider][t.Model] {
				live = true
				break
			}
		}
		if live || len(targets) == 0 {
			kept[exposed] = targets
		} else {
			dropped = append(dropped, exposed)
		}
	}
	sort.Strings(dropped)
	return kept, dropped
}

// AuthenticatedProviders projects which config-level providers can
// authenticate right now (providerbuild's offline twin of the effective
// routing table's authNotReady/expandTarget filter — same BuildProviders
// pass + AuthReady reads the reload path uses, pool virtuals folded to
// parents, booleans only). Exposed here so the admin surface can share the
// exact set a run would prune by without importing providerbuild.
func AuthenticatedProviders(cfg *configdomain.Config, homeDir string) map[string]bool {
	return providerbuild.AuthenticatedProvidersForHome(cfg, homeDir)
}

// PruneUnauthenticatedRoutes drops routes whose EVERY target sits on a
// provider that cannot authenticate (not logged in — the same projection
// that keeps such models out of /v1/models and forward). A route with any
// authenticated target survives (failover keeps it servable); "fusion"
// targets pass without an auth check, mirroring expandTarget. ABSTAINS
// (routes returned untouched) when the authenticated set is empty: a fresh
// setup mid-`config init` has no logins yet and must still write model
// lists — login plus a re-takeover fills them in. Dropped exposed names are
// returned sorted for the run log.
func PruneUnauthenticatedRoutes(routes map[string][]configdomain.RouteTarget, authed map[string]bool) (map[string][]configdomain.RouteTarget, []string) {
	if len(authed) == 0 {
		return routes, nil
	}
	kept := make(map[string][]configdomain.RouteTarget, len(routes))
	var dropped []string
	for exposed, targets := range routes {
		live := false
		for _, t := range targets {
			if t.Provider == configdomain.FusionProvider || authed[t.Provider] {
				live = true
				break
			}
		}
		if live || len(targets) == 0 {
			kept[exposed] = targets
		} else {
			dropped = append(dropped, exposed)
		}
	}
	sort.Strings(dropped)
	return kept, dropped
}

// ModelFactsFor computes the route table (derived from provider model lists,
// explicit routes overriding) and hydrated models.dev metadata for
// RunTakeover. homeDir locates
// the models.dev catalog cache (callers pass the CLI's HOME seam so tests can
// isolate it). disabled is the operator disabled-model override — the live
// port projection on the daemon path, DisabledModelsForHome(homeDir) on the
// CLI path: routes whose every target it disables are dropped so takeover
// offers exactly the model set the proxy can actually serve. The same holds
// for providers that cannot authenticate (not logged in — computed from
// homeDir's credential stores via providerbuild, the reload path's own
// signals; ModelFactsForAuthed lets a caller inject the set instead): a
// route with no authenticated target is dropped unless NO
// provider is logged in at all (fresh-setup abstain — see
// PruneUnauthenticatedRoutes).
//
// The route table is chat-reachable only (routing.ChatReachableRoutes):
// models no target can serve over anthropic|openai|responses (decisions-only
// providers, e.g. typesafe's jev) are dropped from Routes and listed in
// Unreachable — writing them into a client config hands the agent an entry
// that can only ever fail (the decisions protocol has no chat conversion).
func ModelFactsFor(cfg *configdomain.Config, which, homeDir, templatesDir string, mode ResolveMode, disabled map[string][]string) ModelFacts {
	return ModelFactsForAuthed(cfg, which, homeDir, templatesDir, mode, disabled, AuthenticatedProviders(cfg, homeDir))
}

// ModelFactsForAuthed is ModelFactsFor with the authenticated-provider set
// supplied by the caller instead of recomputed from homeDir's credential
// stores. The daemon's admin surface resolves the set from a per-config-
// generation cache (the takeover read/preview endpoints answer every
// request, and the offline BuildProviders pass behind AuthenticatedProviders
// is far too expensive to rerun per preview); CLI callers keep the direct
// compute via ModelFactsFor. An empty authed set keeps the fresh-setup
// abstain (see PruneUnauthenticatedRoutes).
func ModelFactsForAuthed(cfg *configdomain.Config, which, homeDir, templatesDir string, mode ResolveMode, disabled map[string][]string, authed map[string]bool) ModelFacts {
	routes, unreachable := routing.ChatReachableRoutes(cfg, routing.RouteTable(cfg))
	routes, disabledDropped := PruneDisabledRoutes(routes, disabled)
	routes, loggedOut := PruneUnauthenticatedRoutes(routes, authed)
	facts := ModelFacts{
		Routes:        routes,
		Unreachable:   unreachable,
		Disabled:      disabledDropped,
		NotLoggedIn:   loggedOut,
		SourceDefault: -1,
	}
	// A corrupt disabled-model store degrades fail-open (nil override =
	// unfiltered — see DisabledModelsForHome), which used to be invisible to
	// the operator. Surface it in the report warnings channel: re-check the
	// store whenever no override was supplied (a present override proves the
	// store loaded, or that the daemon's in-memory set is authoritative).
	if len(disabled) == 0 {
		if _, err := wirecap.LoadDisabledModelsFile(wirecap.DisabledModelsPathForHome(homeDir)); err != nil {
			facts.DisabledStoreWarning = fmt.Sprintf("disabled models store unreadable: %v; continuing unfiltered (operator-disabled models may be written to client configs)", err)
		}
	}
	// Metadata is hydrated unconditionally, not just for metadata-writing
	// clients (opencode/pi/kimi): the single-model default placeholders
	// ({{model.primary}}/{{model.fast}}, e.g. claude's default-model envs)
	// also rank models by context window. A missing catalog cache just
	// degrades those to name order; MetadataWarnings keeps its own
	// WritesMetadata gate, so warnings are unaffected.
	cat, _ := configdomain.LoadModelsCatalog(homeDir, false)
	meta, sources := routing.HydrateModels(cfg, cat)
	facts.Meta = meta
	facts.Sources = make(map[string]map[string]int, len(sources))
	for provider, models := range sources {
		facts.Sources[provider] = make(map[string]int, len(models))
		for model, source := range models {
			facts.Sources[provider][model] = int(source)
		}
	}
	facts.SourceDefault = int(routing.SrcDefault)
	facts.DefaultContext = routing.DefaultModelMetadata.Context
	facts.DefaultOutput = routing.DefaultModelMetadata.Output
	return facts
}
