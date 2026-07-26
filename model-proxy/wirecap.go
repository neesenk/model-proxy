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
// /responses (config.go), but many third-party endpoints implement only chat.
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
// or whose base_url changed. No periodic re-probe: endpoint capabilities
// rarely change, and a wrong "yes" is corrected at runtime by the 404 path
// (tryTarget → noteWireResponsesMiss).
package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"model-proxy/provider"
)

// triState is a three-valued probe verdict: unknown (not probed / probe
// inconclusive), yes (endpoint exists), no (endpoint does not exist).
type triState int

const (
	triUnknown triState = iota
	triYes
	triNo
)

func (s triState) String() string {
	switch s {
	case triYes:
		return "yes"
	case triNo:
		return "no"
	default:
		return "unknown"
	}
}

func parseTriState(s string) triState {
	switch s {
	case "yes":
		return triYes
	case "no":
		return triNo
	default:
		return triUnknown
	}
}

func (s triState) MarshalJSON() ([]byte, error) { return []byte(`"` + s.String() + `"`), nil }

func (s *triState) UnmarshalJSON(b []byte) error {
	*s = parseTriState(strings.Trim(string(b), `"`))
	return nil
}

// wireCaps is one provider's probed wire capability verdict. Keyed by PARENT
// provider name (pool virtuals share the parent's base URL). Persisted as a
// top-level "wire_caps" key in quota_state.json — restored on boot only when
// the recorded base_url still matches the current config (an endpoint change
// invalidates the verdict), and NOT gated on the health config fingerprint
// (capabilities are endpoint properties, independent of credentials/quota).
type wireCaps struct {
	BaseURL   string    `json:"base_url"`
	Responses triState  `json:"responses"`
	Anthropic triState  `json:"anthropic"`
	ProbedAt  time.Time `json:"probed_at"`
}

const (
	// wireCapProbeConcurrency bounds concurrent probe requests across
	// providers — probes are real upstream calls; don't burst at boot.
	wireCapProbeConcurrency = 4
)

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
	if err != nil {
		return triUnknown
	}
	if status == 404 {
		return triNo
	}
	if status >= 200 && status < 500 {
		return triYes
	}
	return triUnknown
}

// wireProbeBodies builds the two minimal probe bodies (responses, anthropic).
func wireProbeBodies(model string) (responsesBody, anthropicBody []byte) {
	responsesBody = []byte(`{"model":` + strconv.Quote(model) + `,"input":"hi","max_output_tokens":16,"store":false}`)
	anthropicBody = []byte(`{"model":` + strconv.Quote(model) + `,"max_tokens":1,"messages":[{"role":"user","content":"hi"}]}`)
	return
}

// wireProbe sends ONE minimal probe request to the provider's openai_base_url
// and returns the HTTP status (0 on transport/build error). Request build
// mirrors probeModelCallable (RewriteRequest → AuthHeaders → prov.Headers →
// ExtraHeaders) but ALWAYS uses OpenAIBaseURL — the anthropic-base switch in
// models_check.go is deliberately not replicated: the verdict is about what
// the openai endpoint speaks.
func wireProbe(client *http.Client, prov Provider, impl provider.Provider, path string, body []byte) (int, error) {
	targetURL := strings.TrimRight(prov.OpenAIBaseURL, "/") + path
	targetURL, body = impl.RewriteRequest(targetURL, body, path)
	req, err := http.NewRequest(http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if path == "/v1/messages" {
		// Anthropic endpoints require the version header; set it BEFORE
		// ExtraHeaders so a provider impl can still override it.
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	if err := impl.AuthHeaders(req); err != nil {
		return 0, err
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	impl.ExtraHeaders(req, path)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
	return resp.StatusCode, nil
}

// wireProbeModel picks the model id used in probe bodies: the provider's first
// configured model, else the first route target pointing at it, else the
// implicit route for it.
func wireProbeModel(cfg *Config, implicit map[string]RouteTarget, provName string) string {
	if ms := cfg.Providers[provName].Models; len(ms) > 0 {
		return ms[0]
	}
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			if t.Provider == provName {
				return t.Model
			}
		}
	}
	for _, t := range implicit {
		if t.Provider == provName {
			return t.Model
		}
	}
	return ""
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
		// capability concluded (unknown legs are re-probed — no negative
		// conclusion is ever final). With anthropic_base_url set the anthropic
		// leg is never probed (the decision matrix short-circuits to the
		// dedicated base), so only responses counts for freshness.
		if cur, ok := p.wireVerdict(name); ok && cur.BaseURL == provCfg.OpenAIBaseURL &&
			cur.Responses != triUnknown &&
			(provCfg.AnthropicBaseURL != "" || cur.Anthropic != triUnknown) {
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
			log.Printf("[wirecap] provider %s probed: responses=%s anthropic=%s", name, caps.Responses, caps.Anthropic)
		}(name, provCfg, impl)
	}
	wg.Wait()
	if probed {
		p.persistWireCaps()
	}
}

