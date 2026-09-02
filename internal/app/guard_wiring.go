// guard_wiring.go — outbound guard wiring: per-generation scanner construction, the known-secret refresh loop, and the pure per-request guard decision shared by live forwarding and /debug/route.
package app

import (
	"fmt"
	"model-proxy/internal/guard"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	"regexp"
	"sort"
	"time"
)

// buildGuardScanner compiles the per-generation outbound guard scanner from
// the resolved config and the credential values collected by BuildProviders.
// It runs OUTSIDE p.mu (regexp compilation + variant precomputation are not
// free); reload/startup then swap the immutable pointer under the lock.
//
// known_secrets: false drops the credential values (pattern table only);
// decode: false disables the encoded-form channels (base64/hex/url). Bad
// extra_patterns are rejected at config validate, so an error here means an
// unvalidated Config — fail-closed (reload keeps the old generation).
func buildGuardScanner(cfg *Config, secrets []string) (*guard.Scanner, error) {
	g := cfg.Guard
	var custom []guard.CustomPattern
	for _, ep := range g.ExtraPatterns {
		re, err := regexp.Compile(ep.Regex)
		if err != nil {
			return nil, fmt.Errorf("guard.extra_patterns[%s]: %w", ep.Name, err)
		}
		var literal []byte
		if ep.Literal != "" {
			literal = []byte(ep.Literal)
		}
		custom = append(custom, guard.CustomPattern{Name: ep.Name, RE: re, Literal: literal})
	}
	if !g.KnownSecretsEnabled() {
		secrets = nil
	}
	return guard.NewScannerWithOptions(custom, secrets, g.ExtraPaths, guard.Options{Decode: g.DecodeEnabled()})
}

// guardSecretRefreshLoop periodically re-syncs the guard known-secret set with
// the on-disk codex/aqp OAuth auth files (those providers refresh and rewrite
// <name>_oauth_auth.json in place during serve) and with the in-memory
// credential values providers report (provider.SecretReporter: aqp's minted
// key, codex's cached access_token — fresher than any file). The beat is the CURRENT
// generation's scheduling.quota_poll_interval (default 5m, re-read every tick
// so a reload changes the cadence without a restart) — the same cadence the
// quota tracker polls upstream quotas on, so credential freshness follows the
// established background rhythm instead of growing a second one. Admitted to
// the Proxy lifecycle at construction; stopped and waited by Close like the
// stats flusher and budget watcher.
func (p *Proxy) guardSecretRefreshLoop(stop <-chan struct{}) {
	for {
		interval := 5 * time.Minute
		if cfg := p.cfgSnapshot(); cfg != nil {
			interval = cfg.Scheduling.PollInterval()
		}
		if interval <= 0 {
			interval = 5 * time.Minute // hand-built Config with a bad value: never spin
		}
		timer := time.NewTimer(interval)
		select {
		case <-timer.C:
			p.refreshGuardKnownSecrets()
		case <-stop:
			timer.Stop()
			return
		}
	}
}

