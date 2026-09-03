// proxy_read_endpoints.go — application-to-Manager health/cooldown/param-block adapter, plus the read-only /debug/route preview endpoint.
package app

import (
	"encoding/json"
	"fmt"
	"io"
	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
	"net/http"
	"strings"
	"time"
)

// quotaFreshnessMaxAge keeps every consumer of the quota-snapshot freshness
// window (scheduling skip, failure classification) on the SAME frozen source
// the quota poll ticker captured at Start — a hot quota_poll_interval change
// must not split the window from the actual polling cadence (pitfalls #29).
// The fallback covers degenerate trackers in tests.
func (p *Proxy) quotaFreshnessMaxAge(fallback *Config) time.Duration {
	if p.quota != nil {
		return p.quota.FreshnessMaxAge()
	}
	return 3 * fallback.Scheduling.PollInterval()
}

func (p *Proxy) takeHalfOpenSlot(name string, generations ...uint64) bool {
	return p.runtimeState.TakeHalfOpenSlot(
		name,
		runtimestate.GenerationArg(generations),
	)
}

func (p *Proxy) releaseHalfOpenSlot(name string, generations ...uint64) {
	p.runtimeState.ReleaseHalfOpenSlot(
		name,
		runtimestate.GenerationArg(generations),
	)
}

func (p *Proxy) recordSuccess(name, model string, generations ...uint64) {
	p.runtimeState.RecordSuccess(
		name,
		model,
		runtimestate.GenerationArg(generations),
	)
}

// recordAttemptQuality feeds a committed attempt's TTFT into the provider's
// quality EWMA (scheduling penalty signal). Only called for successful
// commits — terminal errors flow through recordFailure instead.
func (p *Proxy) recordAttemptQuality(name string, ttft time.Duration, generations ...uint64) {
	p.runtimeState.RecordAttemptQuality(
		name,
		ttft,
		runtimestate.GenerationArg(generations),
	)
}

// recordFailure increments a provider's consecutive failures and opens the
// circuit (for cooldown) once the threshold is reached. Clears any half-open slot.
func (p *Proxy) recordFailure(name string, sched Scheduling, generations ...uint64) {
	p.runtimeState.RecordFailure(
		name,
		sched.Threshold(),
		sched.Cooldown(),
		runtimestate.GenerationArg(generations),
	)
}

// modelLocked is the lock-taking variant for tryTarget's entry check.
func (p *Proxy) modelLocked(provider, model string, now time.Time) bool {
	return p.runtimeState.ModelLocked(provider, model, now)
}

// recordModelFailure locks (provider, model) for model_lockout. Model-level
// failures (404 / model-denied / empty 200) never touch the account's circuit
// breaker — the account may serve its other models fine.
func (p *Proxy) recordModelFailure(provider, model string, sched Scheduling, generations ...uint64) {
	p.runtimeState.RecordModelFailure(
		provider,
		model,
		sched.ModelLockoutDuration(),
		runtimestate.GenerationArg(generations),
	)
}

// resetHealth clears frozen runtime health state (circuit-open cooldowns,
// rate-limit cooldowns, model lockouts) so the named provider — or every
// provider when name == "" — is retried immediately instead of waiting out a
// possibly hours-long cooldown (quota-exhausted 429s). Learned param
// blocklists, sticky routes, and pins are NOT cleared (request-shape
// knowledge / routing decisions, not frozen health). A pooled parent name
// matches all its virtual accounts (same matching as pins). Returns the
// cleared provider names + the number of model locks removed.
func (p *Proxy) resetHealth(name string) (cleared []string, locks int) {
	p.mu.RLock()
	parentOf := p.parentOf
	p.mu.RUnlock()
	return p.runtimeState.ResetHealth(name, parentOf)
}

// cooldownState inspects a route's target providers' health (and quota
// exhaustion — skipped targets never earn a health entry) for the wait-retry
// decision: allDown = EVERY target is currently unavailable; allRateLimited =
// every down reason is rate-limit/quota class (the honest terminal status is
// then 429, not 502); earliest = soonest recovery across targets.
func (p *Proxy) cooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (allDown, allRateLimited bool, earliest time.Time) {
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for index, target := range targets {
		runtimeTargets[index] = runtimestate.Target{
			Provider: target.Provider,
			Model:    target.Model,
		}
	}
	return p.runtimeState.CooldownState(runtimeTargets, now, quotaMaxAge)
}

