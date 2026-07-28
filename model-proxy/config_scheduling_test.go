package main

import (
	"testing"
	"time"
)

// --- Scheduling default + invalid-duration fallbacks ---

func TestScheduling_DefaultsAndInvalid(t *testing.T) {
	var s Scheduling // all zero
	if s.threshold() != 3 {
		t.Errorf("default threshold=%d want 3", s.threshold())
	}
	if s.cooldown() != 10*time.Minute {
		t.Errorf("default cooldown=%v want 10m", s.cooldown())
	}
	if s.rateBackoff() != 60*time.Second {
		t.Errorf("default rateBackoff=%v want 60s", s.rateBackoff())
	}
	if s.timeout() != 1800*time.Second {
		t.Errorf("default timeout=%v want 1800s", s.timeout())
	}
	if s.dwell() != 10*time.Minute {
		t.Errorf("default dwell=%v want 10m", s.dwell())
	}
	if s.pollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v want 5m", s.pollInterval())
	}
	if s.switchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v want 0.15", s.switchMargin())
	}

	// Invalid duration strings fall back to defaults.
	bad := Scheduling{CircuitCooldown: "bad", RateLimitBackoff: "bad", UpstreamTimeout: "bad", StickyDwell: "bad", QuotaPollInterval: "bad"}
	if bad.cooldown() != 10*time.Minute {
		t.Errorf("invalid cooldown fallback=%v want 10m", bad.cooldown())
	}
	if bad.rateBackoff() != 60*time.Second {
		t.Errorf("invalid rateBackoff fallback=%v want 60s", bad.rateBackoff())
	}
	if bad.timeout() != 1800*time.Second {
		t.Errorf("invalid timeout fallback=%v want 1800s", bad.timeout())
	}
	if bad.dwell() != 10*time.Minute {
		t.Errorf("invalid dwell fallback=%v want 10m", bad.dwell())
	}
	if bad.pollInterval() != 5*time.Minute {
		t.Errorf("invalid pollInterval fallback=%v want 5m", bad.pollInterval())
	}

	// Explicit values are honored.
	good := Scheduling{CircuitThreshold: 5, CircuitCooldown: "2m", RateLimitBackoff: "10s", UpstreamTimeout: "5s", StickyDwell: "3m", QuotaPollInterval: "1m", QuotaSwitchMargin: 25}
	if good.threshold() != 5 {
		t.Errorf("explicit threshold=%d want 5", good.threshold())
	}
	if good.cooldown() != 2*time.Minute {
		t.Errorf("explicit cooldown=%v want 2m", good.cooldown())
	}
	if good.switchMargin() != 0.25 {
		t.Errorf("explicit switchMargin=%v want 0.25", good.switchMargin())
	}
}
