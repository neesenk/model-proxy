package main

import (
	"bytes"
	"math"
	"regexp"
	"strconv"
	"time"

	runtimestate "model-proxy/internal/runtime"
)

// failclass.go — upstream failure classification (P0). Conservative substring /
// regex matchers over peeked error bodies, in the spirit of isContextOverflow:
// deliberately specific so ordinary client errors never match. Everything here
// is a pure function over a bounded body peek (≤64KiB, see peekResponseBody).

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

// modelDeniedMarkers: 400/403 bodies saying the MODEL (not the account, not the
// request shape) is the problem — removed, renamed, or not provisioned on this
// account. Such failures must lock only (provider, model), never the account's
// circuit breaker. Kept specific so auth/credential errors never match.
var modelDeniedMarkers = [][]byte{
	[]byte("model not found"),
	[]byte("model_not_found"),
	[]byte("model does not exist"),
	[]byte("does not exist"),
	[]byte("no such model"),
	[]byte("model is not available"),
	[]byte("model unavailable"),
	[]byte("do not have access to model"),
	[]byte("not have access to the model"),
	[]byte("no access to model"),
	[]byte("not entitled to access model"),
	[]byte("invalid model"),
	[]byte("unknown model"),
	[]byte("模型不存在"),
	[]byte("模型已下线"),
	[]byte("模型不可用"),
	[]byte("无权限访问模型"),
	[]byte("没有该模型的访问权限"),
}

// isModelDenied reports whether a 400/403 body says the model itself is
// unavailable. 404 is handled unconditionally by the caller (the proxy only
// forwards known LLM paths, so an upstream 404 means the model/path is gone).
func isModelDenied(status int, bodyPeek []byte) bool {
	if status != 400 && status != 403 {
		return false
	}
	if len(bodyPeek) == 0 {
		return false
	}
	lower := bytes.ToLower(bodyPeek)
	for _, m := range modelDeniedMarkers {
		if bytes.Contains(lower, m) {
			return true
		}
	}
	return false
}

var (
	// OpenAI style: "Unsupported parameter: 'max_tokens'" / `unsupported_parameter`.
	unsupportedParamRe = regexp.MustCompile(`(?i)unsupported[ _]parameter[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	unknownParamRe     = regexp.MustCompile(`(?i)unknown[ _](?:parameter|param|field|argument)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	unrecogParamRe     = regexp.MustCompile(`(?i)unrecognized[ _](?:parameter|param|field)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})`)
	notSupportedRe     = regexp.MustCompile(`(?i)(?:parameter|param|field)[^a-z0-9]{0,12}["'` + "`" + `]?([a-zA-Z0-9_.\-]{1,64})["'` + "`" + `]?[^a-z0-9]{0,20}(?:is\s+)?not\s+(?:supported|allowed|recognized)`)
	// Structured form: {"error": {"param": "store", "code": "unsupported_parameter"}}.
	jsonParamRe = regexp.MustCompile(`"param"\s*:\s*"([a-zA-Z0-9_.\-]{1,64})"`)
)

// neverStripParams can never be auto-stripped even if an upstream names them —
// removing them would break routing, hollow out the request, or silently change
// its semantics (streaming on/off, tool availability, response shape).
var neverStripParams = map[string]bool{
	"model": true, "messages": true, "input": true, "prompt": true, "system": true,
	"stream": true, "tools": true, "tool_choice": true, "response_format": true,
}

// parseUnsupportedParam extracts the offending top-level request parameter
// from a 400 body, for the auto-strip blocklist. Conservative: returns false
// unless a known phrasing matches AND the extracted name survives the
// never-strip list. Only top-level JSON keys are ever stripped.
func parseUnsupportedParam(bodyPeek []byte) (string, bool) {
	if len(bodyPeek) == 0 {
		return "", false
	}
	for _, re := range []*regexp.Regexp{unsupportedParamRe, unknownParamRe, unrecogParamRe, notSupportedRe} {
		if m := re.FindSubmatch(bodyPeek); m != nil {
			if p := string(m[1]); !neverStripParams[p] {
				return p, true
			}
		}
	}
	// Structured {"param": ...} only counts when the same body says the param is
	// unsupported — a bare "param" field appears in unrelated errors.
	if bytes.Contains(bytes.ToLower(bodyPeek), []byte("unsupported")) {
		if m := jsonParamRe.FindSubmatch(bodyPeek); m != nil {
			if p := string(m[1]); !neverStripParams[p] {
				return p, true
			}
		}
	}
	return "", false
}