// hasRecoveredUntried reports the TOCTOU case: a target is available now but was
// NOT tried in the failed pass — its cooldown lapsed mid-pass while a sibling
// re-failed, OR every target recovered simultaneously after schedule dropped them
// all. The caller answers with an immediate zero-wait re-schedule instead of a
// terminal error. No "at least one other target still cooling" precondition: that
// made the all-recover-simultaneously case terminally fail. The round budget in
// forward (≤2 retries) bounds the loop. Quota-exhausted (skipped) targets never
// count as recovered.
func (p *Proxy) hasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for index, target := range targets {
		runtimeTargets[index] = runtimestate.Target{
			Provider: target.Provider,
			Model:    target.Model,
		}
	}
	return p.runtimeState.HasRecoveredUntried(runtimeTargets, tried, now, quotaMaxAge)
}

// learnParamBlock records an upstream-rejected top-level request parameter for
// a (provider, model); subsequent requests strip it preemptively
// (applyParamBlock). Scoped per MODEL: one model's quirk (e.g. reasoning
// models rejecting temperature) must not strip params for its siblings.
// Reports whether the parameter is newly learned (for log-once).
func (p *Proxy) learnParamBlock(provider, model, param string, generations ...uint64) (isNew bool) {
	return p.runtimeState.LearnParamBlock(
		provider,
		model,
		param,
		runtimestate.GenerationArg(generations),
	)
}

// applyParamBlock strips every learned-unsupported top-level parameter for the
// (provider, model) from the outgoing body. Best-effort: a non-JSON body (or
// one the params aren't in) passes through unchanged.
func (p *Proxy) applyParamBlock(provider, model string, body []byte) []byte {
	for _, param := range p.runtimeState.ParamBlock(provider, model) {
		if nb, did := targetexec.StripTopLevelParam(body, param); did {
			body = nb
		}
	}
	return body
}

// recordRateLimit marks a provider rate-limited until `until` (extends if later),
// records the exhaustion class for display, and clears any half-open slot. Does
// not count toward the circuit. It then triggers an async quota refresh of the
// provider so its snapshot is fresh when the rate-limit clears.
func (p *Proxy) recordRateLimit(name string, until time.Time, kind runtimestate.RateLimitKind, generations ...uint64) {
	generation := runtimestate.GenerationArg(generations)
	if !p.runtimeState.RecordRateLimit(name, until, kind, generation) {
		return
	}
	if p.quota != nil {
		// refreshAsync is tracked + stop-aware: a 429-triggered refresh can't
		// outlive Proxy.Close (no persist after the final flush).
		p.quota.RefreshAsync(name, generation)
	}
}

