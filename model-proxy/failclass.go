package main

import (
	"bytes"
	"math"
	"regexp"
	"strconv"
	"time"

	runtimestate "model-proxy/internal/runtime"
)

// failclass.go — upstream 429 classification. Conservative substring/regex
// matchers inspect a bounded body peek; target-specific context, model-denial,
// and unsupported-parameter classifiers live in internal/targetexec.

// rateLimitKind classifies a 429 by WHAT the upstream says is exhausted. The
// kind picks the default cooldown when the response carries no explicit reset
// hint (body text or Retry-After header always wins over the kind default).
type rateLimitKind = runtimestate.RateLimitKind

const (
	rlTransient = runtimestate.Transient // generic rate limit (req/s, tokens/min) — short cooldown
	rlQuota     = runtimestate.Quota     // account quota/balance exhausted — long cooldown
	rlDaily     = runtimestate.Daily     // daily quota — locked until local midnight
)

func rateLimitKindFromString(s string) rateLimitKind {
	return runtimestate.ParseRateLimitKind(s)
}

// dailyQuotaMarkers: the daily-quota class is checked FIRST — it is strictly
// more specific than generic quota exhaustion ("today's quota" also contains
// "quota") and its cooldown (next midnight) differs.
var dailyQuotaMarkers = [][]byte{
	[]byte("today's quota"),
	[]byte("todays quota"),
	[]byte("daily quota"),
	[]byte("daily limit"),
	[]byte("per-day"),
	[]byte("per day quota"),
	[]byte("每日限额"),
	[]byte("每日额度"),
	[]byte("日配额"),
	[]byte("今日额度"),
}

// quotaExhaustedMarkers: account-level quota / balance / plan exhaustion, as
// opposed to a transient request-rate limit. Subscription (5h/weekly/monthly
// window) and pay-as-you-go balance failures both land here — both deserve a
// cooldown much longer than the 60s transient default.
var quotaExhaustedMarkers = [][]byte{
	[]byte("insufficient_quota"),
	[]byte("insufficient quota"),
	[]byte("insufficient balance"),
	[]byte("quota_exceeded"),
	[]byte("quota exceeded"),
	[]byte("exceeded your current quota"),
	[]byte("quota exhausted"),
	[]byte("quota_exhausted"),
	[]byte("credit balance"),
	[]byte("balance not enough"),
	[]byte("account balance"),
	[]byte("余额不足"),
	[]byte("额度不足"),
	[]byte("配额不足"),
	[]byte("套餐额度"),
	[]byte("配额已用完"),
	[]byte("额度已用完"),
}

// classify429 inspects a peeked 429 body and reports the exhaustion class.
// Empty/unmatched bodies are transient (the historical behavior).
func classify429(bodyPeek []byte) rateLimitKind {
	if len(bodyPeek) == 0 {
		return rlTransient
	}
	lower := bytes.ToLower(bodyPeek)
	for _, m := range dailyQuotaMarkers {
		if bytes.Contains(lower, m) {
			return rlDaily
		}
	}
	for _, m := range quotaExhaustedMarkers {
		if bytes.Contains(lower, m) {
			return rlQuota
		}
	}
	return rlTransient
}

// maxResetHint is the cap on upstream-provided reset times. Aqp monthly windows
// can legitimately be weeks out, but freezing a provider for a month on one
// 429 is worse than re-probing after a bounded cooldown — the quota-aware
// scheduler already deprioritizes an exhausted account independently.
const maxResetHint = 7 * 24 * time.Hour

var (
	// "retry after 20 seconds" / "retry-after: 20s" style, word or unit-letter forms.
	retryAfterRe = regexp.MustCompile(`(?i)retry[ -]?after[^0-9]{0,20}(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
	// "reset after 2h5m" / "resets in 164h27m24s" — one to three duration tokens.
	resetInRe = regexp.MustCompile(`(?i)resets?\s*(?:after|in)[^0-9]{0,20}((?:\d+\s*[hms]\s*){1,3})`)
	// Same but with word units: "reset in 2 hours 5 minutes".
	resetInWordsRe = regexp.MustCompile(`(?i)resets?\s*(?:after|in)[^0-9]{0,20}(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
	// RFC3339 timestamps — only when keyword-adjacent ("reset_at": "..." /
	// "retry at ..."), so an unrelated event time in the body is never
	// mistaken for a reset horizon (P1-4b).
	rfc3339KeyedRe = regexp.MustCompile(`(?i)(?:reset|retry)[^\n]{0,40}?(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:?\d{2}))`)
	durTokenRe     = regexp.MustCompile(`(\d+)\s*([hms])`)
)

// parseResetHint extracts an explicit reset time from an error body. Providers
// increasingly tell you exactly when the window resets; honoring that beats any
// fixed backoff. Returns false when no hint is found. Hints in the past or
// beyond maxResetHint are clamped (and still reported — a clamped hint is more
// precise than a blind default).
func parseResetHint(bodyPeek []byte, now time.Time) (time.Time, bool) {
	if len(bodyPeek) == 0 {
		return time.Time{}, false
	}
	var until time.Time
	if m := retryAfterRe.FindSubmatch(bodyPeek); m != nil {
		if d, ok := hintDuration(m[1], m[2]); ok {
			until = now.Add(d)
		}
	}
	if until.IsZero() {
		if m := resetInRe.FindSubmatch(bodyPeek); m != nil {
			var total time.Duration
			for _, tok := range durTokenRe.FindAllSubmatch(m[1], -1) {
				if d, ok := hintDuration(tok[1], tok[2]); ok {
					total += d
				}
			}
			if total > 0 {
				until = now.Add(total)
			}
		}
	}
	if until.IsZero() {
		if m := resetInWordsRe.FindSubmatch(bodyPeek); m != nil {
			if d, ok := hintDuration(m[1], m[2]); ok {
				until = now.Add(d)
			}
		}
	}
	if until.IsZero() {
		if m := rfc3339KeyedRe.FindSubmatch(bodyPeek); m != nil {
			if t, err := time.Parse(time.RFC3339, string(m[1])); err == nil {
				until = t
			}
		}
	}
	if until.IsZero() {
		return time.Time{}, false
	}
	if until.Before(now) {
		return now, true
	}
	if max := now.Add(maxResetHint); until.After(max) {
		return max, true
	}
	return until, true
}

// hintDuration converts a (number, unit) pair from the hint regexes into a
// Duration. Unit letters are ambiguous ("m" = minutes here — hours/seconds are
// what providers actually send; a "month" unit never appears in these hints).
// Huge values are clamped to maxResetHint BEFORE the multiply: time.Duration
// is int64 nanoseconds and would otherwise wrap negative → "retry immediately".
func hintDuration(numB, unitB []byte) (time.Duration, bool) {
	n, err := strconv.Atoi(string(numB))
	if err != nil || n < 0 {
		return 0, false
	}
	u := string(bytes.ToLower(unitB))
	var unit time.Duration
	switch {
	case u == "d" || u == "day" || u == "days":
		unit = 24 * time.Hour
	case u == "h" || u == "hr" || u == "hrs" || u == "hour" || u == "hours":
		unit = time.Hour
	case u == "m" || u == "min" || u == "mins" || u == "minute" || u == "minutes":
		unit = time.Minute
	default: // s / sec / secs / second / seconds
		unit = time.Second
	}
	if n > int(math.MaxInt64/int64(unit)) {
		return maxResetHint, true
	}
	return time.Duration(n) * unit, true
}
