package main

import (
	"bytes"
	"net/http"
)

// request_routing.go implements request-aware routing decisions that need the
// models.dev catalog (context window + modalities), loaded onto the Proxy at
// startup (see initCatalog). Three features share this file:
//
//   - context-window fallback (#9): when a request's estimated prompt size
//     exceeds the largest context window among a route's targets, reroute to a
//     configured larger-context fallback route instead of letting the upstream
//     return 400.
//   - capability routing (#8): when a request carries an image or tools, narrow a
//     route's targets to those whose capabilities accept it — per models.dev
//     metadata, overridden per-model by the provider's `capabilities:` config.
//   - context-overflow retry: when the upstream rejects a request with a
//     context-overflow 400 (the estimate said it fit; the upstream disagrees),
//     retry ONCE on a strictly-larger-context target picked cross-route.
//
// All degrade to a no-op (forward unchanged) when the catalog is unavailable or
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
// the raw request body. It's rune-aware: CJK runes count ~1 token each (not
// ~0.5 tokens like the old len/4 would give). Non-CJK bytes count at the standard
// /4 rate. Base64 image data (long alphanumeric runs >100 chars) is excluded
// entirely. One pass over the body — a few MB cost is negligible.
func estimateInputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var cjkRunes, otherBytes int64
	i := 0
	for i < len(body) {
		if isBase64Run(body, i) {
			for i < len(body) && isBase64Char(body[i]) {
				i++
			}
			continue
		}
		r, size := decodeRune(body, i)
		if isCJK(rune(r)) {
			cjkRunes++
		} else {
			otherBytes += int64(size)
		}
		i += size
	}
	return cjkRunes + otherBytes/4
}

// decodeRune decodes a single UTF-8 rune starting at body[i]. Returns the rune
// value (as int32) and the byte length. Falls back to (body[i], 1) on invalid
// UTF-8.
func decodeRune(body []byte, i int) (int32, int) {
	b := body[i]
	if b&0x80 == 0 {
		return int32(b), 1
	}
	if b&0xE0 == 0xC0 && i+1 < len(body) {
		return int32(b&0x1F)<<6 | int32(body[i+1]&0x3F), 2
	}
	if b&0xF0 == 0xE0 && i+2 < len(body) {
		return int32(b&0x0F)<<12 | int32(body[i+1]&0x3F)<<6 | int32(body[i+2]&0x3F), 3
	}
	if b&0xF8 == 0xF0 && i+3 < len(body) {
		return int32(b&0x07)<<18 | int32(body[i+1]&0x3F)<<12 | int32(body[i+2]&0x3F)<<6 | int32(body[i+3]&0x3F), 4
	}
	return int32(b), 1
}

// isCJK reports whether a rune is in a CJK Unicode block (approximates 1-token
// density per rune).
func isCJK(r rune) bool {
	return (r >= 0x4E00 && r <= 0x9FFF) || // CJK Unified Ideographs
		(r >= 0x3040 && r <= 0x30FF) || // Hiragana + Katakana
		(r >= 0xAC00 && r <= 0xD7AF) // Hangul Syllables
}

// isBase64Char reports whether b is a valid base64 character.
func isBase64Char(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') ||
		(b >= '0' && b <= '9') || b == '+' || b == '/' || b == '='
}

