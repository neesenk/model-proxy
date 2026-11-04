package stats

import "testing"

func TestGenerationWindowMs(t *testing.T) {
	cases := []struct {
		name         string
		totalMs      uint64
		ttftMs       uint64
		bodyReads    int
		outputTokens uint64
		want         uint64
	}{
		// Streamed call: TTFT well inside the call, body spread over many
		// reads, sane implied rate → the generation window is observable.
		{"streamed window", 4000, 1000, 40, 500, 3000},
		{"streamed small output", 2000, 1200, 6, 100, 800},
		// Buffered/non-streamed: no first-read stamp (ttft 0) → full call.
		{"missing ttft", 4000, 0, 40, 500, 4000},
		// Buffered: first byte is effectively the last (ttft ≥ duration) →
		// full call. Exact equality is the long-standing fallback.
		{"ttft swallows call", 4000, 4000, 1, 500, 4000},
		{"ttft beyond call", 4000, 9000, 1, 500, 4000},
		// Near-miss bulk: the regression this file exists for. A 415s call
		// (20596 output) whose first byte arrived 4ms before its last used to
		// read as a 4ms decode window → 5,149,000 tok/s.
		{"near-miss bulk few reads", 415379, 415375, 1, 20596, 415379},
		// The same body split by TCP into several reads is still a transfer
		// tail: the implied rate guard catches it.
		{"near-miss bulk split reads", 415379, 415375, 7, 20596, 415379},
		// A max_tokens-capped burst (8192 tokens landing in a 1.5s tail after
		// a 59s wait) implies ~4.9k tok/s — an artifact band observed live.
		{"burst tail artifact", 60145, 58673, 8, 8192, 60145},
		// Healthy small-window legs near the real rate ceiling (observed up
		// to ~950 tok/s) keep their windows.
		{"fast but plausible stream", 10447, 9686, 12, 745, 761},
		// Usage never parsed: the call's tokens are uncounted too, so the
		// observed window stands (cannot inflate any rate).
		{"unknown output keeps window", 3000, 1000, 10, 0, 2000},
		// No call time recorded at all.
		{"zero duration", 0, 500, 10, 500, 0},
	}
	for _, tc := range cases {
		if got := GenerationWindowMs(tc.totalMs, tc.ttftMs, tc.bodyReads, tc.outputTokens); got != tc.want {
			t.Errorf("GenerationWindowMs(%d,%d,%d,%d) = %d, want %d (%s)",
				tc.totalMs, tc.ttftMs, tc.bodyReads, tc.outputTokens, got, tc.want, tc.name)
		}
	}
}

func TestLegacyGenerationWindowMs(t *testing.T) {
	cases := []struct {
		name         string
		durationMs   uint64
		ttftMs       uint64
		outputTokens uint64
		want         uint64
	}{
		// Old streamed buckets: window survives.
		{"streamed window", 4000, 1000, 500, 3000},
		{"missing ttft", 4000, 0, 500, 4000},
		{"ttft swallows call", 4000, 4000, 500, 4000},
		// Legacy rows with bulk-shaped sums fall back via the rate bound: the
		// observed shopee pool day aggregated 112M output tokens over a 0.4%
		// window (≈11k tok/s implied) — full call instead.
		{"poisoned aggregate", 2490000000, 2480000000, 112285767, 2490000000},
		{"near-miss single", 3210, 3209, 112, 3210},
		{"zero duration", 0, 500, 500, 0},
	}
	for _, tc := range cases {
		if got := LegacyGenerationWindowMs(tc.durationMs, tc.ttftMs, tc.outputTokens); got != tc.want {
			t.Errorf("LegacyGenerationWindowMs(%d,%d,%d) = %d, want %d (%s)",
				tc.durationMs, tc.ttftMs, tc.outputTokens, got, tc.want, tc.name)
		}
	}
}