// wireVerdict returns the stored verdict for a provider (parent name).
func (p *Proxy) wireVerdict(name string) (wireCaps, bool) {
	p.wireCapMu.RLock()
	defer p.wireCapMu.RUnlock()
	c, ok := p.wireCaps[name]
	return c, ok
}

// setWireCaps stores a verdict (parent name).
func (p *Proxy) setWireCaps(name string, caps wireCaps) {
	p.wireCapMu.Lock()
	if p.wireCaps == nil {
		p.wireCaps = map[string]wireCaps{}
	}
	p.wireCaps[name] = caps
	p.wireCapMu.Unlock()
}

// wireCapsSnapshot returns a copy of the verdict map for persistence.
func (p *Proxy) wireCapsSnapshot() map[string]wireCaps {
	p.wireCapMu.RLock()
	defer p.wireCapMu.RUnlock()
	out := make(map[string]wireCaps, len(p.wireCaps))
	for k, v := range p.wireCaps {
		out[k] = v
	}
	return out
}

// persistWireCaps triggers an ASYNC persist (never synchronous from the
// request path: persist takes p.mu.RLock via fullSnapshot, and a handler
// already holds it — a synchronous call could deadlock behind a pending
// reload writer). The goroutine is quota-tracked so Close drains it.
func (p *Proxy) persistWireCaps() {
	if p.quota == nil {
		return
	}
	p.quota.launch(func() {
		if p.quota.stopped() {
			return
		}
		if err := p.quota.persist(); err != nil {
			log.Printf("[wirecap] persist failed: %v", err)
		}
	})
}

// noteWireResponsesMiss flips a provider's responses verdict to no after a
// verdict-driven /responses request came back 404 — the probe said yes but
// the route doesn't exist (stale verdict, or a per-model gateway). This is
// OUR protocol-choice miss, not a missing model, so tryTarget skips
// recordModelFailure for it; subsequent requests fall back to chat.
func (p *Proxy) noteWireResponsesMiss(name string) {
	// Verdicts are keyed by parent name; a pool virtual shares the parent's
	// base URL. parentOf is reload-guarded (p.mu); grab the reference first —
	// reload swaps maps, never mutates them in place (same pattern as
	// resetHealth). tryTarget callers do NOT hold p.mu here.
	p.mu.RLock()
	parentOf := p.parentOf
	p.mu.RUnlock()
	parent := name
	if par, ok := parentOf[name]; ok {
		parent = par
	}
	p.wireCapMu.Lock()
	caps := p.wireCaps[parent]
	caps.Responses = triNo
	caps.ProbedAt = time.Now()
	if p.wireCaps == nil {
		p.wireCaps = map[string]wireCaps{}
	}
	p.wireCaps[parent] = caps
	p.wireCapMu.Unlock()
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
	p.quota.launch(func() {
		if p.quota.stopped() {
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
	switch clientProto {
	case "anthropic":
		if hasAnthropicBase {
			return "anthropic", false
		}
		if ok {
			switch {
			case caps.Anthropic == triYes:
				return "anthropic", false
			case caps.Responses == triYes:
				return "responses", true
			case caps.Responses == triNo:
				return "openai", false
			}
		}
		return clientProto, false
	case "responses":
		if ok && caps.Responses == triNo {
			return "openai", false
		}
		return clientProto, false
	default:
		return clientProto, false
	}
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
