package main

import (
	"encoding/json"
	"strings"
	"testing"

	"model-proxy/provider"
)

// --- billingClassName all tiers ---

func TestBillingClassName(t *testing.T) {
	if got := billingClassName(provider.BillingPlan); got != "plan" {
		t.Errorf("plan=%q", got)
	}
	if got := billingClassName(provider.BillingPayG); got != "pay-as-you-go" {
		t.Errorf("payg=%q", got)
	}
	if got := billingClassName(provider.BillingUnknown); got != "unknown" {
		t.Errorf("unknown=%q", got)
	}
}

// TestScheduleStatus_PoolGrouping verifies the /debug/schedule JSON makes the
// credential pool visible: each virtual in `ordered` carries a pool_parent
// marker, and the route carries a `pools` summary (parent + total accounts).
// Backward-compatible: existing fields (provider/priority/tier/surplus/...)
// remain unchanged.
func TestScheduleStatus_PoolGrouping(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "http://x", Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}},
		},
	}
	p := newTestProxy(t, cfg)
	data := p.scheduleStatus()

	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider   string `json:"provider"`
				PoolParent string `json:"pool_parent"`
				Priority   int    `json:"priority"`
				Tier       string `json:"tier"`
				Available  bool   `json:"available"`
			} `json:"ordered"`
			Pools []struct {
				Parent   string `json:"parent"`
				Accounts int    `json:"accounts"`
			} `json:"pools"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(data))
	}
	ri, ok := st.Models["glm-5.2"]
	if !ok {
		t.Fatalf("glm-5.2 missing from status; body=%s", string(data))
	}
	if len(ri.Ordered) != 3 {
		t.Fatalf("want 3 ordered virtuals, got %d (body=%s)", len(ri.Ordered), string(data))
	}
	for _, o := range ri.Ordered {
		if o.PoolParent != "zhipu" {
			t.Errorf("virtual %q: pool_parent=%q want zhipu", o.Provider, o.PoolParent)
		}
		if !strings.HasPrefix(o.Provider, "zhipu#") {
			t.Errorf("provider %q is not a virtual id", o.Provider)
		}
	}
	if len(ri.Pools) != 1 || ri.Pools[0].Parent != "zhipu" || ri.Pools[0].Accounts != 3 {
		t.Errorf("pools = %+v, want one pool {zhipu, 3 accounts}", ri.Pools)
	}
}

// TestScheduleStatus_NoPoolWhenSingle verifies pool_parent + pools are OMITTED
// for routes whose targets are not pooled (backward-compat: no spurious fields
// for non-pooled providers).
func TestScheduleStatus_NoPoolWhenSingle(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}},
		map[string][]RouteTarget{"m": {{Provider: "a", Priority: 1}}})
	staticSurplus(p, "a", 0.5, 0.5)
	var st struct {
		Models map[string]struct {
			Ordered []struct {
				Provider   string `json:"provider"`
				PoolParent string `json:"pool_parent"`
			} `json:"ordered"`
			Pools []struct {
				Parent string `json:"parent"`
			} `json:"pools"`
		} `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	m := st.Models["m"]
	if len(m.Ordered) != 1 || m.Ordered[0].Provider != "a" {
		t.Fatalf("ordered=%+v want [a]", m.Ordered)
	}
	if m.Ordered[0].PoolParent != "" {
		t.Errorf("non-pooled provider has pool_parent=%q want empty", m.Ordered[0].PoolParent)
	}
	if len(m.Pools) != 0 {
		t.Errorf("non-pooled route has pools=%+v want empty", m.Pools)
	}
}
