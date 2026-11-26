package app

import (
	"testing"

	configdomain "model-proxy/internal/config"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
)

// TestInflightOutcomeMapsOntoTheLedger: the executor port's outcome reaches
// the runtime ledger unchanged, and the ledger keeps its rule through the app
// gate — only an upstream verdict moves an inferred cap's probe-up streak. A
// race for a freed slot leaves one request refused before dispatch; released
// as "no verdict" it costs the streak nothing, so twenty full-occupancy
// successes still raise the cap while requests keep losing races.
func TestInflightOutcomeMapsOntoTheLedger(t *testing.T) {
	for in, want := range map[targetexec.InflightOutcome]runtimestate.InflightOutcome{
		targetexec.InflightNoVerdict: runtimestate.InflightNoVerdict,
		targetexec.InflightFailed:    runtimestate.InflightFailed,
		targetexec.InflightSucceeded: runtimestate.InflightSucceeded,
	} {
		if got := inflightOutcome(in); got != want {
			t.Errorf("inflightOutcome(%v) = %v, want %v", in, got, want)
		}
	}

	proxy, _, _ := newReqLogProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{"a": {Provider: testProviderID}},
	})
	gate := proxyHealthGate{proxy: proxy}
	proxy.runtimeState.NoteConcurrencyLimit("a", 0, 1, proxy.runtimeState.Generation()) // an inferred cap of one
	// race is one freed slot contested by two requests through the gate: the
	// winner takes it at level 0 (full occupancy), the loser is refused at
	// level 1, then the winner succeeds.
	race := func(wantRaised bool) {
		t.Helper()
		winner, ok := gate.TakeInflight("a")
		if !ok {
			t.Fatal("the first take on a one-slot account was refused")
		}
		loser, ok := gate.TakeInflight("a")
		if ok {
			t.Fatal("the second take on a one-slot account was dispatchable")
		}
		if _, raised := gate.ReleaseInflight(loser, targetexec.InflightNoVerdict); raised {
			t.Fatal("a refused take raised the cap")
		}
		if _, raised := gate.ReleaseInflight(winner, targetexec.InflightSucceeded); raised != wantRaised {
			t.Fatalf("cap raised = %v on a full-occupancy success, want %v", raised, wantRaised)
		}
	}
	for i := 1; i < 20; i++ {
		race(false)
		if got := proxy.runtimeState.Concurrency("a").Cap; got != 1 {
			t.Fatalf("cap after race %d = %d, want 1 until 20 consecutive full-occupancy successes", i, got)
		}
	}
	race(true)
	if got := proxy.runtimeState.Concurrency("a"); got != (runtimestate.ConcurrencyStatus{Cap: 2}) {
		t.Fatalf("concurrency of a after 20 races = %+v, want the inferred cap probed up to 2", got)
	}

	// A dispatched request the upstream did not admit still resets the streak.
	generation := proxy.runtimeState.Generation()
	proxy.runtimeState.NoteConcurrencyLimit("a", 0, 1, generation)
	for i := 0; i < 19; i++ {
		race(false)
	}
	failed, _ := gate.TakeInflight("a")
	if _, raised := gate.ReleaseInflight(failed, targetexec.InflightFailed); raised {
		t.Fatal("a failed request raised the cap")
	}
	race(false)
	if got := proxy.runtimeState.Concurrency("a").Cap; got != 1 {
		t.Fatalf("cap after a failure and one success = %d, want 1 (the streak restarted from zero)", got)
	}
	if got := proxy.runtimeState.Concurrency("a").Inflight; got != 0 {
		t.Fatalf("in flight = %d, want every slot released", got)
	}
}
