// wirecap.go — wire capability probing: the proxy actively probes each
// provider's openai_base_url for /responses support (and /v1/messages ONLY
// when the provider has no anthropic_base_url — then the probe URL is the
// exact URL an anthropic passthrough would hit; with a dedicated anthropic
// base the matrix short-circuits and /v1/messages is never fabricated on the
// openai base), and uses
// the verdict as the DEFAULT backend protocol when a route target declares no
// explicit `protocol:` and no ProtocolHint applies (explicit protocol: remains
// the top-priority escape hatch). See docs/architecture/routing-and-failure.md.
//
// Rationale: openai_base_url contractually serves BOTH /chat/completions and
// /responses (internal/config), but many third-party endpoints implement only chat.
// Without a verdict, an anthropic client defaults to byte-level passthrough of
// an anthropic body to an openai base — right only for gateways that accept
// anthropic. A probe verdict lets the proxy convert instead (responses first —
// the responses-direction converters preserve reasoning/usage details — then
// chat), with zero user configuration.
//
// Lifecycle: probed once asynchronously at boot (production NewProxy only —
// the injectable constructor used by tests leaves probing off so mock
// upstreams don't see surprise probe hits; tests call probeAllWireCaps
// synchronously) and after each reload for providers whose verdict is missing
// or whose base_url changed. A "yes" verdict is trusted indefinitely (a wrong
// yes is corrected at runtime by the 404 path — tryTarget →
// noteWireResponsesMiss); a "no" verdict expires after wireCapNegativeTTL so
// one transient 404 (endpoint mid-deploy, gateway route gap) cannot downgrade
// a provider forever — the next boot/reload pass re-probes it.
package app

