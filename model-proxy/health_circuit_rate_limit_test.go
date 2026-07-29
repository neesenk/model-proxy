package main

import (
	"net/http"
	"testing"
	"time"
)

// TestParseRateLimit keeps the application-layer parsing contract in the
// composition root. Health/circuit/rate-limit state transitions themselves
// belong to internal/runtime/manager_health_test.go.
func TestParseRateLimit(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	p := newTestProxy(t, cfg)
	sched := Scheduling{RateLimitBackoff: "60s", QuotaCooldown: "2h"}
	now := time.Now()

	// Seconds.
	r := &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	got, kind := p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d < 119*time.Second || d > 121*time.Second {
		t.Errorf("seconds Retry-After: got %v want ~120s", d)
	}
	if kind != rlTransient {
		t.Errorf("no body: kind = %v want transient", kind)
	}
	// Negative seconds clamped to 0.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"-5"}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Before(now) {
		t.Errorf("negative Retry-After: got %v before now", got)
	}
	// HTTP-date.
	future := now.Add(2 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{future.UTC().Format(http.TimeFormat)}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Unix() != future.Unix() {
		t.Errorf("HTTP-date Retry-After: got %v want %v", got, future)
	}
	// HTTP-date in the past → clamped to now.
	past := now.Add(-1 * time.Hour)
	r = &http.Response{Header: http.Header{"Retry-After": []string{past.UTC().Format(http.TimeFormat)}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if got.Before(now) {
		t.Errorf("past HTTP-date: got %v before now", got)
	}
	// Missing Retry-After → default backoff.
	r = &http.Response{Header: http.Header{}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("missing Retry-After: got %v want 60s", d)
	}
	// Garbage Retry-After → default backoff.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"not-a-number-or-date"}}}
	got, _ = p.parseRateLimit(r, nil, now, sched)
	if d := got.Sub(now); d != 60*time.Second {
		t.Errorf("garbage Retry-After: got %v want 60s", d)
	}

	// Body reset hint beats the Retry-After header.
	r = &http.Response{Header: http.Header{"Retry-After": []string{"120"}}}
	got, kind = p.parseRateLimit(r, []byte(`{"error":"quota exceeded, reset after 2h5m"}`), now, sched)
	if d := got.Sub(now); d < 2*time.Hour+4*time.Minute || d > 2*time.Hour+6*time.Minute {
		t.Errorf("body hint should win over header: got %v want ~2h5m", d)
	}
	if kind != rlQuota {
		t.Errorf("quota body: kind = %v want rlQuota", kind)
	}

	// Quota class without any hint → quota_cooldown (2h here).
	r = &http.Response{Header: http.Header{}}
	got, kind = p.parseRateLimit(r, []byte(`{"error":"insufficient_quota"}`), now, sched)
	if d := got.Sub(now); d != 2*time.Hour {
		t.Errorf("quota class default: got %v want 2h", d)
	}
	if kind != rlQuota {
		t.Errorf("kind = %v want rlQuota", kind)
	}

	// Daily class without any hint → next local midnight.
	got, kind = p.parseRateLimit(r, []byte(`{"error":"today's quota exhausted"}`), now, sched)
	if kind != rlDaily {
		t.Errorf("kind = %v want rlDaily", kind)
	}
	midnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location())
	if !got.Equal(midnight) {
		t.Errorf("daily class: got %v want local midnight %v", got, midnight)
	}
}
