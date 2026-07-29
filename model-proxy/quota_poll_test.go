package main

import (
	"testing"
	"time"

	"model-proxy/provider"
)

// TestPollAll_PollsPooledVirtuals (bug 1): a multi-account provider is unrolled
// into "name#<accountID>" virtuals in the RUNTIME map; the parent name is NOT a
// runtime key. pollAll must iterate the runtime map (the runnable instances) —
// iterating cfg.Providers (parent names) looked the parent up and found nil, so
// every pooled account stayed BillingUnknown and was never polled, defeating
// surplus/tier scheduling and persistence for the whole pool.
func TestPollAll_PollsPooledVirtuals(t *testing.T) {
	// cfg.Providers carries only the PARENT name, exactly as a pooled provider
	// appears in config; the runtime map carries the unrolled virtuals.
	cfg := &Config{Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}
	provs := map[string]provider.Provider{
		"zhipu#a": &testProv{key: "zhipu#a"},
		"zhipu#b": &testProv{key: "zhipu#b"},
	}
	tr := newStandaloneQuotaTracker("", func() *Config { return cfg }, func() map[string]provider.Provider { return provs })
	tr.pollAll(time.Now())
	for _, vid := range []string{"zhipu#a", "zhipu#b"} {
		if tr.snapshot(vid) == nil {
			t.Errorf("%s: pooled virtual has no snapshot after pollAll (poller skipped it)", vid)
		}
	}
	// The parent name is not runnable and must NOT be polled as a key.
	if tr.snapshot("zhipu") != nil {
		t.Errorf("parent name should not appear as a polled runtime key")
	}
}
