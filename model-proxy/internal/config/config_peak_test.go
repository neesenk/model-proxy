package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// --- PeakConfig unmarshal: list of strings shape ---

func TestPeakConfig_UnmarshalListOfStrings(t *testing.T) {
	var pc PeakConfig
	if err := yaml.Unmarshal([]byte(`["09:00-12:00", "14:00-18:00"]`), &pc); err != nil {
		t.Fatalf("unmarshal list of strings: %v", err)
	}
	if len(pc) != 2 {
		t.Fatalf("list-of-strings: len=%d want 2", len(pc))
	}
	if pc[0].Window != "09:00-12:00" {
		t.Errorf("pc[0].Window=%q", pc[0].Window)
	}
}

func TestPeakConfig_UnmarshalInvalid(t *testing.T) {
	// A sequence of non-string, non-map elements must fail closed instead of
	// being silently discarded or coerced into bogus time windows.
	var pc PeakConfig
	if err := yaml.Unmarshal([]byte(`[1, 2, 3]`), &pc); err == nil {
		t.Fatalf("integer peak_hours unexpectedly accepted: %+v", pc)
	}
	if len(pc) != 0 {
		t.Errorf("failed unmarshal left partial peak_hours: %+v", pc)
	}
}

// --- peakMultiplier: wrap-around window (e.g. 22:00-02:00) ---

func TestPeakMultiplier_WrapAround(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "22:00-02:00", Multiplier: 3}}}
	// 23:00 is inside the wrap-around window.
	late := timeAt(t, 23, 0)
	if got := p.PeakMultiplier(late); got != 3 {
		t.Errorf("wrap 23:00 multiplier=%v want 3", got)
	}
	// 01:00 is also inside (after midnight).
	early := timeAt(t, 1, 0)
	if got := p.PeakMultiplier(early); got != 3 {
		t.Errorf("wrap 01:00 multiplier=%v want 3", got)
	}
	// 12:00 is outside.
	noon := timeAt(t, 12, 0)
	if got := p.PeakMultiplier(noon); got != 1 {
		t.Errorf("noon multiplier=%v want 1 (no peak)", got)
	}
	// Wrap window edges: start inclusive, end exclusive.
	if got := p.PeakMultiplier(timeAt(t, 22, 0)); got != 3 {
		t.Errorf("wrap 22:00 (start, inclusive) multiplier=%v want 3", got)
	}
	if got := p.PeakMultiplier(timeAt(t, 2, 0)); got != 1 {
		t.Errorf("wrap 02:00 (end, exclusive) multiplier=%v want 1", got)
	}
}

// --- peakMultiplier: default multiplier when 0/unset on the segment ---

func TestPeakMultiplier_DefaultMultiplier(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "09:00-18:00"}}} // Multiplier 0 → default
	inside := timeAt(t, 12, 0)
	if got := p.PeakMultiplier(inside); got != DefaultPeakMultiplier {
		t.Errorf("default multiplier=%v want %v", got, DefaultPeakMultiplier)
	}
}

// --- peakMultiplier: malformed window is skipped (no panic, returns 1) ---

func TestPeakMultiplier_MalformedSkipped(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{{Window: "bad"}, {Window: "10:00-11:00", Multiplier: 2}}}
	if got := p.PeakMultiplier(timeAt(t, 10, 30)); got != 2 {
		t.Errorf("malformed-then-valid: multiplier=%v want 2 (malformed skipped)", got)
	}
}

func timeAt(t *testing.T, hour, min int) time.Time {
	t.Helper()
	now := time.Now()
	return time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
}
