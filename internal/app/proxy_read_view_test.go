package app

import (
	"encoding/json"
	"testing"
	"time"

	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
)

func TestProxyReadViewReturnsDetachedSnapshots(t *testing.T) {
	p := newTestProxy(t, &Config{
		Listen: "127.0.0.1:1234",
		Providers: map[string]Provider{
			"up": {Provider: testProviderID, OpenAIBaseURL: "https://example.test"},
		},
	})
	p.routeWarnings = []string{"warning-one"}
	p.recordModelFailure("up", "m", Scheduling{ModelLockout: "1h"})

	view := p.readView()
	dashboard := view.dashboard(time.Now())
	providers := view.providerConfigs()
	dashboard.warnings[0] = "changed"
	providers["up"] = Provider{Provider: "changed"}

	p.mu.RLock()
	warning := p.routeWarnings[0]
	providerID := p.cfg.Providers["up"].Provider
	p.mu.RUnlock()
	if warning != "warning-one" || providerID != testProviderID {
		t.Fatalf("read view leaked mutable runtime references: warning=%q provider=%q", warning, providerID)
	}
	if dashboard.listen != "127.0.0.1:1234" ||
		len(dashboard.modelLocks["up"]) != 1 {
		t.Fatalf("incomplete read view: %+v", dashboard)
	}
}

func TestProxyReadViewDashboardScheduleUsesCapturedRuntimeSnapshot(t *testing.T) {
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

	fresh := decode(p.readView().dashboard(now).schedule)
	if got := fresh.Models["m"]; got.First != "b" || got.Pin != "b" {
		t.Fatalf("fresh schedule did not observe current state: %+v", got)
	}
}
