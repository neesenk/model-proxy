package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/provider"
)

func writeConfigFile(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustLoadConfigFile(t *testing.T, path string) *Config {
	t.Helper()
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config %s: %v", path, err)
	}
	return cfg
}

func TestReload_PersistFailureReturnsAppliedWarning(t *testing.T) {
	cfg1Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`)
	cfg2Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
`)
	cfg1 := mustLoadConfigFile(t, cfg1Path)
	p := newTestProxy(t, cfg1)
	p.quota.path = t.TempDir() // rename(temp, existing directory) must fail
	err := p.reload(cfg2Path)
	var warning *reloadAppliedWarning
	if !errors.As(err, &warning) {
		t.Fatalf("reload persist failure = %v, want reloadAppliedWarning", err)
	}
	if got := p.cfgSnapshot(); got.Providers["deepseek"].Provider != "deepseek" {
		t.Fatal("reload warning must still leave the new config live")
	}
}

// TestReload_PersistsClearedHealth (P1b): reload clears health and must persist
// the empty state + new fingerprint SYNCHRONOUSLY, so the on-disk file reflects
// the new generation the moment reload returns (the documented contract) — not
// left with the stale frozen entry until an async poll happens to fire. Before
// the fix reload only kicked `go pollAll`, so a crash in the window restored the
// old frozen state, and a concurrent persist could pair old health with the new
// fingerprint. (The immediate read below observes the synchronous persist;
// reload's async poll would still be blocked on the new provider's network
// Quota.)
func TestReload_PersistsClearedHealth(t *testing.T) {
	setPoolHome(t, t.TempDir())
	cfg1Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`)
	cfg2Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
`)
	cfg1 := mustLoadConfigFile(t, cfg1Path)
	p := newTestProxy(t, cfg1)

	// Freeze zhipu health, persist → disk carries zhipu + cfg1 fingerprint.
	p.healthMu.Lock()
	p.health["zhipu"] = &providerHealth{rateLimitedUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	if err := p.quota.persist(); err != nil {
		t.Fatal(err)
	}
	statePath := p.quota.path

	// Reload to cfg2 (different provider/fingerprint) — clears health.
	if err := p.reload(cfg2Path); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var wrap struct {
		Health   map[string]json.RawMessage `json:"health"`
		HealthFP string                     `json:"health_fp"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		t.Fatal(err)
	}
	if _, has := wrap.Health["zhipu"]; has {
		t.Errorf("stale zhipu health still on disk immediately after reload: %s", data)
	}
	cfg2 := mustLoadConfigFile(t, cfg2Path)
	if got, want := wrap.HealthFP, healthConfigFingerprint(cfg2); got != want {
		t.Errorf("health_fp after reload = %q, want cfg2 fingerprint %q", got, want)
	}
}

type persistedRuntimeState struct {
	Providers map[string]persistedSnapshot `json:"providers"`
	Sticky    map[string]persistedSticky   `json:"sticky"`
	Health    map[string]persistedHealth   `json:"health"`
	HealthFP  string                       `json:"health_fp"`
}

func readPersistedRuntimeState(t *testing.T, path string) persistedRuntimeState {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted state: %v", err)
	}
	var state persistedRuntimeState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("decode persisted state: %v", err)
	}
	return state
}

func seedRuntimeGeneration(p *Proxy, providerName string) {
	p.mu.Lock()
	p.healthMu.Lock()
	p.quota.mu.Lock()
	p.health = map[string]*providerHealth{
		providerName: {rateLimitedUntil: time.Now().Add(time.Hour)},
	}
	p.sticky = map[string]routeSticky{
		"m": {provider: providerName, since: time.Now()},
	}
	p.quota.state = map[string]*provider.QuotaSnapshot{
		providerName: {Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now()},
	}
	p.quota.stateGeneration = p.runtimeGeneration
	p.quota.mu.Unlock()
	p.healthMu.Unlock()
	p.mu.Unlock()
}

func assertPersistedGeneration(t *testing.T, p *Proxy, providerName string, empty bool) {
	t.Helper()
	state := readPersistedRuntimeState(t, p.quota.path)
	wantFP := healthConfigFingerprint(p.cfgSnapshot())
	if state.HealthFP != wantFP {
		t.Fatalf("health_fp = %q, want current generation %q", state.HealthFP, wantFP)
	}
	if empty {
		if len(state.Providers) != 0 || len(state.Health) != 0 || len(state.Sticky) != 0 {
			t.Fatalf("reload did not persist an empty current-generation state: %+v", state)
		}
		return
	}
	if len(state.Providers) != 1 || state.Providers[providerName].Billing != provider.BillingPlan {
		t.Fatalf("persisted quota does not belong to %q: %+v", providerName, state.Providers)
	}
	if len(state.Health) != 1 {
		t.Fatalf("persisted health does not belong to one generation: %+v", state.Health)
	}
	if _, ok := state.Health[providerName]; !ok {
		t.Fatalf("persisted health missing %q: %+v", providerName, state.Health)
	}
	if len(state.Sticky) != 1 || state.Sticky["m"].Provider != providerName {
		t.Fatalf("persisted sticky does not belong to %q: %+v", providerName, state.Sticky)
	}
}

func assertSnapshotGeneration(t *testing.T, state persistedFullSnapshot, cfg *Config, providerName string) {
	t.Helper()
	if got, want := state.HealthFP, healthConfigFingerprint(cfg); got != want {
		t.Fatalf("snapshot fingerprint = %q, want %q", got, want)
	}
	if len(state.Providers) != 1 || state.Providers[providerName].Billing != provider.BillingPlan {
		t.Fatalf("snapshot quota is mixed or missing for %q: %+v", providerName, state.Providers)
	}
	if len(state.Health) != 1 {
		t.Fatalf("snapshot health is mixed or missing for %q: %+v", providerName, state.Health)
	}
	if _, ok := state.Health[providerName]; !ok {
		t.Fatalf("snapshot health missing %q: %+v", providerName, state.Health)
	}
	if len(state.Sticky) != 1 || state.Sticky["m"].provider != providerName {
		t.Fatalf("snapshot sticky is mixed or missing for %q: %+v", providerName, state.Sticky)
	}
}

// TestPersistedSnapshot_BlocksGenerationSwap forces the exact lock-boundary
// interleaving: snapshot pauses after taking p.mu.RLock while a generation swap
// attempts p.mu.Lock. The captured tuple must be wholly old-generation; after
// the swap, a second tuple must be wholly new-generation.
func TestPersistedSnapshot_BlocksGenerationSwap(t *testing.T) {
	cfg1Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  m: [{provider: zhipu, model: m}]
`)
	cfg2Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
routes:
  m: [{provider: deepseek, model: m}]
`)
	cfg1 := mustLoadConfigFile(t, cfg1Path)
	cfg2 := mustLoadConfigFile(t, cfg2Path)
	p := newTestProxy(t, cfg1)
	p.quota.stop()
	seedRuntimeGeneration(p, "zhipu")

	snapshotHoldingConfig := make(chan struct{})
	releaseSnapshot := make(chan struct{})
	p.mu.Lock()
	p.persistSnapshotHook = func() {
		close(snapshotHoldingConfig)
		<-releaseSnapshot
	}
	p.mu.Unlock()
	snapshotDone := make(chan persistedFullSnapshot, 1)
	go func() { snapshotDone <- p.snapshotPersistedState() }()
	select {
	case <-snapshotHoldingConfig:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not reach the config-lock barrier")
	}

	swapAttempted := make(chan struct{})
	swapDone := make(chan struct{})
	go func() {
		close(swapAttempted)
		p.mu.Lock()
		p.healthMu.Lock()
		p.quota.mu.Lock()
		p.cfg = cfg2
		p.runtimeGeneration++
		p.configGeneration.Store(p.runtimeGeneration)
		p.health = map[string]*providerHealth{"deepseek": {rateLimitedUntil: time.Now().Add(time.Hour)}}
		p.sticky = map[string]routeSticky{"m": {provider: "deepseek", since: time.Now()}}
		p.quota.state = map[string]*provider.QuotaSnapshot{
			"deepseek": {Billing: provider.BillingPlan, RemainingPct: 0.4, AsOf: time.Now()},
		}
		p.quota.stateGeneration = p.runtimeGeneration
		p.quota.mu.Unlock()
		p.healthMu.Unlock()
		p.mu.Unlock()
		close(swapDone)
	}()
	<-swapAttempted
	select {
	case <-swapDone:
		t.Fatal("generation swap passed p.mu while snapshot held its read lock")
	default:
	}
	close(releaseSnapshot)

	var oldSnapshot persistedFullSnapshot
	select {
	case oldSnapshot = <-snapshotDone:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot did not finish after barrier release")
	}
	assertSnapshotGeneration(t, oldSnapshot, cfg1, "zhipu")
	select {
	case <-swapDone:
	case <-time.After(2 * time.Second):
		t.Fatal("generation swap did not finish after snapshot")
	}
	p.mu.Lock()
	p.persistSnapshotHook = nil
	p.mu.Unlock()
	assertSnapshotGeneration(t, p.snapshotPersistedState(), cfg2, "deepseek")
}

// TestReload_PersistedSnapshotMatchesGeneration deterministically verifies the
// complete persisted tuple. Each reload must first write the new fingerprint
// with empty quota/health/sticky, then a seeded current-generation snapshot must
// contain only that generation's provider keys.
func TestReload_PersistedSnapshotMatchesGeneration(t *testing.T) {
	setPoolHome(t, t.TempDir())
	cfg1Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  m: [{provider: zhipu, model: m}]
`)
	cfg2Path := writeConfigFile(t, `listen: 127.0.0.1:0
providers:
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
routes:
  m: [{provider: deepseek, model: m}]
`)
	cfg1 := mustLoadConfigFile(t, cfg1Path)
	p := newTestProxy(t, cfg1)
	// Disable background quota dispatch for this persistence-only test so the
	// synchronous empty snapshot observed immediately after reload is stable.
	p.quota.stop()
	seedRuntimeGeneration(p, "zhipu")
	if err := p.quota.persist(); err != nil {
		t.Fatal(err)
	}
	assertPersistedGeneration(t, p, "zhipu", false)

	steps := []struct {
		path, provider string
	}{
		{cfg2Path, "deepseek"},
		{cfg1Path, "zhipu"},
		{cfg2Path, "deepseek"},
	}
	for _, step := range steps {
		if err := p.reload(step.path); err != nil {
			t.Fatalf("reload %s: %v", step.provider, err)
		}
		assertPersistedGeneration(t, p, step.provider, true)
		seedRuntimeGeneration(p, step.provider)
		if err := p.quota.persist(); err != nil {
			t.Fatalf("persist %s generation: %v", step.provider, err)
		}
		assertPersistedGeneration(t, p, step.provider, false)
	}
}

