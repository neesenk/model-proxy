package targetexec

import (
	"bytes"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"time"

	configdomain "model-proxy/internal/config"
)

const (
	RateLimitTransient RateLimitKind = "transient"
	RateLimitQuota     RateLimitKind = "quota"
	RateLimitDaily     RateLimitKind = "daily"
	maxResetHint                     = 7 * 24 * time.Hour
)

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

var (
	retryAfterRe    = regexp.MustCompile(`(?i)retry[ -]?after[^0-9]{0,20}(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
	resetInRe       = regexp.MustCompile(`(?i)resets?\s*(?:after|in)[^0-9]{0,20}((?:\d+\s*[hms]\s*){1,3})`)
	resetInWordsRe  = regexp.MustCompile(`(?i)resets?\s*(?:after|in)[^0-9]{0,20}(\d+)\s*(days?|d|hours?|hrs?|h|minutes?|mins?|m|seconds?|secs?|s)`)
	rfc3339KeyedRe  = regexp.MustCompile(`(?i)(?:reset|retry)[^\n]{0,40}?(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:Z|[+-]\d{2}:?\d{2}))`)
	durationTokenRe = regexp.MustCompile(`(\d+)\s*([hms])`)
)

// RateLimitKind classifies which upstream budget a 429 says is exhausted.
type RateLimitKind string

// RateLimitDecision is the pure 429 policy result, calculated before any
// runtime mutation so every caller (normal routing and Fusion) uses one rule.
type RateLimitDecision struct {
	Until time.Time
	Kind  RateLimitKind
}

// ParseRateLimit applies daily-before-quota classification. An explicit body
// reset hint wins over Retry-After, which wins over the scheduling default.
func ParseRateLimit(response *http.Response, bodyPeek []byte, now time.Time, scheduling configdomain.Scheduling) RateLimitDecision {
	kind := classify429(bodyPeek)
	if until, ok := parseResetHint(bodyPeek, now); ok {
		return RateLimitDecision{Until: until, Kind: kind}
	}
	if response != nil {
		if retryAfter := response.Header.Get("Retry-After"); retryAfter != "" {
			if seconds, err := strconv.Atoi(retryAfter); err == nil {
				if seconds < 0 {
					seconds = 0
				}
				return RateLimitDecision{Until: now.Add(time.Duration(seconds) * time.Second), Kind: kind}
			}
			if until, err := http.ParseTime(retryAfter); err == nil {
				if until.Before(now) {
					until = now
				}
				return RateLimitDecision{Until: until, Kind: kind}
			}
		}
	}
	switch kind {
	case RateLimitDaily:
		return RateLimitDecision{Until: time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location()), Kind: kind}
	case RateLimitQuota:
		return RateLimitDecision{Until: now.Add(scheduling.QuotaCooldownDuration()), Kind: kind}
	default:
		return RateLimitDecision{Until: now.Add(scheduling.RateBackoff()), Kind: kind}
	}
}

func classify429(body []byte) RateLimitKind {
	lower := bytes.ToLower(body)
	for _, marker := range dailyQuotaMarkers {
		if bytes.Contains(lower, marker) {
			return RateLimitDaily
		}
	}
	for _, marker := range quotaExhaustedMarkers {
		if bytes.Contains(lower, marker) {
			return RateLimitQuota
		}
	}
	return RateLimitTransient
}

func parseResetHint(body []byte, now time.Time) (time.Time, bool) {
	if len(body) == 0 {
		return time.Time{}, false
	}
	var until time.Time
	if m := retryAfterRe.FindSubmatch(body); m != nil {
		if d, ok := hintDuration(m[1], m[2]); ok {
			until = now.Add(d)
		}
	}
	if until.IsZero() {
		if m := resetInRe.FindSubmatch(body); m != nil {
			var total time.Duration
			for _, tok := range durationTokenRe.FindAllSubmatch(m[1], -1) {
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
		if m := resetInWordsRe.FindSubmatch(body); m != nil {
			if d, ok := hintDuration(m[1], m[2]); ok {
				until = now.Add(d)
			}
		}
	}
	if until.IsZero() {
		if m := rfc3339KeyedRe.FindSubmatch(body); m != nil {
			if parsed, err := time.Parse(time.RFC3339, string(m[1])); err == nil {
				until = parsed
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

func hintDuration(number, unit []byte) (time.Duration, bool) {
	n, err := strconv.Atoi(string(number))
	if err != nil || n < 0 {
		return 0, false
	}
	var scale time.Duration
	switch string(bytes.ToLower(unit)) {
	case "d", "day", "days":
		scale = 24 * time.Hour
	case "h", "hr", "hrs", "hour", "hours":
		scale = time.Hour
	case "m", "min", "mins", "minute", "minutes":
		scale = time.Minute
	default:
		scale = time.Second
	}
	if n > int(math.MaxInt64/int64(scale)) {
		return maxResetHint, true
	}
	return time.Duration(n) * scale, true
}