// isBase64Run checks if a long base64 sequence starts at body[i] (>100 chars).
func isBase64Run(body []byte, i int) bool {
	j := i
	for j < len(body) && j-i < 120 && isBase64Char(body[j]) {
		j++
	}
	return j-i >= 100
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
//
// caps is the target provider's manual capabilities override (Provider.Capabilities;
// nil when undeclared). A model DECLARED in caps is judged by that list exactly for
// image/tools ([image] = image yes, tools no) — the catalog is ignored for those
// checks. This is the escape hatch for catalog blind spots (codex/aqp/volcengine).
// Context-window checks always consult the catalog (capabilities declare no window).
func modelFits(cat *modelsDevCatalog, caps map[string][]string, model string, prof requestProfile) bool {
	if declared, ok := caps[model]; ok {
		if prof.hasImage && !hasCapability(declared, "image") {
			return false
		}
		if prof.hasTools && !hasCapability(declared, "tools") {
			return false
		}
	} else if cat != nil {
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
	}
	if prof.est > 0 {
		if m, ok := lookupModelMeta(cat, model); ok && m.Context > 0 && prof.est > m.Context {
			return false // known limit, request exceeds it
		}
	}
	return true
}

// modelFitsRequest is a convenience wrapper (profiles the body then checks fit,
// without a capabilities override).
func modelFitsRequest(cat *modelsDevCatalog, model string, body []byte) bool {
	return modelFits(cat, nil, model, profileRequest(body))
}

// hasCapability reports whether a declared capabilities list contains want.
func hasCapability(caps []string, want string) bool {
	for _, c := range caps {
		if c == want {
			return true
		}
	}
	return false
}

// targetCapabilities returns the manual capabilities override of a target's
// provider (nil when it declares none). A credential-pool virtual id
// ("name#<accountID>") resolves to its parent's config — capabilities are
// declared per provider, not per account.
func targetCapabilities(cfg *Config, parentOf map[string]string, t RouteTarget) map[string][]string {
	pconf, _ := providerConfig(cfg, parentOf, t.Provider)
	return pconf.Capabilities
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
	// In-route: keep targets that fit the request's capability + context. Fusion
	// targets always "fit" — the recipe model name isn't a catalog model, and
	// per-member fit is the engine's own concern (draft legs keep images; a
	// tool-blind synthesizer degrades to a direct answer).
	var inRoute []RouteTarget
	for _, t := range ordered {
		if t.Provider == "fusion" || modelFits(cat, targetCapabilities(cfg, parentOf, t), t.Model, prof) {
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
	result := p.crossRoutePool(cfg, parentOf, exposed+"#req", sessionKey, expanded, routeKeys, func(t RouteTarget) bool {
		return modelFits(cat, targetCapabilities(cfg, parentOf, t), t.Model, prof)
	})
	if len(result) == 0 {
		// No model anywhere fits, or all capable targets are unavailable
		// (circuit-open/rate-limited). Fall back to the ORIGINAL ordered (which
		// has available targets from the first schedule pass) — trying a
		// capability-mismatched target is better than a guaranteed zero-attempt
		// 502.
		return ordered
	}
	return result
}

// crossRoutePool collects the targets matching keep across ALL expanded routes
// (deduped by RouteTarget value), then ranks them with the standard scheduler
// under a synthetic sticky key (routeName: exposed+"#req"/"#ctx") so the
// pooled sticky entry doesn't collide with the route's own and ages out as a
// non-route key under dwell eviction. Returns nil when nothing matches or every
// match is unavailable (circuit-open/rate-limited). Shared by the proactive
// cross-route fallback and the reactive context-overflow retry.
func (p *Proxy) crossRoutePool(cfg *Config, parentOf map[string]string, routeName, sessionKey string, expanded map[string][]RouteTarget, routeKeys map[string]bool, keep func(RouteTarget) bool) []RouteTarget {
	pool := collectCrossRoute(expanded, keep)
	if len(pool) == 0 {
		return nil
	}
	return p.schedule(cfg, parentOf, routeName, sessionKey, pool, routeKeys)
}

// collectCrossRoute gathers the targets matching keep across ALL expanded routes,
// deduped by {provider, model, protocol}. Priority does NOT distinguish a backend:
// the same {provider, model, protocol} surfacing in two routes at different
// priorities is ONE call, not two (previously the dedup key was the whole
// RouteTarget value — which includes Priority — so the duplicate slipped through
// and could be called twice). On a collision the best (lowest) priority is kept.
// Fusion recipes are excluded (opt-in per route; never drag one into another
// route's fallback pool). Pure — no scheduling — so the dedup is unit-testable.
func collectCrossRoute(expanded map[string][]RouteTarget, keep func(RouteTarget) bool) []RouteTarget {
	type dedupKey struct{ provider, model, protocol string }
	best := map[dedupKey]RouteTarget{}
	for _, ts := range expanded {
		for _, t := range ts {
			if t.Provider == "fusion" || !keep(t) {
				continue
			}
			// For pooled providers t.Provider is already the virtual id, so two
			// accounts of one parent (distinct virtual ids) are NOT collapsed —
			// only a truly identical backend across routes is.
			key := dedupKey{t.Provider, t.Model, t.Protocol}
			if ex, ok := best[key]; !ok || t.Priority < ex.Priority {
				best[key] = t
			}
		}
	}
	pool := make([]RouteTarget, 0, len(best))
	for _, t := range best {
		pool = append(pool, t)
	}
	return pool
}

// contextOverflowRetry builds the replacement target list after an upstream
// rejects a request with a context-overflow 400: every target across ALL routes
// whose catalog context window is STRICTLY larger than the largest window among
// the just-tried targets AND still satisfies the request's image/tools
// capability (modelFits), ranked by the standard scheduler. The capability
// re-check is essential — a larger-context model that lacks image support would
// just fail the same request again. Returns nil without a catalog, when no tried
// model has a known window (can't establish "larger"), or when nothing larger
// AND capable exists — forward then commits the upstream 400.
func (p *Proxy) contextOverflowRetry(cfg *Config, parentOf map[string]string, cat *modelsDevCatalog, exposed, sessionKey string, tried []RouteTarget, expanded map[string][]RouteTarget, routeKeys map[string]bool, body []byte) []RouteTarget {
	if cat == nil {
		return nil
	}
	var maxContext int64
	for _, t := range tried {
		if m, ok := lookupModelMeta(cat, t.Model); ok && m.Context > maxContext {
			maxContext = m.Context
		}
	}
	if maxContext == 0 {
		return nil
	}
	prof := profileRequest(body)
	return p.crossRoutePool(cfg, parentOf, exposed+"#ctx", sessionKey, expanded, routeKeys, func(t RouteTarget) bool {
		m, ok := lookupModelMeta(cat, t.Model)
		if !ok || m.Context <= maxContext {
			return false // not strictly larger than what was already tried
		}
		// Re-check capability + context-fit so the retry doesn't land on a
		// larger window that still can't serve this request (e.g. no image).
		return modelFits(cat, targetCapabilities(cfg, parentOf, t), t.Model, prof)
	})
}

// contextOverflowMarkers are conservative case-insensitive substrings matching
// upstream "prompt exceeds the context window" error bodies across providers
// (openai's context_length_exceeded code, deepseek/zhipu "maximum context
// length" messages, anthropic "prompt is too long", ...). Deliberately specific
// so an ordinary 400 (bad key, malformed request) never matches.
var contextOverflowMarkers = [][]byte{
	[]byte("context_length_exceeded"),
	[]byte("maximum context length"),
	[]byte("context window"),
	[]byte("context length"),
	[]byte("prompt is too long"),
	[]byte("reduce the length"),
	[]byte("too many tokens"),
}

// isContextOverflow reports whether a 4xx response body looks like a
// context-window overflow error rather than an ordinary client error. status
// must be 4xx; bodyPeek is the first ≤64KiB of the body (peekResponseBody).
func isContextOverflow(status int, bodyPeek []byte) bool {
	if status < 400 || status >= 500 || len(bodyPeek) == 0 {
		return false
	}
	lower := bytes.ToLower(bodyPeek)
	for _, m := range contextOverflowMarkers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
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
