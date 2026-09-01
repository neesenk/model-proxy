package app

import (
	"fmt"
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/logx"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/fusion"
	"model-proxy/internal/guard"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
	"model-proxy/internal/shadow"
)

func NewProxy(cfg *Config) *Proxy {
	home, _ := os.UserHomeDir()
	p := NewProxyWithStatePath(cfg, filepath.Join(home, ".model-proxy", "quota_state.json"))
	// Production only: probe wire capabilities asynchronously at boot (the
	// injectable constructor leaves probing off; tests drive it synchronously).
	p.wireProbe = true
	p.startWireCapProbe()
	return p
}

// NewProxyWithStatePath is the injectable constructor used by tests so every
// Proxy owns an isolated state file before the tracker loads or starts.
func NewProxyWithStatePath(cfg *Config, qpath string) *Proxy {
	// Apply the configured credentials mode (`credentials:`) before any pool
	// I/O: AccountStore and the web/login save paths resolve stores through
	// the accounts process default, and OAuth blob storage follows credstore's
	// process mode. Reload re-applies both per config. A non-empty
	// MP_CRED_STORE overriding only the OAuth side gets one visible line.
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	if note := accounts.CredentialMismatchNote(cfg.Credentials); note != "" {
		logx.Warnf("[startup] ⚠ %s", note)
	}
	built := BuildProviders(cfg, AccountStore(), buildOpts())
	// Same scanner entry point as Reload: startup and reload build identical
	// generations. An error is only reachable with an unvalidated Config
	// (validate rejects bad guard.extra_patterns at load) — degrade to the
	// embedded table + known secrets rather than start without any guard.
	guardScanner, err := buildGuardScanner(cfg, built.Secrets)
	if err != nil {
		logx.Warnf("[startup] guard scanner: %v; falling back to built-in rules + known secrets", err)
		secrets := built.Secrets
		if !cfg.Guard.KnownSecretsEnabled() {
			secrets = nil
		}
		// The fallback can only fail if the embedded rule table itself is
		// broken (custom patterns are nil here, so config cannot be the cause).
		// Degrade to nil — forward skips the guard entirely — rather than run
		// a scanner we no longer trust; the warning makes the loss loud.
		guardScanner, err = guard.NewScannerWithOptions(nil, secrets, cfg.Guard.ExtraPaths, guard.Options{Decode: cfg.Guard.DecodeEnabled()})
		if err != nil {
			// Errorf, not Warnf: this announces a security control is OFF, so
			// it must survive even log_level: error (level filtering, 063b2f9).
			// The sibling warn above keeps a working fallback scanner, so it
			// stays level-filtered.
			logx.Errorf("[startup] guard fallback scanner failed: %v; outbound guard scanning is DISABLED for this process", err)
			guardScanner = nil
		}
	}
	// http.DefaultTransport pools at most 2 idle connections per host; concurrent
	// streams to one upstream would re-dial TLS after the first two close. The
	// proxy fans out to a handful of upstream hosts, so pool generously instead.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 256
	transport.MaxIdleConnsPerHost = 100
	p := &Proxy{
		lifecycle: runtimestate.NewLifecycle(),
		cfg:       cfg,
		providers: built.Providers,
		client:    &http.Client{Timeout: 0, Transport: transport},
		poolIndex: built.PoolIndex,
		parentOf:  built.ParentOf,
		// Read once here (not per request): MP_PPROF=1 turns on /debug/pprof/.
		pprofEnabled: os.Getenv("MP_PPROF") == "1",
	}
	p.runtimeState.ReplaceGeneration(1)
	p.configGeneration.Store(1)
	// Reload-owned: assigned before any request/goroutine can read it, swapped
	// under p.mu on reload.
	p.guardScanner = guardScanner
	p.applyAuthSources(cfg)
	// Split-exfiltration session windows: cross-generation observation state
	// (like p.metrics below), NOT reload-owned — reload must not wipe in-flight
	// session context. Never logged or persisted (see session_scan.go).
	p.sessionScan = newSessionScanStore()
	// The OAuth subset is tracked separately so the refresh loop can re-sync it
	// (OAuth tokens rotate in place during serve) without re-running a full
	// BuildProviders pass; the pool subset is the stable base of every rebuild.
	p.guardPoolSecrets = built.PoolSecrets
	p.guardOAuthSecrets = built.OAuthSecrets
	p.derivedRoutes = DeriveRoutesFrom(cfg)
	p.expandedRoutes = p.buildExpandedRoutes()
	p.routeKeys = routeKeySet(p.expandedRoutes)
	// Config-time routing hazards (reasoning-replay models behind conversion,
	// missing protocol: on hint providers): appended to the warnings channel
	// (/api/status + `models` CLI) AND logged — the operator should see them at
	// boot, not only when they open the dashboard.
	if hw := ConfigRoutingWarnings(cfg, p.expandedRoutes); len(hw) > 0 {
		p.routeWarnings = append(p.routeWarnings, hw...)
		for _, w := range hw {
			logx.Warnf("[startup] ⚠ %s", w)
		}
	}
	// The tracker reads cfg/providers asynchronously via the snapshot closures
	// (each takes p.mu.RLock), so reloads are picked up without recreating it.
	p.quota = runtimestate.NewQuotaTracker(qpath,
		func() *Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() },
		&p.runtimeState)
	p.quota.Generation = p.configGeneration.Load
	p.quota.FullSnapshot = p.snapshotPersistedState
	p.quota.Start()
	// Guard OAuth known-secret re-sync: codex/aqp providers rotate their tokens
	// in place during serve (rewriting <name>_oauth_auth.json), which would
	// otherwise leave the boot-time scanner matching stale values until the
	// next reload. The loop re-collects the OAuth files on the quota-poll beat
	// and swaps the scanner in place, generation-consistent. Lifecycle-admitted
	// like the other loops: stopped and waited by Close.
	p.lifecycle.Run(p.guardSecretRefreshLoop)
	p.metrics = counters.NewMetricsStore()
	// SSE token counter. Persistence (baseline restore + per-minute flush) is
	// projected by statsFlusher into internal/observe/stats.Store, opened only
	// by lifecycle services so direct-NewProxy tests stay in-memory.
	p.tokens = counters.NewTokenCounter()
	// Per-agent counters (detected from the client UA). Flushed alongside the
	// minute buckets by the same flusher; nil-stats tests keep them in-memory.
	p.agents = counters.NewAgentCounter()
	// Exact-match response cache. nil unless cache.enabled is set in config, so
	// the default (off) path and direct-NewProxy tests pay zero overhead.
	p.cache = NewResponseCache(cfg.Cache)
	p.responsesState = protocol.NewResponsesStateStore(protocol.ResponsesStatePath(qpath))
	// Live request monitor hub (SSE /api/events). Always on — empty unless a Web
	// UI client subscribes; publish is non-blocking so it never stalls forward.
	p.events = observeevents.NewHub()
	// Fusion orchestration observability registry (recent runs + per-workflow
	// aggregates + daily budget counters). Like the event hub, reload does NOT
	// rebuild it — aggregates and today's budget survive config edits.
	p.fusionReg = fusion.NewRegistry()
	// Detached Shadow runtime (sample rate, concurrency gate, shared client). Stored
	// in an atomic pointer so reload can swap the whole bundle race-free; each
	// dispatch loads it once and uses that snapshot, so in-flight shadow goroutines
	// finish on the old bundle while new traffic follows the reloaded config.
	p.shadow.Store(shadow.NewRuntime(shadow.Options{
		SampleRate:    cfg.ShadowSampleRate,
		MaxConcurrent: cfg.ShadowMaxConcurrent,
		Timeout:       cfg.Scheduling.Timeout(),
	}))
	// Restore the per-route sticky selections persisted before the last restart,
	// so the proxy resumes parking on the same providers (prompt-cache-friendly).
	// Gated like the health restore below: a file with a MISMATCHING config
	// fingerprint belongs to a different config (or a test binary sharing the
	// state file) — its route→provider parks must not leak over (route names
	// like "m1" collide across configs even when providers don't). Legacy
	// files without a fingerprint keep the historical restore behavior.
	fp := healthConfigFingerprint(cfg)
	fpMatch := p.quota.LoadedHealthFP == "" || p.quota.LoadedHealthFP == fp
	if p.quota.LoadedHealthFP != "" && !fpMatch {
		// Quota snapshots are provider/account observations too. A changed provider
		// identity or endpoint must not inherit the old file's scheduling tier.
		p.quota.ClearForGeneration(p.configGeneration.Load())
	}
	if loaded := p.quota.LoadedSticky; len(loaded) > 0 && fpMatch {
		p.runtimeState.RestoreSticky(loaded)
	}
	// Restore frozen health state (rate-limit/circuit cooldowns, model lockouts,
	// learned param blocklist) persisted before the last restart — ONLY when the
	// file's config fingerprint matches the current config: health is keyed by
	// provider name, so without the gate a different config (or a test binary
	// sharing the state file) would inherit cooldowns onto unrelated same-named
	// providers. Only future-dated cooldowns are applied — expired ones
	// self-heal by being dropped. A restored circuit gets a full failure count
	// so its next failure re-opens it immediately (same semantics as before).
	if loaded := p.quota.LoadedHealth; len(loaded) > 0 && p.quota.LoadedHealthFP != "" && p.quota.LoadedHealthFP == fp {
		p.runtimeState.RestoreHealth(loaded, time.Now(), cfg.Scheduling.Threshold())
	}
	// Restore wire capability verdicts (independent of the health fingerprint:
	// capabilities are endpoint properties). A verdict is honored only while
	// its recorded base_url still matches the current config — an endpoint
	// change invalidates it and triggers a re-probe at the next boot probe.
	if loaded := p.quota.LoadedWireCaps; len(loaded) > 0 {
		baseURLs := make(map[string]string, len(cfg.Providers))
		for name, prov := range cfg.Providers {
			baseURLs[name] = prov.OpenAIBaseURL
		}
		p.wireCaps.RestoreMatching(loaded, baseURLs)
	}
	return p
}

