package app

import (
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
)

// TestTakeoverAuthProvidersCache_ComputesOncePerGeneration pins the memoizer
// contract behind the takeover read/preview paths: repeated reads within one
// config generation compute the offline authenticated-provider projection
// exactly once, and a generation bump forces exactly one recompute. A fake
// counting compute stands in for the BuildProviders pass (no credential
// stores are touched).
func TestTakeoverAuthProvidersCache_ComputesOncePerGeneration(t *testing.T) {
	gen := uint64(1)
	calls := 0
	c := &takeoverAuthProvidersCache{
		generation: func() uint64 { return gen },
		compute: func() map[string]bool {
			calls++
			return map[string]bool{"provider-a": true}
		},
	}
	if got := c.Get(); !got["provider-a"] {
		t.Fatalf("first Get = %v, want provider-a authenticated", got)
	}
	c.Get()
	c.Get()
	if calls != 1 {
		t.Fatalf("same-generation Gets computed %d times, want 1", calls)
	}
	gen = 2
	c.Get()
	if calls != 2 {
		t.Fatalf("post-reload Get computed %d times, want 2 (one recompute)", calls)
	}
	c.Get()
	if calls != 2 {
		t.Fatalf("second-generation repeat computed %d times, want 2 (cached again)", calls)
	}
}

// TestTakeoverAuthProvidersCache_ReloadDuringComputeStaysUncached pins the
// interleaving guard: when the generation moves between the pre-compute read
// and the post-compute check, the result is returned but never cached, so no
// generation can be served a projection that mixes two config states.
func TestTakeoverAuthProvidersCache_ReloadDuringComputeStaysUncached(t *testing.T) {
	gen := uint64(1)
	calls := 0
	c := &takeoverAuthProvidersCache{
		generation: func() uint64 { return gen },
		compute: func() map[string]bool {
			calls++
			gen = 2 // a reload lands mid-compute
			return map[string]bool{"provider-a": true}
		},
	}
	c.Get()
	if calls != 1 {
		t.Fatalf("compute calls = %d, want 1", calls)
	}
	// The mixed-generation result was not cached: the next Get recomputes.
	c.Get()
	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2 (mixed result must not be cached)", calls)
	}
	// Now generation 2 is stable: cached from here on.
	c.Get()
	if calls != 2 {
		t.Fatalf("compute calls = %d, want 2 (stable generation cached)", calls)
	}
}

// TestTakeoverAuthProvidersCache_ConcurrentGetsSerializeCache is the race
// guard for the memoizer: concurrent first-touch Gets must all return a
// valid set and leave the cache in a consistent state (run under -race).
func TestTakeoverAuthProvidersCache_ConcurrentGetsSerializeCache(t *testing.T) {
	calls := make(chan struct{}, 64)
	c := &takeoverAuthProvidersCache{
		generation: func() uint64 { return 1 },
		compute: func() map[string]bool {
			calls <- struct{}{}
			return map[string]bool{"provider-a": true}
		},
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got := c.Get(); !got["provider-a"] {
				t.Errorf("Get = %v, want provider-a authenticated", got)
			}
		}()
	}
	wg.Wait()
	// Concurrent first-touch may compute more than once (allowed and cheap to
	// reason about), but after the dust settles the cache must serve without
	// further computes.
	before := len(calls)
	if got := c.Get(); !got["provider-a"] {
		t.Fatalf("Get = %v, want provider-a authenticated", got)
	}
	if len(calls) != before {
		t.Fatalf("settled cache recomputed: computes %d -> %d", before, len(calls))
	}
}

// TestAdminPortsWiresTakeoverAuthCache pins the composition-root wiring: the
// admin takeover surface must receive the generation-cached projection, not
// the per-call compute.
func TestAdminPortsWiresTakeoverAuthCache(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{
		Listen:    "127.0.0.1:1234",
		Providers: map[string]configdomain.Provider{},
	})
	ports := p.adminPorts(func() string { return "" }, nil, nil, nil)
	if ports.TakeoverAuthenticatedProviders == nil {
		t.Fatal("adminPorts must wire TakeoverAuthenticatedProviders (the per-generation cache)")
	}
}

// TestDisabledModelSet_ExpandsPoolVirtualKeys pins the refresh-side parity
// with TargetDisabled: a model disabled under a pool virtual key is excluded
// for the whole parent, not just that account.
func TestDisabledModelSet_ExpandsPoolVirtualKeys(t *testing.T) {
	byProvider := map[string][]string{
		"zhipu#acct1": {"m2"},
	}
	set := disabledModelSet(byProvider, []string{"zhipu#acct1", "zhipu#acct2"}, "zhipu")
	if !set["m2"] {
		t.Fatalf("virtual-key disabled model missing from parent set: %v", set)
	}
	if got := disabledModelSet(map[string][]string{}, nil, "zhipu"); got != nil {
		t.Fatalf("empty store = %v, want nil", got)
	}
	if got := disabledModelSet(map[string][]string{"zhipu": {"m1"}}, nil, "zhipu"); !got["m1"] || len(got) != 1 {
		t.Fatalf("parent-only set = %v, want exactly {m1}", got)
	}
}
