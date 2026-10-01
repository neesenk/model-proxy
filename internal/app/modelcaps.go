// modelcaps.go — model-level protocol capability probing: after boot (and
// each reload), the proxy probes every provider's models for chat /
// anthropic / responses support and caches the verdicts in
// model_caps.json. The cache is invalidated ONLY by the provider's
// protocol-relevant config fingerprint (base urls / provider_id / headers) —
// an unchanged fingerprint means no re-probe (no TTL); legs that concluded
// Unknown (auth/quota/5xx/network) are re-probed on the next pass, and a
// transient Unknown never overwrites a stored conclusion (wirecap
// MergeOnUnknown — one rate-limited pass must not flap a known-good yes to
// "? unknown"). Operator DISABLED models are never probed: their verdicts
// stay frozen in the store and probing resumes when they are re-enabled.
// The fingerprint does NOT cover the model list: each pass prunes verdicts
// for models the current config no longer serves (removed from
// models:/routes), so the store and /api/models never serve dropped
// models.
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
	configdomain "model-proxy/internal/config"
	"net/http"
	"os"
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
func protocolFingerprints(cfg *configdomain.Config) map[string]string {
	fps := make(map[string]string, len(cfg.Providers))
	for name, prov := range cfg.Providers {
		fps[name] = providerbuild.ProtocolConfigFingerprint(prov)
	}
	return fps
}

// disabledSet turns one provider's disabled-model list into a lookup set.
// A nil list yields a nil-safe empty set.
func disabledSet(models []string) map[string]bool {
	if len(models) == 0 {
		return nil
	}
	set := make(map[string]bool, len(models))
	for _, m := range models {
		set[m] = true
	}
	return set
}