// refreshGuardKnownSecrets performs one re-sync pass: re-collect the OAuth
// token values for the current config generation (auth files + provider
// in-memory reports) and, when the set changed, rebuild the guard scanner and
// swap it in place.
//
// Locking: the generation snapshot (cfg, pool/OAuth secret subsets, providers
// map) is taken under a brief read lock; all file I/O, provider ReportSecrets
// calls, and scanner construction happen OUTSIDE p.mu (the providers map is
// generation-owned and immutable, so iterating the captured reference is
// safe); the write lock below only swaps pointers, and only when the
// generation captured before the I/O is still current. A reload that
// interleaved already installed its own scanner and secret subsets, so a
// stale rebuild is dropped — scanner contents never mix generations.
func (p *Proxy) refreshGuardKnownSecrets() {
	p.mu.RLock()
	cfg := p.cfg
	generation := p.configGeneration.Load()
	poolSecrets := p.guardPoolSecrets
	prevOAuth := p.guardOAuthSecrets
	providers := p.providers
	p.mu.RUnlock()

	if cfg == nil || !cfg.Guard.KnownSecretsEnabled() {
		// known_secrets: false means the scanner carries no credential values
		// at all — nothing to re-sync.
		return
	}
	freshOAuth := providerbuild.CollectOAuthSecrets(cfg, providerbuild.BuildOpts())
	// In-memory reports (aqp minted key, codex cached access_token) merge into
	// the OAuth subset: like the file-derived values they can change without a
	// reload, and the generation check below keeps both generation-consistent.
	freshOAuth = append(freshOAuth, collectMemorySecrets(providers)...)
	if equalSecretSets(freshOAuth, prevOAuth) {
		return
	}
	// Fresh concatenation: poolSecrets is reload-owned state shared with the
	// current scanner's build inputs — never append into its backing array.
	scanner, err := buildGuardScanner(cfg, append(append([]string(nil), poolSecrets...), freshOAuth...))
	if err != nil {
		// Only reachable with an unvalidated Config; keep the previous scanner.
		logx.Warnf("[guard] OAuth secret re-sync: rebuild failed: %v (keeping previous scanner)", err)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.configGeneration.Load() != generation || p.cfg != cfg {
		// A reload swapped the generation while we read files: its scanner and
		// secret subsets win; the next tick re-evaluates against them.
		return
	}
	p.guardScanner = scanner
	p.guardOAuthSecrets = freshOAuth
}

// collectMemorySecrets gathers the in-memory credential values providers
// choose to report (provider.SecretReporter): aqp's minted managed key and
// codex's cached access_token, which the on-disk collection cannot see (the
// minted key exists in no file) or sees only stale (a fresher in-memory
// token). The map must be a generation-owned snapshot captured under p.mu by
// the caller. Reported values stay in process memory, guard scanning only.
func collectMemorySecrets(providers map[string]provider.Provider) []string {
	var out []string
	for _, prov := range providers {
		if r, ok := prov.(provider.SecretReporter); ok {
			out = append(out, r.ReportSecrets()...)
		}
	}
	return out
}

// equalSecretSets compares two secret sets order-insensitively (collection
// iterates a map, so order is nondeterministic). Values never leave process
// memory; nothing here logs them.
func equalSecretSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// requestGuardDecision is the pure, per-request portion of the outbound guard
// contract shared by live forwarding and /debug/route. Observation side
// effects (counters, events, audit, and session-fragment state) stay in the
// live path; both callers nevertheless make the same secret/path decision and
// derive the same body that may participate in the response-cache key.
type requestGuardDecision struct {
	forwardBody []byte
	secrets     []string
	strongPath  []string
	weakPath    []string
}

func evaluateRequestGuard(cfg GuardConfig, scanner *guard.Scanner, body []byte) requestGuardDecision {
	decision := requestGuardDecision{forwardBody: body}
	if scanner == nil {
		return decision
	}
	action := cfg.SecretsAction()
	pathsOn := cfg.PathsAction() != "off"
	switch {
	case action == "redact" && pathsOn:
		// One scan claims the secret spans once; names and the redacted body
		// both come from that single claimed table (no second full scan for
		// Redact). Paths classify the POST-REDACT body (unchanged contract):
		// redaction rewrites bytes, so a pre-redact gate/classification
		// cannot be reused.
		decision.secrets, decision.forwardBody = scanner.ScanAndRedact(body)
		decision.strongPath, decision.weakPath = scanner.ScanPathsContext(decision.forwardBody)
	case action != "off" && pathsOn:
		// Both channels over the SAME body: one shared automaton pass — the
		// paths gate verdict rides the secrets scan's phase 1 instead of
		// paying ~10 per-literal bytes.Contains sweeps afterwards.
		decision.secrets, decision.strongPath, decision.weakPath = scanner.ScanSecretsAndPaths(body)
	case action != "off":
		if action == "redact" {
			decision.secrets, decision.forwardBody = scanner.ScanAndRedact(body)
		} else {
			decision.secrets = scanner.Scan(body)
		}
	case pathsOn:
		decision.strongPath, decision.weakPath = scanner.ScanPathsContext(decision.forwardBody)
	}
	return decision
}

func (d requestGuardDecision) blocks(cfg GuardConfig) (kind string, names []string) {
	if len(d.secrets) > 0 && cfg.SecretsAction() == "block" {
		return "secret", d.secrets
	}
	if len(d.strongPath) > 0 && cfg.PathsAction() == "block" {
		return "sensitive_path", d.strongPath
	}
	return "", nil
}
