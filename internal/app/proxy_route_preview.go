package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	runtimestate "model-proxy/internal/runtime"
	webtransport "model-proxy/internal/web"
)

// serveRoutePreview answers "where would this request go RIGHT NOW?" — the
// same early forward steps (model extraction, claude_mapping, route lookup,
// pin / force narrowing, cache probe, scheduling preview, per-target
// request-fit verdicts) with NO upstream call and NO scheduler mutation (the
// ordering comes from the detached Manager preview, same as /debug/schedule).
// POST the body exactly as the client would send it; ?proto= overrides the
// protocol (default anthropic) and the x-mp-force-provider /
// x-claude-code-session-id headers participate like in a real request. The
// cache probe keys on THIS request's headers — forward the cache-relevant
// ones (anthropic-beta, accept-language) for an exact verdict.
func (p *Proxy) serveRoutePreview(w http.ResponseWriter, r *http.Request) {
	if !webtransport.GuardBrowserOrigin(w, r) {
		return
	}
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
	if proto == "anthropic" && cfg.ClaudeMapping != nil {
		if mapped, ok := cfg.ClaudeMapping[calledModel]; ok && mapped != "" {
			exposed = mapped
		}
	}
	out["exposed"] = exposed

	targets := expanded[exposed]
	if len(targets) == 0 {
		out["route_found"] = false
		writeJSON(http.StatusOK, out)
		return
	}
	out["route_found"] = true

	force := p.pinForces(exposed, targets, parentOf)
	if force {
		if pin, ok := dash.Pins[exposed]; ok {
			out["pinned"] = pin.Provider
		}
	}
	forcedProvider := forcedProviderFromRequest(r)
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

	cacheState := "off"
	if cache != nil && forcedProvider == "" && !force {
		if _, hit := cache.Lookup(responsecache.Key(r, body), now); hit {
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
