package config

import (
	"testing"
	"time"
)

// --- Scheduling default + invalid-duration fallbacks ---

func TestScheduling_DefaultsAndInvalid(t *testing.T) {
	var s Scheduling // all zero
	if s.Threshold() != 3 {
		t.Errorf("default threshold=%d want 3", s.Threshold())
	}
	if s.Cooldown() != 10*time.Minute {
		t.Errorf("default cooldown=%v want 10m", s.Cooldown())
	}
	if s.RateBackoff() != 60*time.Second {
		t.Errorf("default rateBackoff=%v want 60s", s.RateBackoff())
	}
	if s.QuotaCooldownDuration() != time.Hour {
		t.Errorf("default quotaCooldown=%v want 1h", s.QuotaCooldownDuration())
	}
	if s.ModelLockoutDuration() != 10*time.Minute {
		t.Errorf("default modelLockout=%v want 10m", s.ModelLockoutDuration())
	}
	if s.RetryWaitDuration() != 10*time.Second {
		t.Errorf("default retryWait=%v want 10s", s.RetryWaitDuration())
	}
	if s.Timeout() != 1800*time.Second {
		t.Errorf("default timeout=%v want 1800s", s.Timeout())
	}
	if s.Dwell() != 10*time.Minute {
		t.Errorf("default dwell=%v want 10m", s.Dwell())
	}
	if s.PollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v want 5m", s.PollInterval())
	}
	if s.SwitchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v want 0.15", s.SwitchMargin())
	}

	// Invalid duration strings fall back to defaults.
	bad := Scheduling{
		CircuitCooldown:   "bad",
		RateLimitBackoff:  "bad",
		QuotaCooldown:     "bad",
		ModelLockout:      "bad",
		RetryWait:         "bad",
		UpstreamTimeout:   "bad",
		StickyDwell:       "bad",
		QuotaPollInterval: "bad",
	}
	if bad.Cooldown() != 10*time.Minute {
		t.Errorf("invalid cooldown fallback=%v want 10m", bad.Cooldown())
	}
	if bad.RateBackoff() != 60*time.Second {
		t.Errorf("invalid rateBackoff fallback=%v want 60s", bad.RateBackoff())
	}
	if bad.QuotaCooldownDuration() != time.Hour {
		t.Errorf("invalid quotaCooldown fallback=%v want 1h", bad.QuotaCooldownDuration())
	}
	if bad.ModelLockoutDuration() != 10*time.Minute {
		t.Errorf("invalid modelLockout fallback=%v want 10m", bad.ModelLockoutDuration())
	}
	if bad.RetryWaitDuration() != 10*time.Second {
		t.Errorf("invalid retryWait fallback=%v want 10s", bad.RetryWaitDuration())
	}
	if bad.Timeout() != 1800*time.Second {
		t.Errorf("invalid timeout fallback=%v want 1800s", bad.Timeout())
	}
	if bad.Dwell() != 10*time.Minute {
		t.Errorf("invalid dwell fallback=%v want 10m", bad.Dwell())
	}
	if bad.PollInterval() != 5*time.Minute {
		t.Errorf("invalid pollInterval fallback=%v want 5m", bad.PollInterval())
	}

	// Explicit values are honored.
	good := Scheduling{
		CircuitThreshold:  5,
		CircuitCooldown:   "2m",
		RateLimitBackoff:  "10s",
		QuotaCooldown:     "2h",
		ModelLockout:      "30m",
		RetryWait:         "0",
		UpstreamTimeout:   "5s",
		StickyDwell:       "3m",
		QuotaPollInterval: "1m",
		QuotaSwitchMargin: 25,
	}
	if good.Threshold() != 5 {
		t.Errorf("explicit threshold=%d want 5", good.Threshold())
	}
	if good.Cooldown() != 2*time.Minute {
		t.Errorf("explicit cooldown=%v want 2m", good.Cooldown())
	}
	if good.RateBackoff() != 10*time.Second {
		t.Errorf("explicit rateBackoff=%v want 10s", good.RateBackoff())
	}
	if good.QuotaCooldownDuration() != 2*time.Hour {
		t.Errorf("explicit quotaCooldown=%v want 2h", good.QuotaCooldownDuration())
	}
	if good.ModelLockoutDuration() != 30*time.Minute {
		t.Errorf("explicit modelLockout=%v want 30m", good.ModelLockoutDuration())
	}
	if good.RetryWaitDuration() != 0 {
		t.Errorf("explicit retryWait=%v want 0", good.RetryWaitDuration())
	}
	if good.Timeout() != 5*time.Second {
		t.Errorf("explicit timeout=%v want 5s", good.Timeout())
	}
	if good.Dwell() != 3*time.Minute {
		t.Errorf("explicit dwell=%v want 3m", good.Dwell())
	}
	if good.PollInterval() != time.Minute {
		t.Errorf("explicit pollInterval=%v want 1m", good.PollInterval())
	}
	if good.SwitchMargin() != 0.25 {
		t.Errorf("explicit switchMargin=%v want 0.25", good.SwitchMargin())
	}
}
