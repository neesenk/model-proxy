// wirecap.go — wire capability probing: the proxy actively probes each
// provider's openai_base_url for /chat/completions and /responses support and
// uses the verdict as the DEFAULT backend protocol when a route target
// declares no explicit `protocol:` and no ProtocolHint applies (explicit
// protocol: remains the top-priority escape hatch). Anthropic support is NOT
// probed: a provider declares it by configuring anthropic_base_url.
// See docs/architecture/routing-and-failure.md.
//
// Probes are agent-grade: every provider-level leg carries a function-tool
// declaration and goes through the same pipeline as model-level probes
// (probe.ProbeProviderOpenAILegs), so a "yes" means callable WITH tools — a
// gateway that answers a bare ping but rejects function tools cannot produce
// a false positive here either. Verdict entries carry a probe-semantics
// version (runtimewire.ProbeVersion); entries persisted under older probe
// semantics are discarded on restore and re-probed.
//
// Rationale: openai_base_url contractually serves BOTH /chat/completions and
// /responses (internal/config), but many third-party endpoints implement only
// chat. Without a verdict, an anthropic client defaults to byte-level
// passthrough of an anthropic body to an openai base — usually wrong. A probe
// verdict lets the proxy convert instead (responses first — the
// responses-direction converters preserve reasoning/usage details — then
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
	"context"
	"model-proxy/internal/observe/logx"
	"net/http"
	"sync"
	"time"

	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/upstreamproxy"
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

// classifyProviderWireStatus is classifyWireStatus with the agent-grade 400
// rule: provider legs are probed with a function tool attached, and a 400
// carrying a model/tool rejection wording ("Function tools ... are not
// supported for <model> in <path>") means the leg is not callable for
// agentic traffic — No, not the generic "shape dispute proves the route"
// yes. See runtimewire.ClassifyProviderStatus.
func classifyProviderWireStatus(status int, err error, body []byte) triState {
	return runtimewire.ClassifyProviderStatus(status, err, body)
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
	derived := p.derivedRoutes
	p.mu.RUnlock()

	client := &http.Client{Timeout: wireCapProbeTimeout, Transport: upstreamproxy.AutoTransport()}
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
		// Skip providers with a fresh verdict: same base_url and both PROBED
		// legs concluded and still trusted (unknown or expired negative legs
		// are re-probed — no negative conclusion is final).
		if cur, ok := p.wireVerdict(name); ok && cur.BaseURL == provCfg.OpenAIBaseURL &&
			wireLegFresh(cur.Chat, cur.ProbedAt) &&
			wireLegFresh(cur.Responses, cur.ProbedAt) {
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
		model := probe.PickModel(cfg, derived, name)
		probed = true
		wg.Add(1)
		go func(name string, provCfg Provider, impl provider.Provider) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			chatLeg, responsesLeg := probe.ProbeProviderOpenAILegs(context.Background(), client, provCfg, impl, model)
			caps := wireCaps{
				BaseURL:      provCfg.OpenAIBaseURL,
				Chat:         classifyProviderWireStatus(chatLeg.Status, chatLeg.Err, chatLeg.Body),
				Responses:    classifyProviderWireStatus(responsesLeg.Status, responsesLeg.Err, responsesLeg.Body),
				ProbedAt:     time.Now(),
				ProbeVersion: runtimewire.ProbeVersion,
			}
			p.setWireCaps(name, caps)
			logx.Debugf("[wirecap] provider %s probed: chat=%s responses=%s", name, caps.Chat, caps.Responses)
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

// noteWireResponsesMiss records that a verdict-driven /responses request came
// back 404 — the probe said yes but the route doesn't exist for THIS model
// (stale verdict, or a per-model gateway). With a model id the model-level
// verdict is flipped; without one (passthrough target) the provider-level
// verdict is. This is OUR protocol-choice miss, not a missing model, so
// tryTarget skips recordModelFailure for it; subsequent requests fall back to
// chat.
//
// Verdicts are keyed by parent name; a pool virtual shares the parent's base
// URL. `parent` is the ALREADY-RESOLVED parent name: callers project it from
// their request snapshot (RuntimeSnapshot.ParentOf, nil-safe) so a pre-reload
// in-flight request records the verdict under ITS generation's parent instead
// of re-reading reload-owned state here (single-snapshot red line).
func (p *Proxy) noteWireResponsesMiss(parent, model string) {
	if model != "" {
		// Flip the MODEL-level verdict when the model has an entry — the 404
		// proves this model can't do /responses regardless of what the
		// provider-level verdict says (more precise than poisoning the whole
		// provider). Without a model entry the choice was provider-driven, so
		// the provider-level verdict is the one to correct.
		if _, ok := p.modelCaps.Get(parent, model); ok {
			p.modelCaps.MarkResponsesUnsupported(parent, model, time.Now())
			p.persistModelCaps()
			return
		}
	}
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
		p.probeAllModelCaps()
	})
}

// resolveByWire is the pure wire-verdict decision matrix, applied when neither
// an explicit protocol: nor a ProtocolHint decided. Returns the backend
// protocol and whether the choice was a VERDICT-DRIVEN switch to responses
// (the only case the runtime 404 correction rewinds).
func resolveByWire(clientProto string, hasAnthropicBase bool, caps wireCaps, ok bool) (proto string, viaResponsesVerdict bool) {
	return runtimewire.Resolve(clientProto, hasAnthropicBase, caps, ok)
}

// resolvedBackendProto determines the backend protocol for a route target (or
// a fusion/shadow leg): the declared `protocol:`, else the provider's
// ProtocolHint (codex→responses), else the MODEL-level capability verdict,
// else the provider-level wire probe verdict, else the client's protocol
// (byte-level passthrough). provName may be a pool virtual ("name#<id>") —
// verdicts are looked up by parent name via the caller's parentOf snapshot
// (grabbed under p.mu earlier; passing it in avoids taking p.mu here, which
// callers may or may not hold). model is the UPSTREAM model id; an empty model
// (passthrough target) skips the model-level lookup.
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
	var mc runtimewire.ModelProtocols
	mok := false
	if model != "" {
		mc, mok = p.modelCaps.Get(parent, model)
	}
	caps, cok := p.wireVerdict(parent)
	return runtimewire.ResolveModel(clientProto, provCfg.AnthropicBaseURL != "", mc, mok, caps, cok)
}