func routingConfig(upstreamURL string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
providers:
  p: {provider_id: static, openai_base_url: %s}
routes:
  m: [{provider: p, model: m}]
scheduling:
  circuit_threshold: 1
`, upstreamURL)
}

type proxyRequestResult struct {
	status int
	body   string
	err    error
}

func startProxyRequest(t *testing.T, proxyURL string) <-chan proxyRequestResult {
	t.Helper()
	done := make(chan proxyRequestResult, 1)
	go func() {
		resp, err := http.Post(proxyURL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m","messages":[]}`))
		result := proxyRequestResult{err: err}
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			result.status = resp.StatusCode
			result.body = string(body)
			if readErr != nil {
				result.err = readErr
			} else if closeErr != nil {
				result.err = closeErr
			}
		}
		done <- result
	}()
	return done
}

func waitRequestDone(t *testing.T, done <-chan proxyRequestResult) proxyRequestResult {
	t.Helper()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("proxy request: %v", result.err)
		}
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("proxy request did not finish")
		return proxyRequestResult{}
	}
}

// TestReload_RejectsOldRequestFailureMutation exercises the real request path:
// an old-generation upstream failure arriving after reload must not repopulate
// the new generation's provider health, and subsequent traffic uses the new URL.
func TestReload_RejectsOldRequestFailureMutation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		http.Error(w, "old failure", http.StatusInternalServerError)
	}))
	defer oldUpstream.Close()
	newHit := make(chan struct{}, 1)
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		newHit <- struct{}{}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer newUpstream.Close()

	cfg1Path := writeConfigFile(t, routingConfig(oldUpstream.URL))
	cfg2Path := writeConfigFile(t, routingConfig(newUpstream.URL))
	p := newTestProxy(t, mustLoadConfigFile(t, cfg1Path))
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handler))
	defer proxyServer.Close()
	done := startProxyRequest(t, proxyServer.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation request did not reach upstream")
	}
	if err := p.reload(cfg2Path); err != nil {
		t.Fatal(err)
	}
	close(release)
	oldResult := waitRequestDone(t, done)
	if oldResult.status != http.StatusBadGateway || !strings.Contains(oldResult.body, `all targets failed for model "m"`) {
		t.Fatalf("old failed request = status %d body %q, want 502 with target-failure diagnostic", oldResult.status, oldResult.body)
	}
	p.healthMu.Lock()
	_, polluted := p.health["p"]
	p.healthMu.Unlock()
	if polluted {
		t.Fatal("old-generation failure repopulated new-generation health")
	}
	newResult := waitRequestDone(t, startProxyRequest(t, proxyServer.URL))
	if newResult.status != http.StatusOK || newResult.body != `{"ok":true}` {
		t.Fatalf("post-reload request = status %d body %q, want 200/new body", newResult.status, newResult.body)
	}
	select {
	case <-newHit:
	default:
		t.Fatal("post-reload request did not hit the new upstream")
	}
}

