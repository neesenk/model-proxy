package app

import (
	"log"
	"sort"
	"time"
)

// guardSecretRefreshLoop periodically re-syncs the guard known-secret set with
// the on-disk codex/aqp OAuth auth files (those providers refresh and rewrite
// <name>_oauth_auth.json in place during serve). The beat is the CURRENT
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
// token values for the current config generation and, when the set changed,
// rebuild the guard scanner and swap it in place.
//
// Locking: the generation snapshot (cfg, pool/OAuth secret subsets) is taken
// under a brief read lock; all file I/O and scanner construction happen
// OUTSIDE p.mu; the write lock below only swaps pointers, and only when the
// generation captured before the I/O is still current. A reload that
// interleaved already installed its own scanner and secret subsets, so a
// stale rebuild is dropped — scanner contents never mix generations.
func (p *Proxy) refreshGuardKnownSecrets() {
	p.mu.RLock()
	cfg := p.cfg
	generation := p.configGeneration.Load()
	poolSecrets := p.guardPoolSecrets
	prevOAuth := p.guardOAuthSecrets
	p.mu.RUnlock()

	if cfg == nil || !cfg.Guard.KnownSecretsEnabled() {
		// known_secrets: false means the scanner carries no credential values
		// at all — nothing to re-sync.
		return
	}
	freshOAuth := CollectOAuthSecrets(cfg, buildOpts())
	if equalSecretSets(freshOAuth, prevOAuth) {
		return
	}
	// Fresh concatenation: poolSecrets is reload-owned state shared with the
	// current scanner's build inputs — never append into its backing array.
	scanner, err := buildGuardScanner(cfg, append(append([]string(nil), poolSecrets...), freshOAuth...))
	if err != nil {
		// Only reachable with an unvalidated Config; keep the previous scanner.
		log.Printf("[guard] OAuth secret re-sync: rebuild failed: %v (keeping previous scanner)", err)
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
