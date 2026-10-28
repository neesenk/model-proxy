package counters

// adopt_test.go — coverage for the runtime provider-key adoption (the
// in-memory half of a bare-name → pooled-virtual-key rename on a live
// pipeline): values merge ADDITIVELY into existing target entries, the source
// key owns nothing afterwards, and concurrent hot-path increments on the
// target key are never lost.

import (
	"sync"
	"testing"
)

func TestAdoptProviderKey_MergesIntoTargetAndDropsSource(t *testing.T) {
	metrics := NewMetricsStore()
	tokens := NewTokenCounter()
	agents := NewAgentCounter()

	// Bare-key era under "p": two (provider, model) entries, one agent entry.
	metrics.Seed(PMKey{Provider: "p", Model: "m1"}, ProviderMetricsSnapshot{
		Requests: 100, Failures: 2, LatencySum: 5_000, LastRequestAt: 100,
	})
	metrics.Seed(PMKey{Provider: "p", Model: "m2"}, ProviderMetricsSnapshot{Requests: 7})
	tokens.Seed(TokenKey{Provider: "p", Model: "m1"}, TokenUsage{Input: 400, Output: 100, Requests: 20})
	agents.Seed(AgentKey{Agent: "pi", Provider: "p", Model: "m1"}, AgentCount{Requests: 30, Input: 400})
	// New-key era: "p#acc" already serves m1 traffic.
	metrics.Seed(PMKey{Provider: "p#acc", Model: "m1"}, ProviderMetricsSnapshot{Requests: 3, LastRequestAt: 50})
	tokens.Seed(TokenKey{Provider: "p#acc", Model: "m1"}, TokenUsage{Input: 7, Requests: 2})
	agents.Seed(AgentKey{Agent: "pi", Provider: "p#acc", Model: "m1"}, AgentCount{Requests: 3})

	if got := metrics.AdoptProviderKey("p", "p#acc"); got != 2 {
		t.Fatalf("metrics moved %d entries, want 2", got)
	}
	if got := tokens.AdoptProviderKey("p", "p#acc"); got != 1 {
		t.Fatalf("tokens moved %d entries, want 1", got)
	}
	if got := agents.AdoptProviderKey("p", "p#acc"); got != 1 {
		t.Fatalf("agents moved %d entries, want 1", got)
	}

	pm := metrics.Snapshot()
	if got := pm[PMKey{Provider: "p#acc", Model: "m1"}]; got.Requests != 103 || got.Failures != 2 ||
		got.LatencySum != 5_000 || got.LastRequestAt != 100 {
		t.Fatalf("metrics m1 = %+v, want requests=103 failures=2 latency=5000 last=100 (timestamp keeps the max)", got)
	}
	if got := pm[PMKey{Provider: "p#acc", Model: "m2"}]; got.Requests != 7 {
		t.Fatalf("metrics m2 = %+v, want 7 requests carried", got)
	}
	if _, bare := pm[PMKey{Provider: "p", Model: "m1"}]; bare {
		t.Error("bare metrics entry survived the adoption")
	}

	tu := tokens.Snapshot()[TokenKey{Provider: "p#acc", Model: "m1"}]
	if tu.Input != 407 || tu.Output != 100 || tu.Requests != 22 {
		t.Fatalf("tokens = %+v, want input=407 output=100 requests=22", tu)
	}
	if _, bare := tokens.Snapshot()[TokenKey{Provider: "p", Model: "m1"}]; bare {
		t.Error("bare token entry survived the adoption")
	}

	ac := agents.Snapshot()[AgentKey{Agent: "pi", Provider: "p#acc", Model: "m1"}]
	if ac.Requests != 33 || ac.Input != 400 {
		t.Fatalf("agents = %+v, want requests=33 input=400", ac)
	}
	if _, bare := agents.Snapshot()[AgentKey{Agent: "pi", Provider: "p", Model: "m1"}]; bare {
		t.Error("bare agent entry survived the adoption")
	}

	// Idempotence and guards: a second pass (no source rows) and degenerate
	// key arguments are no-ops.
	if got := metrics.AdoptProviderKey("p", "p#acc"); got != 0 {
		t.Fatalf("second adoption moved %d entries, want 0", got)
	}
	if got := tokens.AdoptProviderKey("p", "p#acc"); got != 0 {
		t.Fatalf("second token adoption moved %d entries, want 0", got)
	}
	if got := agents.AdoptProviderKey("p", "p#acc"); got != 0 {
		t.Fatalf("second agent adoption moved %d entries, want 0", got)
	}
	if got := metrics.AdoptProviderKey("", "x"); got != 0 {
		t.Fatalf("empty from moved %d entries, want 0", got)
	}
	if got := metrics.AdoptProviderKey("x", "x"); got != 0 {
		t.Fatalf("from==to moved %d entries, want 0", got)
	}
}

func TestAdoptProviderKey_ConcurrentTargetIncrementsAreKept(t *testing.T) {
	metrics := NewMetricsStore()
	tokens := NewTokenCounter()
	metrics.Seed(PMKey{Provider: "p", Model: "m"}, ProviderMetricsSnapshot{Requests: 1000})
	tokens.Seed(TokenKey{Provider: "p", Model: "m"}, TokenUsage{Input: 1000})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					metrics.Inc("p#acc", "m", EvRequests)
					tokens.Commit(TokenKey{Provider: "p#acc", Model: "m"}, TokenUsage{Input: 1})
				}
			}
		}()
	}
	if got := metrics.AdoptProviderKey("p", "p#acc"); got != 1 {
		t.Fatalf("metrics moved %d, want 1", got)
	}
	if got := tokens.AdoptProviderKey("p", "p#acc"); got != 1 {
		t.Fatalf("tokens moved %d, want 1", got)
	}
	close(stop)
	wg.Wait()

	// The adoption adds to the target; an overwrite-style regressor would
	// reset the target to the bare-era value (1000 exactly, or near it while
	// losing concurrent adds that raced the read).
	if got := metrics.Snapshot()[PMKey{Provider: "p#acc", Model: "m"}].Requests; got < 1000 {
		t.Fatalf("target requests = %d, want >= 1000 (adoption lost live increments)", got)
	}
	if got := tokens.Snapshot()[TokenKey{Provider: "p#acc", Model: "m"}].Input; got < 1000 {
		t.Fatalf("target input = %d, want >= 1000 (adoption lost live increments)", got)
	}
	if _, bare := metrics.Snapshot()[PMKey{Provider: "p", Model: "m"}]; bare {
		t.Error("bare metrics entry survived the adoption")
	}
}