// TestReload_RejectsOldRequestSuccessMutation covers the reverse corruption:
// stale success must not clear a failure recorded by the new generation.
func TestReload_RejectsOldRequestSuccessMutation(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer oldUpstream.Close()
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer newUpstream.Close()

	cfg1Path := writeConfigFile(t, routingConfig(oldUpstream.URL))
	cfg2Path := writeConfigFile(t, routingConfig(newUpstream.URL))
	p := newTestProxy(t, mustLoadConfigFile(t, cfg1Path))
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handler))
	defer proxyServer.Close()
	done := startProxyRequest(t, proxyServer.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation request did not reach upstream")
	}
	if err := p.reload(cfg2Path); err != nil {
		t.Fatal(err)
	}
	wantUntil := time.Now().Add(time.Hour)
	p.healthMu.Lock()
	p.health["p"] = &providerHealth{consecutiveFailures: 1, circuitOpenUntil: wantUntil}
	p.healthMu.Unlock()
	close(release)
	oldResult := waitRequestDone(t, done)
	if oldResult.status != http.StatusOK || oldResult.body != `{"ok":true}` {
		t.Fatalf("old successful request = status %d body %q, want 200/old body", oldResult.status, oldResult.body)
	}
	p.healthMu.Lock()
	h := p.health["p"]
	p.healthMu.Unlock()
	if h == nil || h.consecutiveFailures != 1 || !h.circuitOpenUntil.Equal(wantUntil) {
		t.Fatalf("old-generation success cleared new-generation health: %+v", h)
	}
}