// modelCapsModels returns the model set probed for one provider: config
// models ∪ explicit route targets ∪ derived route targets.
func modelCapsModels(cfg *configdomain.Config, derived map[string][]configdomain.RouteTarget, provName string) []string {
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
// Runs synchronously; callers dispatch it on a tracked goroutine with a
// stop/budget-bound ctx (boot/reload via runWireProbePass) or invoke it
// directly (tests). A canceled ctx aborts the pass: in-flight legs are
// dropped WITHOUT storing a matrix (an aborted leg is no information, not an
// unknown verdict) and remaining providers/models are skipped — the next pass
// re-probes them.
func (p *Proxy) probeAllModelCaps(ctx context.Context) {
	p.mu.RLock()
	cfg := p.cfg
	provs := p.providers
	poolIndex := p.poolIndex
	derived := p.derivedRoutes
	p.mu.RUnlock()
	// Operator disabled models (Manager-owned, persists across refreshes) are
	// NOT probed: the override means the model is out of rotation, so probing
	// it wastes upstream quota and rate-limit headroom (the observed churn:
	// one not-logged-in provider with 12 disabled models fired 36 auth-error
	// legs per pass). Their stored verdicts stay FROZEN (not pruned, not
	// overwritten); re-enabling resumes probing on the next pass.
	disabledByProvider := p.runtimeState.DisabledModels()

	client := &http.Client{Timeout: wireCapProbeTimeout, Transport: upstreamproxy.AutoTransport()}
	sem := make(chan struct{}, wireCapProbeConcurrency)
	var wg sync.WaitGroup
	dirty := false
	now := time.Now()
	for name, provCfg := range cfg.Providers {
		if ctx.Err() != nil {
			break // pass budget/stop: leave the rest to the next pass
		}
		fp := providerbuild.ProtocolConfigFingerprint(provCfg)
		models := modelCapsModels(cfg, derived, name)
		// Prune verdicts for models the current config no longer serves. The
		// fingerprint does not cover the model list, so without this a model
		// removed from models:/routes would linger in the store (and in
		// /api/models) forever. Disabled models STAY in the keep set: the
		// operator blocked the model, not the verdict store's memory of it.
		keep := make(map[string]bool, len(models))
		for _, m := range models {
			keep[m] = true
		}
		if p.modelCaps.PruneModels(name, keep) {
			dirty = true
		}
		if hint := provider.ProtocolHint(provCfg.Provider, ""); hint != "" {
			// Hint-covered provider (codex → responses, typesafe → decisions):
			// the protocol is known without probing. Synthesize once, then the
			// fingerprint skip below keeps it stable. The synthesized verdict
			// marks the hinted leg; decisions has no probe leg (hint resolves
			// it before caps are ever consulted), so all three legs record No.
			// Synthesis is free (no network), so disabled models are covered too.
			verdict := runtimewire.ModelProtocols{Chat: triNo, Anthropic: triNo, Responses: triNo}
			if hint == "responses" {
				verdict.Responses = triYes
			}
			if _, ok := p.modelCaps.ProviderFingerprint(name); !ok || len(models) > 0 {
				for _, m := range models {
					// Synthesize when missing OR when a legacy entry never
					// concluded (e.g. unknown legs recorded before the provider
					// became hint-covered — without the overwrite they linger as
					// "? unknown" forever; the hint is authoritative, and Put's
					// merge lets the concluded synthesis win over stored unknowns).
					if cur, ok := p.modelCaps.Get(name, m); ok && cur.Concluded() {
						continue
					}
					p.modelCaps.Put(name, fp, m, verdict, now)
					dirty = true
				}
			}
			continue
		}
		// The probe path excludes disabled models: nothing is sent for them,
		// and they cannot hold a provider eligible for a pass (a provider whose
		// every model is disabled is skipped entirely). The filter expands the
		// same keys TargetDisabled matches for scheduling targets — the exact
		// config-level key AND (here, in the parent→virtuals direction) every
		// pool-virtual key of this parent: a model disabled for ANY virtual
		// account ("zhipu#acct1") is out of rotation there, and the pass probes
		// once per parent with a shared parent-keyed verdict, so probing it
		// would burn quota on a model no account rotates.
		disabled := disabledSet(disabledByProvider[name])
		for _, vid := range poolIndex[name] {
			for m := range disabledSet(disabledByProvider[vid]) {
				if disabled == nil {
					disabled = map[string]bool{}
				}
				disabled[m] = true
			}
		}
		probeModels := make([]string, 0, len(models))
		for _, m := range models {
			if !disabled[m] {
				probeModels = append(probeModels, m)
			}
		}
		// Verdicts stored under the CURRENT fingerprint are trusted: skip the
		// provider when every model is concluded, and (below) skip individual
		// concluded models. A stale-fingerprint entry is re-probed wholesale —
		// its conclusions describe a base URL the config no longer has.
		fpMatches := false
		if storedFP, ok := p.modelCaps.ProviderFingerprint(name); ok && storedFP == fp {
			fpMatches = true
			allConcluded := true
			for _, m := range probeModels {
				if mp, ok := p.modelCaps.Get(name, m); !ok || !mp.Concluded() {
					allConcluded = false
					break
				}
			}
			if allConcluded {
				continue
			}
		}
		impl := implOrFirstPooled(provs, poolIndex, name)
		if impl == nil {
			continue // not logged in / not built — nothing to probe with
		}
		if len(probeModels) == 0 {
			continue
		}
		dirty = true
		for _, model := range probeModels {
			// Concluded models under the CURRENT fingerprint are skipped: yes is
			// trusted indefinitely (the runtime 404 correction rewrites a wrong
			// one) and no is fingerprint-gated by design. Re-probing them as
			// collateral of a sibling's unknown leg re-bursts the whole provider
			// against rate-limited upstreams (the zcode/zhipu 429 storms) — the
			// burst both wastes quota and risks downgrading verdicts that were
			// fine (Put's merge-on-unknown is the second line of defense).
			if fpMatches {
				if mp, ok := p.modelCaps.Get(name, model); ok && mp.Concluded() {
					continue
				}
			}
			wg.Add(1)
			go func(name string, provCfg configdomain.Provider, impl provider.Provider, fp, model string) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()
				if ctx.Err() != nil {
					return
				}
				legs := probe.ProbeModelProtocols(ctx, client, provCfg, impl, model)
				if ctx.Err() != nil {
					return // aborted mid-probe: store nothing; the next pass re-probes
				}
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
		p.persistModelCapsNow()
	})
}

// persistModelCapsNow is the synchronous persist core; persistModelCaps runs
// it on the quota-tracked goroutine, tests invoke it directly (no lifecycle
// race). Before writing it stats the target: a file NEWER than
// modelCapsFileBaseline means an external writer (CLI `models refresh`)
// published fresher verdicts after the in-memory store's source state was
// read — overwriting would clobber them with the daemon's older snapshot.
// Instead of writing, the daemon ADOPTS the external state (re-read +
// Restore + re-baseline, the same thing the reload re-read would do): a bare
// skip used to latch the guard — when no reload followed (the CLI only
// SIGHUPs on a model-set change or a successful verdict persist, and the
// signal can miss), the file stayed newer than the baseline forever and every
// later daemon persist was suppressed. An unreadable/external-but-unusable
// file keeps the old defer behavior (reload re-read remains the adoption
// path). The residual stat→rename window is inherent to two uncoordinated
// processes; the check collapses the common case (an earlier-triggered
// persist executing after the CLI's write).
func (p *Proxy) persistModelCapsNow() {
	// The stat→adopt/save→re-baseline section is a daemon-side critical
	// section: without the mutex, a concurrent persist could stat the file
	// this one just renamed (before its baseline note lands) and adopt the
	// daemon's OWN snapshot as an external write.
	p.modelCapsPersistMu.Lock()
	defer p.modelCapsPersistMu.Unlock()
	if fi, err := os.Stat(p.modelCapsPath); err == nil &&
		fi.ModTime().UnixNano() > p.modelCapsFileBaseline.Load() {
		if loaded, err := runtimewire.LoadModelCapsFile(p.modelCapsPath); err == nil && loaded != nil {
			p.modelCaps.Restore(loaded, protocolFingerprints(p.cfgSnapshot()))
			p.noteModelCapsFileState()
			logx.Debugf("[modelcaps] persist adopted external write to %s (e.g. `models refresh`) instead of overwriting; re-baselined", p.modelCapsPath)
		} else {
			logx.Debugf("[modelcaps] persist skipped: %s is newer than the in-memory baseline but unreadable (external write, e.g. `models refresh`); reload will re-read it", p.modelCapsPath)
		}
		return
	}
	if err := runtimewire.SaveModelCapsFile(p.modelCapsPath, p.modelCaps.Snapshot()); err != nil {
		logx.Warnf("[modelcaps] persist failed: %v", err)
		return
	}
	p.noteModelCapsFileState()
}

// noteModelCapsFileState re-baselines modelCapsFileBaseline to the file's
// current mtime (zero when missing/unreadable): called after every read the
// in-memory store is derived from (boot restore, reload re-read) and after
// every successful own persist. A zero baseline means "no file seen yet", so
// any existing file counts as externally written.
func (p *Proxy) noteModelCapsFileState() {
	var nanos int64
	if fi, err := os.Stat(p.modelCapsPath); err == nil {
		nanos = fi.ModTime().UnixNano()
	}
	p.modelCapsFileBaseline.Store(nanos)
}