import (
	"model-proxy/internal/observe/logx"
	"net/http"
	"sync"
	"time"

	"model-proxy/internal/provider"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// Compatibility aliases keep the application adapter readable while the
// verdict, JSON contract, policy, and concurrent Store live in the runtime
// wire-capability package.
type triState = runtimewire.Verdict

const (
	triUnknown = runtimewire.Unknown
	triYes     = runtimewire.Yes
	triNo      = runtimewire.No
)

type wireCaps = runtimewire.Capabilities

const (
	// wireCapProbeConcurrency bounds concurrent probe requests across
	// providers — probes are real upstream calls; don't burst at boot.
	wireCapProbeConcurrency = 4
	// wireCapNegativeTTL bounds how long a "no" verdict is trusted before the
	// next probe pass re-checks it. Endpoint capabilities rarely change, but a
	// negative conclusion can be wrong (transient 404) and has no runtime
	// correction path, unlike a wrong "yes" (404 → noteWireResponsesMiss).
	wireCapNegativeTTL = 24 * time.Hour
)

// wireLegFresh reports whether a concluded verdict is still trusted: a "yes"
// indefinitely, a "no" only within wireCapNegativeTTL.
func wireLegFresh(v triState, probedAt time.Time) bool {
	return runtimewire.LegFresh(v, probedAt, time.Now(), wireCapNegativeTTL)
}

// wireCapProbeTimeout caps one probe request. A var (not const) so tests can
// shrink it for the timeout branch.
var wireCapProbeTimeout = 10 * time.Second

// classifyWireStatus maps a probe outcome to a verdict. 404 → no (the proxy
// only probes known LLM paths, so a 404 means the route doesn't exist).
// 2xx/400/401/403/429 → yes: the endpoint exists (a 400 is a shape dispute,
// not a missing route; auth/quota answers prove the route exists). Timeouts,
// connection errors and 5xx → unknown: no negative conclusion is cached, so
// the next boot re-probes.
func classifyWireStatus(status int, err error) triState {
	return runtimewire.ClassifyStatus(status, err)
}

// wireProbeBodies delegates to runtimewire.ProbeBodies.
func wireProbeBodies(model string) (responsesBody, anthropicBody []byte) {
	return runtimewire.ProbeBodies(model)
}

// wireProbe delegates to runtimewire.Probe.
func wireProbe(client *http.Client, prov Provider, impl provider.Provider, path string, body []byte) (int, error) {
	return runtimewire.Probe(client, prov, impl, path, body)
}

// wireProbeModel delegates to runtimewire.ProbeModel.
func wireProbeModel(cfg *Config, implicit map[string]RouteTarget, provName string) string {
	return runtimewire.ProbeModel(cfg, implicit, provName)
}

// probeAllWireCaps probes every eligible provider once (skipping providers
// with a fresh verdict) and persists the results. Eligible: has an
// openai_base_url AND no ProtocolHint (codex is already hint-covered — its
// protocol is known without probing). Runs synchronously; callers dispatch it
// on a tracked goroutine (boot/reload) or invoke it directly (tests).
func (p *Proxy) probeAllWireCaps() {
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
	poolIndex := p.poolIndex
	implicit := p.implicitRoutes
	p.mu.RUnlock()

	client := &http.Client{Timeout: wireCapProbeTimeout}
	sem := make(chan struct{}, wireCapProbeConcurrency)
	var wg sync.WaitGroup
	probed := false
	for name, provCfg := range cfg.Providers {
		if provCfg.OpenAIBaseURL == "" {
			continue
		}
		if provider.ProtocolHint(provCfg.Provider, "") != "" {
			continue // protocol already known via hint (codex → responses)
		}
		// Skip providers with a fresh verdict: same base_url and every PROBED
		// capability concluded and still trusted (unknown or expired negative
		// legs are re-probed — no negative conclusion is final). With
		// anthropic_base_url set the anthropic leg is never probed (the
		// decision matrix short-circuits to the dedicated base), so only
		// responses counts for freshness.
		if cur, ok := p.wireVerdict(name); ok && cur.BaseURL == provCfg.OpenAIBaseURL &&
			wireLegFresh(cur.Responses, cur.ProbedAt) &&
			(provCfg.AnthropicBaseURL != "" || wireLegFresh(cur.Anthropic, cur.ProbedAt)) {
			continue
		}
		// Resolve the implementation like providerImplFor, but from the live
		// maps (pooled parent → first virtual's impl).
		impl := provs[name]
		if impl == nil {
			if vids := poolIndex[name]; len(vids) > 0 {
				impl = provs[vids[0]]
			}
		}
		if impl == nil {
			continue // not logged in / not built — nothing to probe with
		}
		model := wireProbeModel(cfg, implicit, name)
		probed = true
		wg.Add(1)
		go func(name string, provCfg Provider, impl provider.Provider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			responsesBody, anthropicBody := wireProbeBodies(model)
			rs, rErr := wireProbe(client, provCfg, impl, "/responses", responsesBody)
			// Anthropic leg: only probed on the OPENAI base when the provider
			// has no dedicated anthropic_base_url — the probe URL is then the
			// exact URL forward would passthrough to. With anthropic_base_url
			// set, the matrix short-circuits to it and /v1/messages must NOT
			// be fabricated on the openai base (e.g. zhipu's /api/paas/v4).
			anth := triUnknown
			if provCfg.AnthropicBaseURL == "" {
				as, aErr := wireProbe(client, provCfg, impl, "/v1/messages", anthropicBody)
				anth = classifyWireStatus(as, aErr)
			}
			caps := wireCaps{
				BaseURL:   provCfg.OpenAIBaseURL,
				Responses: classifyWireStatus(rs, rErr),
				Anthropic: anth,
				ProbedAt:  time.Now(),
			}
			p.setWireCaps(name, caps)
			logx.Debugf("[wirecap] provider %s probed: responses=%s anthropic=%s", name, caps.Responses, caps.Anthropic)
		}(name, provCfg, impl)
	}
	wg.Wait()
	if probed {
		p.persistWireCaps()
	}
}

// wireVerdict returns the stored verdict for a provider (parent name).
func (p *Proxy) wireVerdict(name string) (wireCaps, bool) {
	return p.wireCaps.Get(name)
}

// setWireCaps stores a verdict (parent name).
func (p *Proxy) setWireCaps(name string, caps wireCaps) {
	p.wireCaps.Put(name, caps)
}

// wireCapsSnapshot returns a copy of the verdict map for persistence.
func (p *Proxy) wireCapsSnapshot() map[string]wireCaps {
	return p.wireCaps.Snapshot()
}

// persistWireCaps triggers an ASYNC persist (never synchronous from the
// request path: persist takes p.mu.RLock via fullSnapshot, and a handler
// already holds it — a synchronous call could deadlock behind a pending
// reload writer). The goroutine is quota-tracked so Close drains it.
func (p *Proxy) persistWireCaps() {
	if p.quota == nil {
		return
	}
	p.quota.Launch(func() {
		if p.quota.Stopped() {
			return
		}
		if err := p.quota.Persist(); err != nil {
			logx.Warnf("[wirecap] persist failed: %v", err)
		}
	})
}

// noteWireResponsesMiss flips a provider's responses verdict to no after a
// verdict-driven /responses request came back 404 — the probe said yes but
// the route doesn't exist (stale verdict, or a per-model gateway). This is
// OUR protocol-choice miss, not a missing model, so tryTarget skips
// recordModelFailure for it; subsequent requests fall back to chat.
//
// Verdicts are keyed by parent name; a pool virtual shares the parent's base
// URL. `parent` is the ALREADY-RESOLVED parent name: callers project it from
// their request snapshot (RuntimeSnapshot.ParentOf, nil-safe) so a pre-reload
// in-flight request records the verdict under ITS generation's parent instead
// of re-reading reload-owned state here (single-snapshot red line).
func (p *Proxy) noteWireResponsesMiss(parent string) {
	p.wireCaps.MarkResponsesUnsupported(parent, time.Now())
	p.persistWireCaps()
}

// startWireCapProbe dispatches a one-shot probe pass on a quota-tracked,
// stop-aware goroutine. No-op unless production probing is enabled
// (p.wireProbe — set by NewProxy; the injectable test constructor leaves it
// off so mock upstreams see no surprise probe traffic).
func (p *Proxy) startWireCapProbe() {
	if p.quota == nil || !p.wireProbe {
		return
	}
	p.quota.Launch(func() {
		if p.quota.Stopped() {
			return
		}
		p.probeAllWireCaps()
	})
}

// resolveByWire is the pure wire-verdict decision matrix, applied when neither
// an explicit protocol: nor a ProtocolHint decided. Returns the backend
// protocol and whether the choice was a VERDICT-DRIVEN switch to responses
// (the only case the runtime 404 correction rewinds).
//
//	anthropic client + provider has anthropic_base_url → passthrough (unchanged)
//	anthropic client + verdict.anthropic == yes        → passthrough (gateway accepts anthropic)
//	anthropic client + verdict.responses == yes        → convert to responses (reasoning preserved)
//	anthropic client + verdict.responses == no (and anthropic ≠ yes) → convert to chat
//	  (chat is openai_base_url's definitional protocol; this covers both "both no"
//	  and the post-correction state responses=no / anthropic unknown)
//	anthropic client + verdict otherwise unknown       → passthrough (status quo while probing)
//	responses client + verdict.responses == no         → convert to chat
//	responses client + otherwise                       → passthrough (unchanged)
//	chat client                                        → passthrough (unchanged)
func resolveByWire(clientProto string, hasAnthropicBase bool, caps wireCaps, ok bool) (proto string, viaResponsesVerdict bool) {
	return runtimewire.Resolve(clientProto, hasAnthropicBase, caps, ok)
}

// resolvedBackendProto determines the backend protocol for a route target (or
// a fusion/shadow leg): the declared `protocol:`, else the provider's
// ProtocolHint (codex→responses), else the wire probe verdict, else the
// client's protocol (byte-level passthrough). provName may be a pool virtual
// ("name#<id>") — the verdict is looked up by parent name via the caller's
// parentOf snapshot (grabbed under p.mu earlier; passing it in avoids taking
// p.mu here, which callers may or may not hold).
func (p *Proxy) resolvedBackendProto(declared, provName string, provCfg Provider, model, clientProto string, parentOf map[string]string) (proto string, viaResponsesVerdict bool) {
	if declared != "" {
		return declared, false
	}
	if hint := provider.ProtocolHint(provCfg.Provider, model); hint != "" {
		return hint, false
	}
	parent := provName
	if par, ok := parentOf[provName]; ok {
		parent = par
	}
	caps, ok := p.wireVerdict(parent)
	return resolveByWire(clientProto, provCfg.AnthropicBaseURL != "", caps, ok)
}
