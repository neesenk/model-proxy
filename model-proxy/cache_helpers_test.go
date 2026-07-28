package main

import (
	"bytes"
	"io"
	"testing"
	"time"
)

// TestPinEntryExpiresLabel: no-expiry, future, and past states render distinctly.
func TestPinEntryExpiresLabel(t *testing.T) {
	now := time.Now()
	if l := (pinEntry{provider: "z"}).expiresLabel(now); l != "" {
		t.Errorf("no-expiry label=%q want empty", l)
	}
	if l := (pinEntry{provider: "z", expiresAt: now.Add(time.Hour)}).expiresLabel(now); l == "" || l == "expired" {
		t.Errorf("future label=%q want 'expires in ...'", l)
	}
	if l := (pinEntry{provider: "z", expiresAt: now.Add(-time.Hour)}).expiresLabel(now); l != "expired" {
		t.Errorf("past label=%q want expired", l)
	}
}

// TestCacheRecorder_Truncation: a body exceeding the cap is flagged truncated
// and the buffer is capped (so the entry is NOT cached on replay).
func TestCacheRecorder_Truncation(t *testing.T) {
	big := make([]byte, 100)
	for i := range big {
		big[i] = 'x'
	}
	rec := newCacheRecorder(io.NopCloser(bytes.NewReader(big)), 40)
	buf := make([]byte, 8)
	for {
		n, err := rec.Read(buf)
		if err != nil {
			break
		}
		_ = n
	}
	if !rec.truncated {
		t.Error("recorder should be truncated for an over-cap body")
	}
	if len(rec.buf) > 40 {
		t.Errorf("buf len=%d want <= cap 40", len(rec.buf))
	}
}

// TestConvertRequestResponse_NoOp: same-protocol is a pass-through (no conversion).
func TestCacheReset_Direct(t *testing.T) {
	c := newResponseCache(CacheConfig{Enabled: true})
	c.put("k", &cacheEntry{status: 200, body: []byte("x")}, time.Now())
	c.get("k", time.Now()) // hit
	h, m, e := c.stats()
	if h == 0 && m == 0 && e == 0 {
		t.Error("expected non-zero stats before reset")
	}
	c.reset()
	hits, misses, entries := c.stats()
	if hits != 0 || misses != 0 || entries != 0 {
		t.Errorf("after reset: hits=%d misses=%d entries=%d want 0/0/0", hits, misses, entries)
	}
}
