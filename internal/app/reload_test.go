package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- reload_health_test.go ----

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

// TestReloadAppliedWarningMessageContract pins the message/Unwrap shape the
// SIGHUP log path (%v) and the admin port conversion rely on.
func TestReloadAppliedWarningMessageContract(t *testing.T) {
	err := &ReloadAppliedWarning{Err: errors.New("persist failed")}
	if got := err.Error(); got != "reload applied with warning: persist failed" {
		t.Errorf("message = %q", got)
	}
	if !errors.Is(err, err.Err) {
		t.Error("Unwrap must expose the inner error")
	}
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
	p.quota.Path = t.TempDir() // rename(temp, existing directory) must fail
	err := p.Reload(cfg2Path)
	var warning *ReloadAppliedWarning
	if !errors.As(err, &warning) {
		t.Fatalf("reload persist failure = %v, want ReloadAppliedWarning", err)
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
	seedRuntimeRateLimit(t, p, "zhipu", time.Now().Add(time.Hour), rlTransient)
	if err := p.quota.Persist(); err != nil {
		t.Fatal(err)
	}
	statePath := p.quota.Path

	// Reload to cfg2 (different provider/fingerprint) — clears health.
	if err := p.Reload(cfg2Path); err != nil {
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
	Providers map[string]runtimestate.PersistedQuotaSnapshot `json:"providers"`
	Sticky    map[string]runtimestate.Sticky                 `json:"sticky"`
	Health    map[string]runtimestate.PersistedHealth        `json:"health"`
	HealthFP  string                                         `json:"health_fp"`
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
	generation := p.configGeneration.Load()
	p.runtimeState.RecordRateLimit(
		providerName,
		time.Now().Add(time.Hour),
		rlTransient,
		generation,
	)
	p.runtimeState.SetSticky(
		"m",
		runtimestate.Sticky{Provider: providerName, Since: time.Now()},
		generation,
	)
	p.runtimeState.SetQuota(
		providerName,
		&provider.QuotaSnapshot{
			Billing:      provider.BillingPlan,
			RemainingPct: 0.5,
			AsOf:         time.Now(),
		},
		generation,
	)
}

func assertPersistedGeneration(t *testing.T, p *Proxy, providerName string, empty bool) {
	t.Helper()
	state := readPersistedRuntimeState(t, p.quota.Path)
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
	p.quota.Stop()
	seedRuntimeGeneration(p, "zhipu")
	if err := p.quota.Persist(); err != nil {
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
		if err := p.Reload(step.path); err != nil {
			t.Fatalf("reload %s: %v", step.provider, err)
		}
		assertPersistedGeneration(t, p, step.provider, true)
		seedRuntimeGeneration(p, step.provider)
		if err := p.quota.Persist(); err != nil {
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
	useStaticProviderPools(t, "p")
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
	proxyServer := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer proxyServer.Close()
	done := startProxyRequest(t, proxyServer.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation request did not reach upstream")
	}
	if err := p.Reload(cfg2Path); err != nil {
		t.Fatal(err)
	}
	close(release)
	oldResult := waitRequestDone(t, done)
	if oldResult.status != http.StatusBadGateway || !strings.Contains(oldResult.body, `all targets failed for model "m"`) {
		t.Fatalf("old failed request = status %d body %q, want 502 with target-failure diagnostic", oldResult.status, oldResult.body)
	}
	_, polluted := p.runtimeState.Dashboard(time.Now()).Providers["p"]
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
	useStaticProviderPools(t, "p")
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
	proxyServer := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer proxyServer.Close()
	done := startProxyRequest(t, proxyServer.URL)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation request did not reach upstream")
	}
	if err := p.Reload(cfg2Path); err != nil {
		t.Fatal(err)
	}
	wantUntil := time.Now().Add(time.Hour)
	p.runtimeState.RestoreHealth(
		map[string]runtimestate.PersistedHealth{"p": {CircuitOpenUntil: wantUntil}},
		time.Now(),
		1,
	)
	close(release)
	oldResult := waitRequestDone(t, done)
	if oldResult.status != http.StatusOK || oldResult.body != `{"ok":true}` {
		t.Fatalf("old successful request = status %d body %q, want 200/old body", oldResult.status, oldResult.body)
	}
	h, ok := p.runtimeState.Dashboard(time.Now()).Providers["p"]
	if !ok || h.ConsecutiveFailures != 1 || !h.CircuitOpenUntil.Equal(wantUntil) {
		t.Fatalf("old-generation success cleared new-generation health: %+v", h)
	}
}

// TestReload_RejectsAllDirectOldGenerationMutations complements the end-to-end
// request tests by covering every generation-guarded state family explicitly.
func TestReload_RejectsAllDirectOldGenerationMutations(t *testing.T) {
	useStaticProviderPools(t, "p")
	cfg1Path := writeConfigFile(t, routingConfig("http://127.0.0.1:1"))
	cfg2Path := writeConfigFile(t, routingConfig("http://127.0.0.1:2"))
	p := newTestProxy(t, mustLoadConfigFile(t, cfg1Path))
	oldGeneration := p.configGeneration.Load()
	if err := p.Reload(cfg2Path); err != nil {
		t.Fatal(err)
	}

	currentGeneration := p.configGeneration.Load()
	for range 7 {
		p.runtimeState.RecordFailure("p", 1, -time.Second, currentGeneration)
	}
	if !p.runtimeState.TakeHalfOpenSlot("p", currentGeneration) {
		t.Fatal("failed to seed current-generation half-open slot")
	}
	for range 4 {
		p.runtimeState.RecordModelFailure("p", "m", 2*time.Hour, currentGeneration)
	}
	if !p.runtimeState.LearnParamBlock("p", "m", "existing", currentGeneration) {
		t.Fatal("failed to seed current-generation parameter block")
	}
	// reload may have admitted its own poll. Drain it before replacing the
	// tracker, so the assertion below observes only a 429-triggered refresh.
	p.quota.Stop()
	refreshCalled := make(chan string, 1)
	p.providers["p"] = &quotaCountProv{name: "p", refreshed: refreshCalled}
	p.quota = runtimestate.NewQuotaTracker(
		t.TempDir()+"/quota_state.json",
		func() *Config { return p.cfg },
		func() map[string]provider.Provider { return p.providers },
		&p.runtimeState,
	)
	p.quota.Generation = p.configGeneration.Load

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
	p.quota.Stop()
	select {
	case name := <-refreshCalled:
		t.Fatalf("stale rate limit triggered quota refresh for %q", name)
	default:
	}

	snapshot := p.runtimeState.Dashboard(time.Now())
	h, ok := snapshot.Providers["p"]
	if !ok || h.ConsecutiveFailures != 7 || !h.HalfOpenInFlight || !h.RateLimitedUntil.IsZero() {
		t.Fatalf("old generation changed provider health: %+v", h)
	}
	locks := snapshot.ModelLocks["p"]
	if len(locks) != 1 || locks[0].Model != "m" || locks[0].Failures != 4 {
		t.Fatalf("old generation changed model lock: %+v", locks)
	}
	if !p.runtimeState.ParamBlocked("p", "m", "existing") ||
		p.runtimeState.ParamBlocked("p", "m", "stale") {
		t.Fatalf("old generation changed parameter blocklist: %+v", p.runtimeState.ParamBlock("p", "m"))
	}
}

// ---- security_log_reload_test.go ----

// Security audit log as reload-owned state (observe_adapters.go's
// reconcileSecLog): audit off→on starts logging at reload, on→off stops it,
// an audit_path change swaps files, and Close drains the current generation.
// Fixtures are synthetic (guardPoolKey matches no embedded rule).

// seclogRig wires a pool-backed provider behind a YAML config file so tests
// can Reload between audit configurations. reconcileSecLog stands in for
// StartRuntimeServices' boot reconcile (tests never call StartRuntimeServices
// — it would open stats/request-log/catalog services).
type seclogRig struct {
	proxy   *Proxy
	url     string
	upURL   string
	cfgPath string
}

func newSecLogRig(t *testing.T, audit bool, auditPath string) *seclogRig {
	t.Helper()
	setPoolHome(t, t.TempDir())
	writePoolFile(t, "zhipu", "zhipu", guardPoolKey)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"content":"ok"}}]}`))
	}))
	t.Cleanup(up.Close)
	rig := &seclogRig{upURL: up.URL, cfgPath: filepath.Join(t.TempDir(), "config.yaml")}
	rig.writeConfig(t, audit, auditPath)
	rig.proxy = newTestProxy(t, mustLoadConfigFile(t, rig.cfgPath))
	// Boot reconcile (StartRuntimeServices does this in production).
	rig.proxy.reconcileSecLog(rig.proxy.cfgSnapshot())
	px := httptest.NewServer(http.HandlerFunc(rig.proxy.Handler))
	t.Cleanup(px.Close)
	rig.url = px.URL
	return rig
}

// writeConfig rewrites the rig's YAML with the given audit settings.
func (r *seclogRig) writeConfig(t *testing.T, audit bool, auditPath string) {
	t.Helper()
	if err := os.WriteFile(r.cfgPath, []byte(r.configYAML(audit, auditPath)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// configYAML renders the rig's config for the given audit settings.
func (r *seclogRig) configYAML(audit bool, auditPath string) string {
	yaml := "listen: 127.0.0.1:0\n" +
		"providers:\n  zhipu:\n    openai_base_url: " + r.upURL + "\n    provider_id: zhipu\n" +
		"routes:\n  glm:\n    - {provider: zhipu, model: glm}\n" +
		"guard:\n  secrets: log\n"
	if audit {
		yaml += "  audit: true\n"
		if auditPath != "" {
			yaml += "  audit_path: " + auditPath + "\n"
		}
	} else {
		yaml += "  audit: false\n"
	}
	return yaml
}

// reload swaps the rig's config generation to the given audit settings.
func (r *seclogRig) reload(t *testing.T, audit bool, auditPath string) {
	t.Helper()
	r.writeConfig(t, audit, auditPath)
	if err := r.proxy.Reload(r.cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
}

// guardHit posts one body that hits the known-secret channel.
func (r *seclogRig) guardHit(t *testing.T) {
	t.Helper()
	postOK(t, r.url+"/v1/chat/completions", guardPoolRequestBody(guardPoolKey))
}

// reloadErr and guardHitErr are the goroutine-safe variants of reload/guardHit:
// they return errors instead of calling t.Fatal, which is only legal on the
// test goroutine.
func (r *seclogRig) reloadErr(audit bool, auditPath string) error {
	if err := os.WriteFile(r.cfgPath, []byte(r.configYAML(audit, auditPath)), 0o600); err != nil {
		return err
	}
	return r.proxy.Reload(r.cfgPath)
}

func (r *seclogRig) guardHitErr() error {
	resp, err := http.Post(r.url+"/v1/chat/completions", "application/json", stringReader(guardPoolRequestBody(guardPoolKey)))
	if err != nil {
		return err
	}
	b, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return readErr
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("guard hit: status=%d body=%s", resp.StatusCode, b)
	}
	return nil
}

// seclogRecordCount returns how many audit records dir holds (0 if absent).
func seclogRecordCount(t *testing.T, dir string) int {
	t.Helper()
	if _, err := os.Stat(dir); err != nil {
		return 0
	}
	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatalf("query %s: %v", dir, err)
	}
	return len(result.Records)
}

// audit off→on via reload takes effect immediately: hits before the reload
// persist nothing; the first hit after it lands in the audit log.
func TestSecLogReload_AuditOffToOn(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1") // fail catalog refresh fast
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, false, "")

	rig.guardHit(t)
	if got := seclogRecordCount(t, dir); got != 0 {
		t.Fatalf("audit off: records = %d, want 0", got)
	}

	rig.reload(t, true, filepath.Join(dir, "security.log"))
	if rig.proxy.SnapshotRuntime().SecLog == nil {
		t.Fatal("post-reload: snapshot must carry an audit logger (off→on)")
	}

	rig.guardHit(t)
	waitUntil(t, "first audit record after off→on reload", func() bool {
		return seclogRecordCount(t, dir) >= 1
	})
	result, err := seclog.Query(dir, seclog.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := result.Records[0]; got.Kind != seclog.KindSecret || got.Action != "log" {
		t.Errorf("first record = %+v, want kind=secret action=log", got)
	}
}

// audit on→off via reload stops new records at once: the old logger is drained
// during the reload, and post-reload hits write nothing.
func TestSecLogReload_AuditOnToOff(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, true, filepath.Join(dir, "security.log"))

	rig.guardHit(t)
	waitUntil(t, "audit record before the off reload", func() bool {
		return seclogRecordCount(t, dir) >= 1
	})
	before := seclogRecordCount(t, dir)

	rig.reload(t, false, "")
	if rig.proxy.SnapshotRuntime().SecLog != nil {
		t.Fatal("post-reload: snapshot logger must be nil (on→off)")
	}

	// Deterministic: the old logger was drained+stopped synchronously during
	// Reload, and the new generation's snapshot carries no logger.
	rig.guardHit(t)
	if got := seclogRecordCount(t, dir); got != before {
		t.Errorf("post-reload records = %d, want unchanged %d (on→off must stop writes)", got, before)
	}
}

// An audit_path change swaps files: new records land in the new directory and
// the old file is closed (its count never moves again).
func TestSecLogReload_AuditPathChange(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dirA := filepath.Join(t.TempDir(), "audit-a")
	dirB := filepath.Join(t.TempDir(), "audit-b")
	rig := newSecLogRig(t, true, filepath.Join(dirA, "security.log"))

	rig.guardHit(t)
	waitUntil(t, "audit record in the original directory", func() bool {
		return seclogRecordCount(t, dirA) >= 1
	})

	rig.reload(t, true, filepath.Join(dirB, "security.log"))
	logger := rig.proxy.SnapshotRuntime().SecLog
	if logger == nil || logger.Directory() != dirB {
		t.Fatalf("post-reload logger dir = %v, want %s", logger, dirB)
	}

	rig.guardHit(t)
	waitUntil(t, "audit record in the new directory", func() bool {
		return seclogRecordCount(t, dirB) >= 1
	})
	if got := seclogRecordCount(t, dirA); got != 1 {
		t.Errorf("old directory records = %d, want 1 (old file closed at swap)", got)
	}
}

// Reloads toggling audit racing concurrent guard hits: no data race (race
// detector), no deadlock, and the final generation converges.
func TestSecLogReload_ConcurrentHitsAndReloads(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dirA := filepath.Join(t.TempDir(), "audit-a")
	dirB := filepath.Join(t.TempDir(), "audit-b")
	rig := newSecLogRig(t, true, filepath.Join(dirA, "security.log"))

	stop := make(chan struct{})
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := rig.guardHitErr(); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			var err error
			switch i % 3 {
			case 0:
				err = rig.reloadErr(true, filepath.Join(dirA, "security.log"))
			case 1:
				err = rig.reloadErr(false, "")
			case 2:
				err = rig.reloadErr(true, filepath.Join(dirB, "security.log"))
			}
			if err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()

	// Run for a bounded number of full toggle cycles, then converge.
	waitUntil(t, "several audit reload cycles elapsed during the race", func() bool {
		return seclogRecordCount(t, dirA)+seclogRecordCount(t, dirB) >= 5
	})
	close(stop)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatal(err)
	default:
	}

	rig.reload(t, true, filepath.Join(dirB, "security.log"))
	before := seclogRecordCount(t, dirB)
	rig.guardHit(t)
	waitUntil(t, "post-race audit record in the final directory", func() bool {
		return seclogRecordCount(t, dirB) > before
	})
}

// Close drains the current generation's logger: every accepted record is
// flushed before Close returns, and nothing is written afterwards.
func TestSecLogReload_CloseDrainsCurrentGeneration(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://127.0.0.1:1")
	dir := filepath.Join(t.TempDir(), "audit")
	rig := newSecLogRig(t, true, filepath.Join(dir, "security.log"))

	const hits = 5
	for i := 0; i < hits; i++ {
		rig.guardHit(t)
	}
	logger := rig.proxy.SnapshotRuntime().SecLog
	if logger == nil {
		t.Fatal("snapshot must carry the audit logger")
	}
	rig.proxy.Close()

	// All hits were accepted before Close (enqueue is synchronous in forward),
	// so the drain must flush every one of them.
	if got := seclogRecordCount(t, dir); got != hits {
		t.Fatalf("records after Close = %d, want %d (Close must drain)", got, hits)
	}
	// A late enqueue against the drained logger is dropped, never written.
	forward.AuditGuardHit(logger, seclog.KindSecret, []string{"known_secret"}, "log", "late", "agent", "openai", "glm", "", "")
	if got := seclogRecordCount(t, dir); got != hits {
		t.Errorf("records after late enqueue = %d, want %d (no writes after Close)", got, hits)
	}
}
