package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"model-proxy/internal/admin"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

func TestAdminServiceReturnsDetachedSnapshots(t *testing.T) {
	p := newTestProxy(t, &Config{
		Listen: "127.0.0.1:1234",
		Providers: map[string]Provider{
			"up": {Provider: testProviderID, OpenAIBaseURL: "https://example.test"},
		},
	})
	p.routeWarnings = []string{"warning-one"}
	p.recordModelFailure("up", "m", Scheduling{ModelLockout: "1h"})

	ports := p.adminPorts(func() string { return "" }, nil, nil)
	service := admin.New(ports)
	dashboard := service.Dashboard(time.Now())
	providers := ports.ProviderConfigs()
	dashboard.Warnings[0] = "changed"
	providers["up"] = Provider{Provider: "changed"}

	p.mu.RLock()
	warning := p.routeWarnings[0]
	providerID := p.cfg.Providers["up"].Provider
	p.mu.RUnlock()
	if warning != "warning-one" || providerID != testProviderID {
		t.Fatalf("admin service leaked mutable runtime references: warning=%q provider=%q", warning, providerID)
	}
	if dashboard.Listen != "127.0.0.1:1234" ||
		len(dashboard.ModelLocks["up"]) != 1 {
		t.Fatalf("incomplete dashboard projection: %+v", dashboard)
	}
}

func TestAdminDashboardScheduleUsesCapturedRuntimeSnapshot(t *testing.T) {
	now := time.Date(2026, 7, 29, 18, 0, 0, 0, time.UTC)
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"a": {Provider: testProviderID},
			"b": {Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"m": {
			{Provider: "a", Model: "m", Priority: 1},
			{Provider: "b", Model: "m", Priority: 1},
		}},
	})
	p.runtimeState.SetQuota("a", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	}, 1)
	p.runtimeState.SetQuota("b", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		AsOf:    now,
	}, 1)
	p.runtimeState.SetPin("m", runtimestate.Pin{Provider: "a"})

	p.mu.RLock()
	cfg := p.cfg
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	poolIndex := p.poolIndex
	captured := p.runtimeState.Dashboard(now)
	p.mu.RUnlock()

	// Change the live Manager after capture. A schedule derived from captured
	// must retain pin a; a fresh dashboard must observe pin b.
	p.runtimeState.ClearPin("m")
	p.runtimeState.SetPin("m", runtimestate.Pin{Provider: "b"})

	type schedulePayload struct {
		Models map[string]struct {
			First string `json:"first"`
			Pin   string `json:"pin"`
		} `json:"models"`
	}
	decode := func(data []byte) schedulePayload {
		t.Helper()
		var payload schedulePayload
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("decode schedule: %v: %s", err, data)
		}
		return payload
	}

	old := decode(scheduleStatusFromSnapshot(
		cfg,
		expanded,
		parentOf,
		poolIndex,
		captured,
		now,
	))
	if got := old.Models["m"]; got.First != "a" || got.Pin != "a" {
		t.Fatalf("captured schedule mixed live state: %+v", got)
	}

	service := admin.New(p.adminPorts(func() string { return "" }, nil, nil))
	fresh := decode(service.Dashboard(now).Schedule)
	if got := fresh.Models["m"]; got.First != "b" || got.Pin != "b" {
		t.Fatalf("fresh schedule did not observe current state: %+v", got)
	}
}

// TestAdminModelCapsPortProjectsDetachedSnapshot: the ModelCapsSnapshot port
// reads the process-lifetime ModelStore (its own leaf lock, no p.mu) and
// returns a detached deep copy the admin projection can map freely.
func TestAdminModelCapsPortProjectsDetachedSnapshot(t *testing.T) {
	probed := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {Provider: testProviderID, OpenAIBaseURL: "https://example.test", Models: []string{"m1"}},
		},
	})

	// Empty store → empty, non-nil map (JSON {"providers":{}} downstream).
	ports := p.adminPorts(func() string { return "" }, nil, nil)
	if snapshot := ports.ModelCapsSnapshot(); snapshot == nil || len(snapshot) != 0 {
		t.Fatalf("empty store snapshot = %+v", snapshot)
	}

	p.modelCaps.Put("up", "0123456789abcdef", "m1",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triUnknown}, probed)

	snapshot := ports.ModelCapsSnapshot()
	caps, ok := snapshot["up"]
	if !ok || caps.Fingerprint != "0123456789abcdef" || !caps.ProbedAt.Equal(probed) {
		t.Fatalf("snapshot[up] = %+v", caps)
	}
	if mp := caps.Models["m1"]; mp.Chat != triYes || mp.Anthropic != triNo || mp.Responses != triUnknown {
		t.Errorf("snapshot matrix = %+v, want yes/no/unknown", mp)
	}

	// The projection must be detached: mutating it cannot touch the store.
	caps.Models["m1"] = runtimewire.ModelProtocols{Chat: triNo, Anthropic: triNo, Responses: triNo}
	snapshot["up"] = caps
	delete(snapshot, "up")
	mp, ok := p.modelCaps.Get("up", "m1")
	if !ok || mp.Chat != triYes || mp.Anthropic != triNo {
		t.Fatalf("port snapshot aliased the store: %+v ok=%v", mp, ok)
	}

	// The admin read method maps the same port to verdict strings.
	document := admin.New(ports).ModelsDocument()
	if got := document.Providers["up"].Models["m1"]; got.Chat != "yes" || got.Anthropic != "no" || got.Responses != "unknown" {
		t.Errorf("ModelsDocument = %+v, want yes/no/unknown strings", got)
	}
}

