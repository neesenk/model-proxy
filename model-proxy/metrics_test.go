package main

import (
	"testing"
	"time"
)

func TestMetricsCounters(t *testing.T) {
	m := newMetricsStore()
	m.inc("zhipu", "requests")
	m.inc("zhipu", "requests")
	m.inc("zhipu", "failures")
	m.inc("deepseek", "rate_limited_429")
	m.inc("zhipu", "failovers")

	snap := m.snapshot()
	if snap["zhipu"].Requests != 2 {
		t.Errorf("zhipu requests = %d, want 2", snap["zhipu"].Requests)
	}
	if snap["zhipu"].Failures != 1 {
		t.Errorf("zhipu failures = %d, want 1", snap["zhipu"].Failures)
	}
	if snap["zhipu"].Failovers != 1 {
		t.Errorf("zhipu failovers = %d, want 1", snap["zhipu"].Failovers)
	}
	if snap["deepseek"].RateLimited429 != 1 {
		t.Errorf("deepseek 429 = %d, want 1", snap["deepseek"].RateLimited429)
	}
	if snap["missing"].Requests != 0 { // unseen provider → zero value
		t.Errorf("missing provider should be zero-valued")
	}
}

func TestMetricsStartedAt(t *testing.T) {
	before := time.Now()
	m := newMetricsStore()
	sa := m.startedAt()
	if sa.Before(before) || sa.After(time.Now().Add(time.Second)) {
		t.Errorf("startedAt %v not ~now", sa)
	}
}
