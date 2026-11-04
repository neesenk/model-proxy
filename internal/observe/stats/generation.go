// generation.go owns the tok/s denominator: the per-call generation window.
//
// No LLM protocol defines tok/s — usage events carry token counts only, with
// no timestamps — so the rate is a proxy-side observation: output tokens over
// the window in which the response body actually arrived (duration minus the
// pre-first-token wait). That is only meaningful when the body arrived spread
// out; when it arrived in bulk (non-streamed JSON, an upstream or proxy chain
// flushing a buffered stream in one tail, or a long-prompt call whose whole
// output lands at the end) the residual window is body TRANSFER time, and
// tokens/transfer-time is not a decode speed — it produced 5,149,000 tok/s
// readings on a 415s call whose first byte arrived 4ms before its last.
//
// The defined fallback (decision: observable → window, else full call) keeps
// the metric a conservative lower bound instead of a fabrication: a bulk
// arrival's full call duration covers prompt processing plus generation, so
// output/duration still bounds the decode speed from below.
package stats

// MinStreamedBodyReads is the write-side arrival-shape test: a response whose
// body reached the client in fewer distinct reads arrived in bulk, and its
// decode window is unobservable. Real streamed responses deliver dozens of
// reads over their window; a bulk body lands in 1-3 reads (32KB copy-buffer
// granularity).
const MinStreamedBodyReads = 4

// MaxPlausibleTokensPerSecond bounds believable decode rates. Calibration
// (request log, 7 days, real usage tokens): streamed legs sit overwhelmingly
// in 100-500 tok/s; artifacts (bulk tails whose output rides a sub-2s window,
// e.g. max_tokens-capped bursts with TTFT at 95-99% of the call) smear from
// ~500 up to millions. 2000 keeps 4x headroom over the observed real ceiling
// while clipping the artifact band. The guard only ever WIDENS the window.
const MaxPlausibleTokensPerSecond = 2000

// GenerationWindowMs is the authoritative per-call generation window, decided
// at commit time when the arrival shape is still observable.
//
//	bodyReads     distinct body reads served to the client (copy loop count)
//	outputTokens  usage output tokens, 0 when usage was never parsed
//
// Bulk shape (few reads), missing TTFT, or a TTFT that swallows the call fall
// back to the full duration. With a streamed shape, an implied rate beyond the
// plausibility bound means the "window" is a transfer tail that merely landed
// across several reads (e.g. one flushed body split by TCP) — also full
// duration. Unknown output (0) keeps the observed window: the call's tokens
// are uncounted too, so it cannot inflate any rate.
func GenerationWindowMs(totalMs, ttftMs uint64, bodyReads int, outputTokens uint64) uint64 {
	if totalMs == 0 {
		return 0
	}
	if ttftMs == 0 || ttftMs >= totalMs || bodyReads < MinStreamedBodyReads {
		return totalMs
	}
	window := totalMs - ttftMs
	if impliedTokensPerSecond(outputTokens, window) > MaxPlausibleTokensPerSecond {
		return totalMs
	}
	return window
}

// LegacyGenerationWindowMs reconstructs a window from persisted bucket sums
// for rows written before gen_ms_sum existed (no read count survived). Same
// fallback semantics, with the plausibility bound as the only bulk detector.
// Exact for purely streamed or purely bulk buckets; mixed buckets get the
// coarse all-or-nothing treatment until new rows carry per-call windows.
func LegacyGenerationWindowMs(durationMs, ttftMs, outputTokens uint64) uint64 {
	if durationMs == 0 {
		return 0
	}
	if ttftMs == 0 || ttftMs >= durationMs {
		return durationMs
	}
	window := durationMs - ttftMs
	if impliedTokensPerSecond(outputTokens, window) > MaxPlausibleTokensPerSecond {
		return durationMs
	}
	return window
}

// impliedTokensPerSecond returns output/window in tokens per second, 0 when
// either input is absent (never triggers the bound).
func impliedTokensPerSecond(outputTokens, windowMs uint64) float64 {
	if outputTokens == 0 || windowMs == 0 {
		return 0
	}
	return float64(outputTokens) / (float64(windowMs) / 1000)
}
