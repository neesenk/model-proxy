// modelcaps.go — model-level protocol capability probing: after boot (and
// each reload), the proxy probes every provider's models for chat /
// anthropic / responses support and caches the verdicts in
// model_caps.json. The cache is invalidated ONLY by the provider's
// protocol-relevant config fingerprint (base urls / provider_id / headers) —
// an unchanged fingerprint means no re-probe (no TTL); legs that concluded
// Unknown (auth/quota/5xx/network) are re-probed on the next pass. The
// fingerprint does NOT cover the model list: each pass prunes verdicts for
// models the current config no longer serves (removed from models:/routes),
// so the store and /api/models never serve dropped models.
//
// The verdicts feed two consumers: forward's protocol selection
// (resolvedBackendProto inserts the model-level matrix between ProtocolHint
// and the provider-level wire verdict) and the /api/models + CLI display.
// Probing itself lives in internal/probe (ProbeModelProtocols); the verdict
// store and file format live in internal/runtime/wirecap. See
// docs/architecture/routing-and-failure.md and runtime-state.md.
package app

import (
	"context"
	"net/http"
	"sync"
	"time"

	"model-proxy/internal/observe/logx"
	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/upstreamproxy"
)

// protocolFingerprints computes every configured provider's protocol-relevant
// config fingerprint (the cache-invalidation key of model_caps.json).
func protocolFingerprints(cfg *Config) map[string]string {
	fps := make(map[string]string, len(cfg.Providers))
	for name, prov := range cfg.Providers {
		fps[name] = providerbuild.ProtocolConfigFingerprint(prov)
	}
	return fps
}

// modelCapsModels returns the model set probed for one provider: config
// models ∪ explicit route targets ∪ derived route targets.
func modelCapsModels(cfg *Config, derived map[string][]RouteTarget, provName string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(m string) {
		if m != "" && !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	for _, m := range cfg.Providers[provName].Models {
		add(m)
	}
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			if t.Provider == provName {
				add(t.Model)
			}
		}
	}
	for _, targets := range derived {
		for _, t := range targets {
			if t.Provider == provName {
				add(t.Model)
			}
		}
	}
	return out
}

// probeAllModelCaps probes every provider's model protocol matrix once
// (skipping providers whose fingerprint matches and whose models are all
// concluded) and persists the results. Providers with a ProtocolHint (codex)
// are synthesized, not probed: the hint already declares their protocol.
// Runs synchronously; callers dispatch it on a tracked goroutine (boot/reload
// via startWireCapProbe) or invoke it directly (tests).
func (p *Proxy) probeAllModelCaps() {
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
	poolIndex := p.poolIndex
	derived := p.derivedRoutes
	p.mu.RUnlock()

	client := &http.Client{Timeout: wireCapProbeTimeout, Transport: upstreamproxy.AutoTransport()}
	sem := make(chan struct{}, wireCapProbeConcurrency)
	var wg sync.WaitGroup
	dirty := false
	now := time.Now()
	for name, provCfg := range cfg.Providers {
		fp := providerbuild.ProtocolConfigFingerprint(provCfg)
		models := modelCapsModels(cfg, derived, name)
		// Prune verdicts for models the current config no longer serves. The
		// fingerprint does not cover the model list, so without this a model
		// removed from models:/routes would linger in the store (and in
		// /api/models) forever.
		keep := make(map[string]bool, len(models))
		for _, m := range models {
			keep[m] = true
		}
		if p.modelCaps.PruneModels(name, keep) {
			dirty = true
		}
		if provider.ProtocolHint(provCfg.Provider, "") != "" {
			// Hint-covered provider (codex → responses): the protocol is known
			// without probing. Synthesize once, then the fingerprint skip
			// below keeps it stable.
			if _, ok := p.modelCaps.ProviderFingerprint(name); !ok || len(models) > 0 {
				for _, m := range models {
					if _, ok := p.modelCaps.Get(name, m); !ok {
						p.modelCaps.Put(name, fp, m, runtimewire.ModelProtocols{
							Chat:      triNo,
							Anthropic: triNo,
							Responses: triYes,
						}, now)
						dirty = true
					}
				}
			}
			continue
		}
		// Skip when the fingerprint matches and every model is concluded.
		if storedFP, ok := p.modelCaps.ProviderFingerprint(name); ok && storedFP == fp {
			allConcluded := true
			for _, m := range models {
				if mp, ok := p.modelCaps.Get(name, m); !ok || !mp.Concluded() {
					allConcluded = false
					break
				}
			}
			if allConcluded {
				continue
			}
		}
		impl := provs[name]
		if impl == nil {
			if vids := poolIndex[name]; len(vids) > 0 {
				impl = provs[vids[0]]
			}
		}
		if impl == nil {
			continue // not logged in / not built — nothing to probe with
		}
		if len(models) == 0 {
			continue
		}
		dirty = true
		for _, model := range models {
			wg.Add(1)
			go func(name string, provCfg Provider, impl provider.Provider, fp, model string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				legs := probe.ProbeModelProtocols(context.Background(), client, provCfg, impl, model)
				mp := runtimewire.ModelProtocols{}
				for _, leg := range legs {
					v := runtimewire.ClassifyModelStatus(leg.Probed, leg.Status, leg.Err, leg.Body)
					if v == runtimewire.Unknown {
						// Unknown is the only verdict with no persisted cause —
						// without this line a transient (upstream 429/5xx vs a
						// proxy-side dial/timeout) is indistinguishable after
						// the fact. Status/err only; the body may carry
						// sensitive text.
						logx.Warnf("[modelcaps] %s/%s leg %s inconclusive: status=%d err=%v (will re-probe next pass)",
							name, model, leg.Leg, leg.Status, leg.Err)
					}
					switch leg.Leg {
					case probe.LegChat:
						mp.Chat = v
					case probe.LegAnthropic:
						mp.Anthropic = v
					case probe.LegResponses:
						mp.Responses = v
					}
				}
				p.modelCaps.Put(name, fp, model, mp, now)
				logx.Debugf("[modelcaps] %s/%s probed: chat=%s anthropic=%s responses=%s",
					name, model, mp.Chat, mp.Anthropic, mp.Responses)
			}(name, provCfg, impl, fp, model)
		}
	}
	wg.Wait()
	if dirty {
		p.persistModelCaps()
	}
}

// persistModelCaps triggers an ASYNC persist of model_caps.json (never
// synchronous from the request path — same deadlock rationale as
// persistWireCaps). The goroutine is quota-tracked so Close drains it.
func (p *Proxy) persistModelCaps() {
	if p.quota == nil || p.modelCapsPath == "" {
		return
	}
	p.quota.Launch(func() {
		if p.quota.Stopped() {
			return
		}
		if err := runtimewire.SaveModelCapsFile(p.modelCapsPath, p.modelCaps.Snapshot()); err != nil {
			logx.Warnf("[modelcaps] persist failed: %v", err)
		}
	})
}
