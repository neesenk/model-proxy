package main

import (
	"bytes"
	"net/http"
)

// request_routing.go implements request-aware routing decisions that need the
// models.dev catalog (context window + modalities), loaded onto the Proxy at
// startup (see initCatalog). Two features share this file:
//
//   - context-window fallback (#9): when a request's estimated prompt size
//     exceeds the largest context window among a route's targets, reroute to a
//     configured larger-context fallback route instead of letting the upstream
//     return 400.
//   - capability routing (#8): when a request carries an image, narrow a route's
//     targets to those whose models.dev modalities accept image input.
//
// Both degrade to a no-op (forward unchanged) when the catalog is unavailable or
// the relevant config/heuristic doesn't apply — the proxy never blocks on these.

// lookupModelMeta resolves a model's metadata (context window + modalities) from
// the models.dev catalog. The catalog is keyed by canonical model name globally,
// so a provider borrowing another vendor's model still resolves. ok=false when
// the catalog is nil or the name is unknown.
func lookupModelMeta(cat *modelsDevCatalog, model string) (ProviderModel, bool) {
	if cat == nil {
		return ProviderModel{}, false
	}
	md, ok := cat.lookup(model)
	if !ok {
		return ProviderModel{}, false
	}
	return md.toProviderModel(), true
}

// estimateInputTokens returns a rough prompt-size estimate (input tokens) from
// the raw request body. Without a tokenizer we use the standard chars/4 heuristic
// on the whole JSON body — an OVER-estimate (JSON keys/structure inflate it),
// which is the safe direction for a context check (better to fall back to the
// big model than to 400). Returns 0 on an empty body.
func estimateInputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	return int64(len(body)) / 4
}

// requestProfile captures the request-derived inputs to modelFits, computed ONCE
// per request (not per target) — requestHasImage scans the full body for 7 image
// markers, so hoisting it out of the per-target loop avoids N full-body scans.
type requestProfile struct {
	hasImage bool
	hasTools bool
	est      int64
}

func profileRequest(body []byte) requestProfile {
	return requestProfile{
		hasImage: requestHasImage(body),
		hasTools: requestHasTools(body),
		est:      estimateInputTokens(body),
	}
}

// modelFits is the per-target predicate (given a precomputed request profile): a
// model fits iff it supports the request's image content (if any) and its known
// context window holds the estimated prompt. Conservative on unknowns — a model
// with no catalog entry is treated as NOT image-capable (don't route an image to
// an unknown model) but context-OK (don't block a large request on an unknown limit).
func modelFits(cat *modelsDevCatalog, model string, prof requestProfile) bool {
	if cat == nil {
		return true
	}
	if prof.hasImage {
		m, ok := lookupModelMeta(cat, model)
		if !ok || !supportsImage(m) {
			return false
		}
	}
	if prof.hasTools {
		m, ok := lookupModelMeta(cat, model)
		if !ok || !m.ToolCall {
			return false
		}
	}
	if prof.est > 0 {
		if m, ok := lookupModelMeta(cat, model); ok && m.Context > 0 && prof.est > m.Context {
			return false // known limit, request exceeds it
		}
	}
	return true
}

// modelFitsRequest is a convenience wrapper (profiles the body then checks fit).
func modelFitsRequest(cat *modelsDevCatalog, model string, body []byte) bool {
	return modelFits(cat, model, profileRequest(body))
}

// imageMarkers are the JSON content-block shapes that indicate an image payload
// across protocols (anthropic messages image blocks, openai chat image_url, openai
// responses input_image). Matched as byte substrings — cheap, and a false
// positive (text mentioning "image") only routes to an image-capable model, which
// also handles text, so the cost is nil.
var imageMarkers = [][]byte{
	[]byte(`"type":"image"`), // anthropic content block image
	[]byte(`"type": "image"`),
	[]byte(`"type":"image_url"`), // openai chat
	[]byte(`"type": "image_url"`),
	[]byte(`"type":"input_image"`), // openai responses
	[]byte(`"type": "input_image"`),
	[]byte(`"source":{"type":"base64"`), // anthropic inline image source
}

// requestHasImage reports whether the request body carries an image content block
// (heuristic substring match — see imageMarkers).
func requestHasImage(body []byte) bool {
	for _, m := range imageMarkers {
		if bytes.Contains(body, m) {
			return true
		}
	}
	return false
}

