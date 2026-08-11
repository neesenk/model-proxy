package targetexec

import (
	"net/http"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

func TestClassify429(t *testing.T) {
	cases := []struct {
		body string
		want RateLimitKind
	}{
		{`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, RateLimitQuota},
		{`{"error":"quota exceeded for this plan"}`, RateLimitQuota},
		{`{"code":429,"msg":"余额不足，请充值"}`, RateLimitQuota},
		{`{"msg":"套餐额度已用完"}`, RateLimitQuota},
		{`{"error":"credit balance too low"}`, RateLimitQuota},
		{`{"error":"You have used up today's quota, resets at midnight"}`, RateLimitDaily},
		{`{"msg":"daily quota exhausted"}`, RateLimitDaily},
		{`{"msg":"每日限额已达上限"}`, RateLimitDaily},
		{`{"error":"rate limit reached, retry after 20s"}`, RateLimitTransient},
		{`{"error":"too many requests"}`, RateLimitTransient},
		{`{"code":"ArrearQuotaExceeded","message":"Allocated quota exceeded"}`, RateLimitQuota},
		{`{"message":"Requests rate limit exceeded"}`, RateLimitTransient},
		{`{"error":{"code":"Insufficient_Quota","message":"You Exceeded Your Current Quota"}}`, RateLimitQuota},
		{`{"error":"Daily Quota Exhausted"}`, RateLimitDaily},
		{`{"error":"Too Many Requests"}`, RateLimitTransient},
		{``, RateLimitTransient},
	}
	for _, test := range cases {
		if got := classify429([]byte(test.body)); got != test.want {
			t.Errorf("classify429(%q) = %v, want %v", test.body, got, test.want)
		}
	}
}

func TestParseResetHint(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		body string
		want time.Duration
		ok   bool
	}{
		{`{"error":"rate limit, retry after 20 seconds"}`, 20 * time.Second, true},
		{`{"error":"retry after 20s"}`, 20 * time.Second, true},
		{`{"error":"retry after 5 minutes"}`, 5 * time.Minute, true},
		{`{"error":"quota reset after 2h5m"}`, 2*time.Hour + 5*time.Minute, true},
		{`{"error":"Resets in 164h27m24s"}`, 164*time.Hour + 27*time.Minute + 24*time.Second, true},
		{`{"error":"reset in 2 hours"}`, 2 * time.Hour, true},
		{`{"error":"reset_at":"2026-07-21T13:30:00Z"}`, 90 * time.Minute, true},
		{`{"error":"too many requests"}`, 0, false},
		{``, 0, false},
	}
	for _, test := range cases {
		got, ok := parseResetHint([]byte(test.body), now)
		if ok != test.ok {
			t.Errorf("parseResetHint(%q) ok = %v, want %v", test.body, ok, test.ok)
			continue
		}
		if ok {
			delta := got.Sub(now)
			if delta < test.want-time.Second || delta > test.want+time.Second {
				t.Errorf("parseResetHint(%q) = %v, want ~%v", test.body, delta, test.want)
			}
		}
	}
}

func TestParseResetHint_Clamps(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if got, ok := parseResetHint([]byte(`{"reset_at":"2026-07-20T00:00:00Z"}`), now); !ok || !got.Equal(now) {
		t.Errorf("past hint = %v,%v, want now,true", got, ok)
	}
	got, ok := parseResetHint([]byte(`{"reset_at":"2026-08-20T00:00:00Z"}`), now)
	if !ok || got.Sub(now) != maxResetHint {
		t.Errorf("far-future hint = %v (%v), want capped at %v", got, ok, maxResetHint)
	}
}

func TestHintDuration_Overflow(t *testing.T) {
	if duration, ok := hintDuration([]byte("99999999999"), []byte("days")); !ok || duration != maxResetHint {
		t.Errorf("overflow days = %v,%v, want %v,true", duration, ok, maxResetHint)
	}
	if duration, ok := hintDuration([]byte("99999999999999999"), []byte("h")); !ok || duration != maxResetHint {
		t.Errorf("overflow hours = %v,%v, want %v,true", duration, ok, maxResetHint)
	}
	if duration, ok := hintDuration([]byte("5"), []byte("h")); !ok || duration != 5*time.Hour {
		t.Errorf("normal = %v,%v, want 5h,true", duration, ok)
	}
}

func TestParseResetHint_RFC3339Keyed(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if _, ok := parseResetHint([]byte(`{"event":"logged at 2026-07-21T13:30:00Z"}`), now); ok {
		t.Error("bare timestamp without reset/retry keyword must not be a hint")
	}
	if got, ok := parseResetHint([]byte(`{"reset_at":"2026-07-21T13:30:00Z"}`), now); !ok || got.Sub(now) != 90*time.Minute {
		t.Errorf("keyed timestamp = %v,%v, want +90m,true", got, ok)
	}
}

func TestParseRateLimit(t *testing.T) {
	location := time.FixedZone("test-local", 8*60*60)
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, location)
	scheduling := configdomain.Scheduling{RateLimitBackoff: "60s", QuotaCooldown: "2h"}

	response := &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	decision := ParseRateLimit(response, nil, now, scheduling)
	if decision.Until.Sub(now) != 120*time.Second || decision.Kind != RateLimitTransient {
		t.Errorf("seconds Retry-After = %+v, want transient +120s", decision)
	}

	response.Header.Set("Retry-After", "-5")
	decision = ParseRateLimit(response, nil, now, scheduling)
	if !decision.Until.Equal(now) {
		t.Errorf("negative Retry-After = %+v, want now", decision)
	}

	future := now.Add(2 * time.Hour)
	response.Header.Set("Retry-After", future.UTC().Format(http.TimeFormat))
	decision = ParseRateLimit(response, nil, now, scheduling)
	if decision.Until.Unix() != future.Unix() {
		t.Errorf("HTTP-date Retry-After = %v, want %v", decision.Until, future)
	}

	past := now.Add(-time.Hour)
	response.Header.Set("Retry-After", past.UTC().Format(http.TimeFormat))
	decision = ParseRateLimit(response, nil, now, scheduling)
	if !decision.Until.Equal(now) {
		t.Errorf("past HTTP-date = %v, want now", decision.Until)
	}

	response.Header.Del("Retry-After")
	decision = ParseRateLimit(response, nil, now, scheduling)
	if decision.Until.Sub(now) != time.Minute {
		t.Errorf("missing Retry-After = %+v, want transient +60s", decision)
	}

	response.Header.Set("Retry-After", "not-a-number-or-date")
	decision = ParseRateLimit(response, nil, now, scheduling)
	if decision.Until.Sub(now) != time.Minute {
		t.Errorf("garbage Retry-After = %+v, want transient +60s", decision)
	}

	response.Header.Set("Retry-After", "120")
	decision = ParseRateLimit(response, []byte(`{"error":"quota exceeded, reset after 2h5m"}`), now, scheduling)
	if decision.Until.Sub(now) != 2*time.Hour+5*time.Minute || decision.Kind != RateLimitQuota {
		t.Errorf("body hint should win over header: %+v", decision)
	}

	response.Header.Del("Retry-After")
	decision = ParseRateLimit(response, []byte(`{"error":"insufficient_quota"}`), now, scheduling)
	if decision.Until.Sub(now) != 2*time.Hour || decision.Kind != RateLimitQuota {
		t.Errorf("quota default = %+v, want quota +2h", decision)
	}

	decision = ParseRateLimit(response, []byte(`{"error":"today's quota exhausted"}`), now, scheduling)
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, location)
	if !decision.Until.Equal(midnight) || decision.Kind != RateLimitDaily {
		t.Errorf("daily default = %+v, want daily until %v", decision, midnight)
	}

	decision = ParseRateLimit(nil, nil, now, scheduling)
	if decision.Until.Sub(now) != time.Minute || decision.Kind != RateLimitTransient {
		t.Errorf("nil response = %+v, want transient default", decision)
	}
}

func TestParseRateLimitPrecedenceAndBoundaries(t *testing.T) {
	location := time.FixedZone("provider-local", -7*60*60)
	now := time.Date(2026, 7, 21, 23, 59, 59, 0, location)
	scheduling := configdomain.Scheduling{RateLimitBackoff: "60s", QuotaCooldown: "2h"}

	// The daily marker must win even when the body also contains generic quota.
	daily := ParseRateLimit(nil, []byte(`daily quota exhausted: quota exceeded`), now, scheduling)
	wantMidnight := time.Date(2026, 7, 22, 0, 0, 0, 0, location)
	if daily.Kind != RateLimitDaily || !daily.Until.Equal(wantMidnight) {
		t.Fatalf("daily precedence = %+v, want daily until %v", daily, wantMidnight)
	}

	// A body reset is authoritative even when an HTTP Retry-After disagrees.
	response := &http.Response{Header: http.Header{"Retry-After": []string{"1"}}}
	bodyHint := ParseRateLimit(response, []byte(`daily quota exhausted; reset after 3h`), now, scheduling)
	if bodyHint.Kind != RateLimitDaily || bodyHint.Until.Sub(now) != 3*time.Hour {
		t.Fatalf("body hint precedence = %+v", bodyHint)
	}

	// An HTTP Retry-After remains authoritative over a quota default.
	header := ParseRateLimit(&http.Response{Header: http.Header{"Retry-After": []string{"5"}}}, []byte(`insufficient_quota`), now, scheduling)
	if header.Kind != RateLimitQuota || header.Until.Sub(now) != 5*time.Second {
		t.Fatalf("header precedence = %+v", header)
	}

	// Body-duration overflow, past reset and the seven-day horizon are clamped.
	overflow := ParseRateLimit(nil, []byte(`retry after 999999999999999 days`), now, scheduling)
	if overflow.Until.Sub(now) != maxResetHint {
		t.Fatalf("overflow clamp = %+v", overflow)
	}
	past := ParseRateLimit(nil, []byte(`reset_at 2026-07-20T00:00:00-07:00`), now, scheduling)
	if !past.Until.Equal(now) {
		t.Fatalf("past clamp = %+v, want %v", past, now)
	}
	future := ParseRateLimit(nil, []byte(`reset_at 2026-08-20T00:00:00-07:00`), now, scheduling)
	if future.Until.Sub(now) != maxResetHint {
		t.Fatalf("future clamp = %+v", future)
	}
}