// serveRoutePreview answers "where would this request go RIGHT NOW?" — the
// same early forward steps (model extraction, route lookup,
// pin / force narrowing, cache probe, scheduling preview, per-target
// request-fit verdicts) with NO upstream call and NO scheduler mutation (the
// ordering comes from the detached Manager preview, same as /debug/schedule).
// POST the body exactly as the client would send it; ?proto= overrides the
// protocol (default anthropic) and the x-mp-force-provider /
// x-claude-code-session-id headers participate like in a real request. The
// cache probe maps the selected protocol to its real client endpoint and uses
// THIS request's cache-relevant headers (anthropic-beta, accept-language).
func (p *Proxy) serveRoutePreview(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(status int, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			http.Error(w, "marshal preview: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		w.Write(data)
	}
	proto := r.URL.Query().Get("proto")
	if proto == "" {
		proto = "anthropic"
	}
	if proto != "anthropic" && proto != "openai" && proto != "responses" {
		writeJSON(http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("unknown proto %q", proto)})
		return
	}

	// One config generation + one detached runtime dashboard, same pairing as
	// scheduleStatus: the ordering/health/quota/pin facts below cannot mix
	// independent reads.
	now := time.Now()
	p.mu.RLock()
	cfg := p.cfg
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	routeKeys := p.routeKeys
	cat := p.catalog
	dash := p.runtimeState.Dashboard(now)
	cache := p.cache
	guardScanner := p.guardScanner
	p.mu.RUnlock()

	maxBody := cfg.MaxRequestBodyBytesValue()
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	r.Body.Close()
	if err != nil {
		writeJSON(http.StatusBadRequest, map[string]any{"error": "read body: " + err.Error()})
		return
	}
	if int64(len(body)) > maxBody {
		writeJSON(http.StatusRequestEntityTooLarge, map[string]any{"error": "request body exceeds max_request_body_bytes"})
		return
	}

	out := map[string]any{
		"protocol": proto,
		"agent":    counters.DetectAgent(r),
	}
	calledModel := protocol.ExtractModel(body)
	out["called_model"] = calledModel
	if calledModel == "" {
		out["route_found"] = false
		out["error"] = `missing or unparseable "model" field in request body`
		writeJSON(http.StatusOK, out)
		return
	}
	exposed := calledModel
	targets := expanded[exposed]
	prefixForced := ""
	if len(targets) == 0 {
		if pref, bare, isPrefix := routing.SplitProviderPrefix(cfg.Providers, calledModel); isPrefix {
			exposed = bare
			prefixForced = pref
			targets = routing.FilterTargetsByProvider(expanded[exposed], parentOf, pref)
		}
	}
	out["exposed"] = exposed
	if prefixForced != "" {
		out["provider_prefix"] = prefixForced
	}

	if len(targets) == 0 {
		out["route_found"] = false
		writeJSON(http.StatusOK, out)
		return
	}
	out["route_found"] = true

	force := false
	if pin, ok := dash.Pins[exposed]; ok {
		for _, target := range targets {
			if target.Provider == pin.Provider || parentOf[target.Provider] == pin.Provider {
				force = true
				out["pinned"] = pin.Provider
				break
			}
		}
	}
	forcedProvider := forward.ForcedProviderFromRequest(r)
	if forcedProvider == "" {
		forcedProvider = prefixForced
	}
	if forcedProvider != "" {
		out["force_provider"] = forcedProvider
		narrowed := routing.FilterTargetsByProvider(targets, parentOf, forcedProvider)
		if len(narrowed) == 0 {
			out["ordered"] = []any{}
			out["error"] = fmt.Sprintf("force-provider %q is not a target for model %q", forcedProvider, exposed)
			writeJSON(http.StatusOK, out)
			return
		}
		targets = narrowed
	}

	// The live path applies the outbound guard before both cache lookup and
	// request-profile planning. Reuse its pure per-request decision here, but
	// deliberately omit live-only observation/session mutation.
	guardDecision := forward.EvaluateRequestGuard(cfg.Guard, guardScanner, body)
	blockKind, blockNames := guardDecision.Blocks(cfg.Guard)
	guardState := map[string]any{
		"secrets_action": cfg.Guard.SecretsAction(),
		"paths_action":   cfg.Guard.PathsAction(),
		"blocked":        blockKind != "",
	}
	if len(guardDecision.Secrets) > 0 {
		guardState["secrets"] = guardDecision.Secrets
	}
	if len(guardDecision.StrongPath) > 0 {
		guardState["strong_paths"] = guardDecision.StrongPath
	}
	if len(guardDecision.WeakPath) > 0 {
		guardState["weak_paths"] = guardDecision.WeakPath
	}
	if len(guardDecision.Secrets) > 0 && cfg.Guard.SecretsAction() == "redact" {
		guardState["body_redacted"] = true
	}
	out["guard"] = guardState
	body = guardDecision.ForwardBody
	if blockKind != "" {
		out["cache"] = "bypass (guard block)"
		out["ordered"] = []any{}
		out["error"] = fmt.Sprintf("guard would block %s: %s", blockKind, strings.Join(blockNames, ", "))
		writeJSON(http.StatusOK, out)
		return
	}

	cacheState := "off"
	if cache != nil && forcedProvider == "" && !force {
		// Peek, not Lookup: this endpoint is a read-only preview that gets
		// polled, and Lookup books a miss (and lazily evicts) on every probe,
		// grinding the operational hit-rate metrics down.
		cacheRequest := routePreviewCacheRequest(r, proto)
		if cache.Peek(responsecache.Key(cacheRequest, body), now) {
			cacheState = "hit"
		} else {
			cacheState = "miss"
		}
	} else if cache != nil {
		cacheState = "bypass (pin/force)"
	}
	out["cache"] = cacheState

	sessionKey := r.Header.Get("x-claude-code-session-id")
	if sessionKey != "" {
		out["session_key"] = sessionKey
	}
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for i, t := range targets {
		pconf, _ := configdomain.ProviderConfig(cfg, parentOf, t.Provider)
		runtimeTargets[i] = runtimestate.Target{
			Provider:        t.Provider,
			Parent:          parentOf[t.Provider],
			Model:           t.Model,
			Priority:        t.Priority,
			BillingOverride: configuredBillingOverride(pconf.Billing),
			PeakMultiplier:  pconf.PeakMultiplier(now),
		}
	}
	decision := dash.PreviewOrder(runtimestate.ScheduleInput{
		Exposed:           exposed,
		SessionKey:        sessionKey,
		Targets:           runtimeTargets,
		RouteKeys:         routeKeys,
		Dwell:             cfg.Scheduling.Dwell(),
		SwitchMargin:      cfg.Scheduling.SwitchMargin(),
		Now:               now,
		QuotaMaxAge:       3 * cfg.Scheduling.PollInterval(),
		QualityErrWeight:  cfg.Scheduling.QualityErrorWeightValue(),
		QualityTTFTWeight: cfg.Scheduling.QualityTTFTWeightValue(),
		Generation:        dash.Generation,
	})

	// Per-target fit verdicts against the request profile — the same facts
	// planner.Apply uses to narrow the route.
	profile := routing.Profile{}
	profileApplies := cat != nil && !force && forcedProvider == ""
	if profileApplies {
		profile = routing.ProfileRequest(body)
		out["request_profile"] = map[string]any{
			"estimated_tokens": profile.EstimatedTokens,
			"has_image":        profile.HasImage,
			"has_tools":        profile.HasTools,
		}
	}
	type previewTarget struct {
		Provider       string  `json:"provider"`
		Model          string  `json:"model"`
		Priority       int     `json:"priority"`
		Tier           string  `json:"tier"`
		Surplus        float64 `json:"surplus"`
		QualityPenalty float64 `json:"quality_penalty"`
		Available      bool    `json:"available"`
		Fits           bool    `json:"fits"`
		FitReason      string  `json:"fit_reason,omitempty"`
	}
	ordered := make([]previewTarget, 0, len(decision.Order))
	avail := func(name string) bool {
		state, ok := dash.Providers[name]
		return !ok || state.Available
	}
	for _, index := range decision.Order {
		t := targets[index]
		pt := previewTarget{
			Provider:       t.Provider,
			Model:          t.Model,
			Priority:       t.Priority,
			Tier:           billingClassName(decision.Facts[index].Billing),
			Surplus:        decision.Facts[index].Surplus,
			QualityPenalty: decision.Facts[index].QualityPenalty,
			Available:      avail(t.Provider),
			Fits:           true,
		}
		if profileApplies {
			pt.Fits, pt.FitReason = routing.FitVerdict(
				cat,
				routing.CapabilitiesFor(cfg, parentOf, t),
				t.Model,
				profile,
			)
		}
		ordered = append(ordered, pt)
	}
	out["ordered"] = ordered
	if cur := dash.Sticky[exposed]; cur.Provider != "" {
		out["sticky"] = cur.Provider
	}
	writeJSON(http.StatusOK, out)
}

// routePreviewCacheRequest translates the diagnostic endpoint into the actual
// inbound protocol endpoint used by forward. /debug/route's own ?proto query
// is control metadata, not part of a real cache key.
func routePreviewCacheRequest(preview *http.Request, proto string) *http.Request {
	request := preview.Clone(preview.Context())
	request.Method = http.MethodPost
	request.URL.RawPath = ""
	request.URL.RawQuery = ""
	switch proto {
	case "openai":
		request.URL.Path = "/v1/chat/completions"
	case "responses":
		request.URL.Path = "/v1/responses"
	default:
		request.URL.Path = "/v1/messages"
	}
	return request
}