// ModelCapsReplace accepts the captured fingerprint while it still matches,
// replaces the matrix wholesale and preserves the fingerprint for boot restore.
func TestAdminModelCapsReplacePortStampsCurrentFingerprint(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {Provider: testProviderID, OpenAIBaseURL: "https://example.test", Models: []string{"m1"}},
		},
	})
	p.modelCaps.Put("up", "stale", "old-m",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triNo}, time.Now())

	ports := p.adminPorts(func() string { return "" }, nil, nil)
	ports.ModelCapsReplace("up", ports.ModelRefreshRuntime("up").Fingerprint, map[string]runtimewire.ModelProtocols{
		"m1": {Chat: triYes, Anthropic: triNo, Responses: triYes},
	})

	if _, ok := p.modelCaps.Get("up", "old-m"); ok {
		t.Error("replaced entry kept a model absent from the fresh matrix")
	}
	if mp, ok := p.modelCaps.Get("up", "m1"); !ok || mp.Responses != triYes {
		t.Errorf("replaced matrix m1 = %+v ok=%v", mp, ok)
	}
	wantFP := providerbuild.ProtocolConfigFingerprint(p.cfg.Providers["up"])
	if fp, _ := p.modelCaps.ProviderFingerprint("up"); fp != wantFP {
		t.Errorf("fingerprint = %q, want the current generation's %q", fp, wantFP)
	}
	// Unknown providers are dropped, not created.
	ports.ModelCapsReplace("ghost", "unused", map[string]runtimewire.ModelProtocols{"m": {Chat: triYes}})
	if _, ok := p.modelCaps.Get("ghost", "m"); ok {
		t.Error("ModelCapsReplace created an entry for an unconfigured provider")
	}
}

// ModelRefreshRuntime captures the named provider's impl, falling back to the
// credential pool's first virtual for pooled parents.
func TestAdminProviderImplPortResolvesPoolFallback(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {Provider: testProviderID, OpenAIBaseURL: "https://example.test", Models: []string{"m1"}},
		},
	})
	direct := &testProv{key: "direct"}
	virtual := &testProv{key: "pool"}
	p.providers["solo"] = direct
	// A pooled parent has NO direct impl — only virtuals (production shape).
	p.providers["pooled#a1"] = virtual
	p.poolIndex["pooled"] = []string{"pooled#a1"}

	ports := p.adminPorts(func() string { return "" }, nil, nil)
	if got := ports.ModelRefreshRuntime("solo").Provider; got != direct {
		t.Errorf("ProviderImpl(solo) = %v, want the direct impl", got)
	}
	if got := ports.ModelRefreshRuntime("pooled").Provider; got != virtual {
		t.Errorf("ProviderImpl(pooled) = %v, want the pool's first virtual", got)
	}
	if got := ports.ModelRefreshRuntime("ghost").Provider; got != nil {
		t.Errorf("ProviderImpl(ghost) = %v, want nil", got)
	}
}

func TestAdminModelRefreshRejectsPreReloadFingerprint(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(`{}`)) }))
	defer up.Close()
	t.Setenv("MP_MODELSDEV_URL", up.URL)
	file := filepath.Join(t.TempDir(), "config.yaml")
	write := func(base string) *Config {
		t.Helper()
		text := fmt.Sprintf("listen: 127.0.0.1:0\nproviders:\n  up: {provider_id: zhipu, openai_base_url: %s, models: [m]}\n", base)
		if err := os.WriteFile(file, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(file)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	p := newTestProxy(t, write(up.URL+"/old"))
	ports := p.adminPorts(func() string { return file }, nil, nil)
	captured := ports.ModelRefreshRuntime("up")
	write(up.URL + "/new")
	if err := p.Reload(file); err != nil {
		t.Fatal(err)
	}
	if captured.Config.Providers["up"].OpenAIBaseURL != up.URL+"/old" {
		t.Fatal("captured config mutated on reload")
	}
	matrix := map[string]runtimewire.ModelProtocols{"m": {Chat: triYes, Responses: triNo}}
	if ports.ModelCapsReplace("up", captured.Fingerprint, matrix) {
		t.Fatal("accepted old endpoint verdicts")
	}
	if _, ok := p.modelCaps.Get("up", "m"); ok {
		t.Fatal("old matrix reached current store")
	}
	fresh := ports.ModelRefreshRuntime("up")
	if fresh.Fingerprint == captured.Fingerprint || !ports.ModelCapsReplace("up", fresh.Fingerprint, matrix) {
		t.Fatal("fresh endpoint verdicts rejected")
	}
	got, ok := p.modelCaps.Get("up", "m")
	if !ok || got != matrix["m"] {
		t.Fatalf("fresh matrix = %+v, present=%v", got, ok)
	}
}