// Close releases every Proxy-owned background component and performs final
// flushes. It is idempotent; lifecycle admission is closed before waiting so a
// concurrent reload cannot add a catalog refresh behind shutdown.
func (p *Proxy) Close() {
	p.closeOnce.Do(p.closeRuntimeServices)
}

// resetStats zeroes all call-statistics state: the in-memory metrics + token
// counters, the persisted SQLite bucket history, and the flusher's diff
// baseline (so the next flush sees zero delta rather than zero-minus-old
// negatives). Drives POST /api/tokens/reset ("reset counters").
func (p *Proxy) resetStats() error {
	// When a flusher exists, delegate to flusher.reset which does the full stats
	// reset (counters + DB + baseline) UNDER the flusher's lock — preventing a
	// concurrent per-minute flush from writing stale deltas to the just-cleared
	// DB (the resetStats vs flush race).
	if p.flusher != nil {
		if err := p.flusher.Reset(); err != nil {
			return fmt.Errorf("reset persisted stats: %w", err)
		}
	} else {
		// Non-flusher path (degenerate tests): clear durable state first. If that
		// fails, keep the in-memory totals so a later restart cannot resurrect a
		// history the user was told had been reset.
		if p.stats != nil {
			if err := p.stats.Reset(); err != nil {
				return fmt.Errorf("reset persisted stats: %w", err)
			}
		}
		if p.metrics != nil {
			p.metrics.Reset()
		}
		if p.tokens != nil {
			p.tokens.Reset()
		}
		if p.agents != nil {
			p.agents.Reset()
		}
	}
	// Response cache participates in the user-facing "reset counters" command,
	// but is not stats persistence and therefore stays outside statsFlusher.
	// p.cache is swapped by Reload under p.mu — read it under the same RLock
	// every other reader takes (POST /api/tokens/reset may race a reload).
	p.mu.RLock()
	cache := p.cache
	p.mu.RUnlock()
	if cache != nil {
		cache.Reset()
	}
	return nil
}

// newResponseCache adapts resolved application configuration into the
// repository-leaf cache component.
func NewResponseCache(config CacheConfig) *responsecache.Store {
	if !config.IsEnabled() {
		return nil
	}
	return responsecache.New(responsecache.Options{
		TTL:          config.TTLDuration(),
		MaxEntries:   config.MaxEntriesValue(),
		MaxBodyBytes: config.MaxBodyBytesValue(),
	})
}
