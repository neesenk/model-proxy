package main

import (
	"testing"
	"time"
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