// requestHasTools reports whether the request body carries a tools definition
// (a top-level "tools" JSON array — function tools the client wants the model to
// use). Heuristic: a `"tools":[` or `"tools": [` substring. A false positive
// (text mentioning "tools") only narrows to tool-capable models (which also handle
// text), so the cost is nil.
func requestHasTools(body []byte) bool {
	return bytes.Contains(body, []byte(`"tools":[`)) ||
		bytes.Contains(body, []byte(`"tools": [`))
}

// supportsImage reports whether a model's modalities accept image input. Models
// with no recorded modalities are treated as NOT image-capable (conservative:
// don't route an image to a model we can't confirm handles them).
func supportsImage(m ProviderModel) bool {
	for _, in := range m.Modalities.Input {
		if in == "image" {
			return true
		}
	}
	return false
}

// applyRequestAwareRouting is the unified request-aware routing decision applied
// in forward after schedule(): narrow the route's targets to those that FIT the
// request (capability + context window), and if NONE in the route fit, fall back
// CROSS-ROUTE — pool every fitting target across ALL routes (deduped) and rank
// them by the standard scheduling policy (plan tier → priority asc → surplus
// desc, + health/sticky/pool). No explicit fallback config is needed; the
// scheduler picks the best-capable, best-ranked backend. No-op without a catalog
// or when every in-route target already fits (the common case).
func (p *Proxy) applyRequestAwareRouting(cfg *Config, parentOf map[string]string, cat *modelsDevCatalog, exposed, sessionKey string, ordered []RouteTarget, expanded map[string][]RouteTarget, routeKeys map[string]bool, body []byte) []RouteTarget {
	if cat == nil {
		return ordered
	}
	prof := profileRequest(body)
	// In-route: keep targets that fit the request's capability + context.
	var inRoute []RouteTarget
	for _, t := range ordered {
		if modelFits(cat, t.Model, prof) {
			inRoute = append(inRoute, t)
		}
	}
	if len(inRoute) == len(ordered) {
		return ordered // everything fits → unchanged (no cross-route needed)
	}
	if len(inRoute) > 0 {
		return inRoute // some in-route targets fit → keep them, in order
	}
	// Cross-route fallback: collect every fitting target across all routes
	// (deduped by RouteTarget value), then let the normal scheduler rank them.
	seen := map[RouteTarget]bool{}
	var pool []RouteTarget
	for _, ts := range expanded {
		for _, t := range ts {
			if seen[t] || !modelFits(cat, t.Model, prof) {
				continue
			}
			pool = append(pool, t)
			seen[t] = true
		}
	}
	if len(pool) == 0 {
		return ordered // no model anywhere fits → unchanged (let upstream respond)
	}
	// Synthetic sticky key so the fallback's sticky entry doesn't collide with the
	// route's own (and ages out as a non-route key under dwell eviction).
	result := p.schedule(cfg, parentOf, exposed+"#req", sessionKey, pool, routeKeys)
	if len(result) == 0 {
		// All capable targets are unavailable (circuit-open/rate-limited). Fall
		// back to the ORIGINAL ordered (which has available targets from the
		// first schedule pass) — trying a capability-mismatched target is better
		// than a guaranteed zero-attempt 502.
		return ordered
	}
	return result
}

// forceProvider returns the one-shot provider override for a request, from the
// x-mp-force-provider header or the force_provider query param (header wins).
// Empty = no override. Used by `model-proxy replay` to re-answer with a chosen
// backend on a single request, without a global pin.
func forceProvider(r *http.Request) string {
	if v := r.Header.Get("x-mp-force-provider"); v != "" {
		return v
	}
	return r.URL.Query().Get("force_provider")
}

// filterTargetsByProvider narrows targets to those whose provider is `provider`
// (matching the parent name for pooled virtuals via parentOf). Empty result if
// none match. Shared by the force-provider override and the manual pin logic.
func filterTargetsByProvider(targets []RouteTarget, parentOf map[string]string, provider string) []RouteTarget {
	var out []RouteTarget
	for _, t := range targets {
		if t.Provider == provider || parentOf[t.Provider] == provider {
			out = append(out, t)
		}
	}
	return out
}
