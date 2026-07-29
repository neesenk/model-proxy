package main

import (
	"testing"
	"time"

	"model-proxy/internal/targetexec"
)

func TestClassify429(t *testing.T) {
	cases := []struct {
		body string
		want rateLimitKind
	}{
		{`{"error":{"code":"insufficient_quota","message":"You exceeded your current quota"}}`, rlQuota},
		{`{"error":"quota exceeded for this plan"}`, rlQuota},
		{`{"code":429,"msg":"余额不足，请充值"}`, rlQuota},
		{`{"msg":"套餐额度已用完"}`, rlQuota},
		{`{"error":"credit balance too low"}`, rlQuota},
		{`{"error":"You have used up today's quota, resets at midnight"}`, rlDaily},
		{`{"msg":"daily quota exhausted"}`, rlDaily},
		{`{"msg":"每日限额已达上限"}`, rlDaily},
		{`{"error":"rate limit reached, retry after 20s"}`, rlTransient},
		{`{"error":"too many requests"}`, rlTransient},
		// qwen-plan (Token Plan 个人版): "Allocated quota exceeded" = 5h/7d window
		// exhausted (rlQuota — matches "quota exceeded"); "Requests rate limit
		// exceeded" = concurrency rate-cap (rlTransient). No failclass code change.
		{`{"code":"ArrearQuotaExceeded","message":"Allocated quota exceeded"}`, rlQuota},
		{`{"message":"Requests rate limit exceeded"}`, rlTransient},
		// Mixed case: matching is case-insensitive (body is lowercased first).
		{`{"error":{"code":"Insufficient_Quota","message":"You Exceeded Your Current Quota"}}`, rlQuota},
		{`{"error":"Daily Quota Exhausted"}`, rlDaily},
		{`{"error":"Too Many Requests"}`, rlTransient},
		{``, rlTransient},
	}
	for _, c := range cases {
		if got := classify429([]byte(c.body)); got != c.want {
			t.Errorf("classify429(%q) = %v want %v", c.body, got, c.want)
		}
	}
}

func TestParseResetHint(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		body string
		want time.Duration // expected until - now
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
	for _, c := range cases {
		got, ok := parseResetHint([]byte(c.body), now)
		if ok != c.ok {
			t.Errorf("parseResetHint(%q) ok = %v want %v", c.body, ok, c.ok)
			continue
		}
		if ok {
			d := got.Sub(now)
			if d < c.want-time.Second || d > c.want+time.Second {
				t.Errorf("parseResetHint(%q) = %v want ~%v", c.body, d, c.want)
			}
		}
	}
}

func TestParseResetHint_Clamps(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	// Past timestamp → clamped to now.
	if got, ok := parseResetHint([]byte(`{"reset_at":"2026-07-20T00:00:00Z"}`), now); !ok || !got.Equal(now) {
		t.Errorf("past hint = %v,%v want now,true", got, ok)
	}
	// Beyond the 7-day cap → clamped to the cap.
	got, ok := parseResetHint([]byte(`{"reset_at":"2026-08-20T00:00:00Z"}`), now)
	if !ok || got.Sub(now) != maxResetHint {
		t.Errorf("far-future hint = %v (%v) want capped at %v", got, ok, maxResetHint)
	}
}

func TestIsModelDenied(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{400, `{"error":{"message":"The model 'gpt-x' does not exist"}}`, true},
		{404, `{"error":"model not found"}`, false}, // 404 is unconditional at the caller, not here
		{403, `{"error":"You do not have access to model glm-x"}`, true},
		{400, `{"error":"model is not available in your region"}`, true},
		{400, `{"msg":"模型不存在"}`, true},
		{400, `{"error":"The Model 'gpt-x' Does Not Exist"}`, true}, // mixed case: matching is case-insensitive
		{400, `{"error":"invalid api key"}`, false},
		{400, `{"error":"max_tokens is too large"}`, false},
		{400, ``, false},
		{500, `{"error":"model not found"}`, false}, // only 400/403 classify
	}
	for _, c := range cases {
		if got := targetexec.IsModelDenied(c.status, []byte(c.body)); got != c.want {
			t.Errorf("isModelDenied(%d, %q) = %v want %v", c.status, c.body, got, c.want)
		}
	}
}

func TestParseUnsupportedParam(t *testing.T) {
	cases := []struct {
		body string
		want string
		ok   bool
	}{
		{`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model."}}`, "max_tokens", true},
		{`{"error":{"type":"invalid_request_error","param":"store","code":"unsupported_parameter"}}`, "store", true},
		{`{"error":"Unknown parameter: 'session_id'"}`, "session_id", true},
		{`{"error":"Unrecognized field: "foo""}`, "foo", true},
		{`{"error":"parameter 'topp' is not supported"}`, "topp", true},
		// never-strip protection
		{`{"error":{"message":"Unsupported parameter: 'model'"}}`, "", false},
		{`{"error":{"message":"Unsupported parameter: 'messages'"}}`, "", false},
		// unrelated 400s must not match
		{`{"error":"invalid api key"}`, "", false},
		{`{"error":"context length exceeded"}`, "", false},
		{`{"param":"store"}`, "", false}, // bare param without "unsupported"
		{``, "", false},
	}
	for _, c := range cases {
		got, ok := targetexec.ParseUnsupportedParam([]byte(c.body))
		if got != c.want || ok != c.ok {
			t.Errorf("parseUnsupportedParam(%q) = %q,%v want %q,%v", c.body, got, ok, c.want, c.ok)
		}
	}
}

// TestHintDuration_Overflow: a pathological hint ("99999999999 days") must
// clamp to maxResetHint instead of wrapping int64 into a negative duration
// (which would read as "retry immediately").
func TestHintDuration_Overflow(t *testing.T) {
	if d, ok := hintDuration([]byte("99999999999"), []byte("days")); !ok || d != maxResetHint {
		t.Errorf("overflow days = %v,%v want %v,true", d, ok, maxResetHint)
	}
	if d, ok := hintDuration([]byte("99999999999999999"), []byte("h")); !ok || d != maxResetHint {
		t.Errorf("overflow hours = %v,%v want %v,true", d, ok, maxResetHint)
	}
	if d, ok := hintDuration([]byte("5"), []byte("h")); !ok || d != 5*time.Hour {
		t.Errorf("normal = %v,%v want 5h,true", d, ok)
	}
}

// TestParseResetHint_RFC3339Keyed: an RFC3339 timestamp only counts when it
// sits next to a reset/retry keyword — a bare event time is not a hint.
func TestParseResetHint_RFC3339Keyed(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if _, ok := parseResetHint([]byte(`{"event":"logged at 2026-07-21T13:30:00Z"}`), now); ok {
		t.Error("bare timestamp without reset/retry keyword must not be a hint")
	}
	if got, ok := parseResetHint([]byte(`{"reset_at":"2026-07-21T13:30:00Z"}`), now); !ok || got.Sub(now) != 90*time.Minute {
		t.Errorf("keyed timestamp = %v,%v want +90m,true", got, ok)
	}
}