// TestReload_RejectsAllDirectOldGenerationMutations complements the end-to-end
// request tests by covering every generation-guarded state family explicitly.
func TestReload_RejectsAllDirectOldGenerationMutations(t *testing.T) {
	cfg1Path := writeConfigFile(t, routingConfig("http://127.0.0.1:1"))
	cfg2Path := writeConfigFile(t, routingConfig("http://127.0.0.1:2"))
	p := newTestProxy(t, mustLoadConfigFile(t, cfg1Path))
	oldGeneration := p.configGeneration.Load()
	if err := p.reload(cfg2Path); err != nil {
		t.Fatal(err)
	}

	key := modelLockKey{provider: "p", model: "m"}
	wantCircuit := time.Now().Add(time.Hour)
	wantModelLock := time.Now().Add(2 * time.Hour)
	p.healthMu.Lock()
	p.health["p"] = &providerHealth{consecutiveFailures: 7, circuitOpenUntil: wantCircuit, halfOpenInFlight: true}
	p.modelLocks[key] = &modelLockEntry{failures: 4, lockedUntil: wantModelLock}
	p.paramBlock[key] = map[string]bool{"existing": true}
	p.healthMu.Unlock()

	p.recordFailure("p", Scheduling{}, oldGeneration)
	p.recordModelFailure("p", "m", Scheduling{}, oldGeneration)
	p.recordRateLimit("p", time.Now().Add(3*time.Hour), rlQuota, oldGeneration)
	p.recordSuccess("p", "m", oldGeneration)
	p.releaseHalfOpenSlot("p", oldGeneration)
	if learned := p.learnParamBlock("p", "m", "stale", oldGeneration); learned {
		t.Fatal("old generation reported a newly learned parameter")
	}
	if allowed := p.takeHalfOpenSlot("p", oldGeneration); !allowed {
		t.Fatal("old request should be allowed to finish without mutating the new generation")
	}

	p.healthMu.Lock()
	defer p.healthMu.Unlock()
	h := p.health["p"]
	if h == nil || h.consecutiveFailures != 7 || !h.circuitOpenUntil.Equal(wantCircuit) || !h.halfOpenInFlight || !h.rateLimitedUntil.IsZero() {
		t.Fatalf("old generation changed provider health: %+v", h)
	}
	lock := p.modelLocks[key]
	if lock == nil || lock.failures != 4 || !lock.lockedUntil.Equal(wantModelLock) {
		t.Fatalf("old generation changed model lock: %+v", lock)
	}
	if !p.paramBlock[key]["existing"] || p.paramBlock[key]["stale"] {
		t.Fatalf("old generation changed parameter blocklist: %+v", p.paramBlock[key])
	}
}
