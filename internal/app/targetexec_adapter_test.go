package app

import (
	"net/http"
	"testing"
	"time"

	"model-proxy/internal/targetexec"
)

// TestCommittedTTFTStaleGenerationDropped (A3 regression): a committed 2xx
// from a PRE-RELOAD in-flight request must not fold its TTFT into the new
// generation's quality EWMA. Pre-fix Committed called recordAttemptQuality
// without a generation, so GenerationArg(nil)=0 made the gate always-true and
// stale TTFT samples polluted the post-reload quality map (the error-rate
// samples were already gated — only this path leaked).
func TestCommittedTTFTStaleGenerationDropped(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{},
		Routes:    map[string][]RouteTarget{},
	})

	committed := targetexec.AttemptDTO{
		Target:           RouteTarget{Provider: "p", Model: "m"},
		Response:         &http.Response{StatusCode: http.StatusOK},
		TTFTMilliseconds: 500,
	}

	// The request started under generation 1 (the constructor's generation);
	// a reload moves the manager to generation 2 while it is in flight.
	stale := targetExecutionEffects{proxy: p, generation: 1}
	p.runtimeState.ReplaceGeneration(2)
	stale.Committed(committed)
	if q := p.runtimeState.Dashboard(time.Now()).Quality; len(q) != 0 {
		t.Fatalf("stale-generation TTFT wrote into the new quality map: %+v", q)
	}

	// Control: a commit on the CURRENT generation lands (decayed by the tiny
	// record→dashboard interval, so assert a tight window around 500ms).
	fresh := targetExecutionEffects{proxy: p, generation: 2}
	fresh.Committed(committed)
	if got := p.runtimeState.Dashboard(time.Now()).Quality["p"].TTFTMilliseconds; got < 490 || got > 500 {
		t.Fatalf("current-generation TTFT = %dms, want ~500ms recorded", got)
	}
}
