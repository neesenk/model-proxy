package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMetricsAddLatency: addLatency accumulates into the sums the flusher diffs,
// snapshot returns them, and seed round-trips them (the boot-restore path).
func TestMetricsAddLatency(t *testing.T) {
	m := newMetricsStore()
	m.inc("z", "glm", evRequests)
	m.addLatency("z", "glm", 100, 20)
	m.addLatency("z", "glm", 50, 10)
	snap := m.snapshot()[pmKey{Provider: "z", Model: "glm"}]
	if snap.Requests != 1 {
		t.Errorf("requests=%d want 1", snap.Requests)
	}
	if snap.LatencySum != 150 || snap.TTFTSum != 30 {
		t.Errorf("latency=%d ttft=%d want 150/30", snap.LatencySum, snap.TTFTSum)
	}
	// aggregateByProvider rolls the sums up across models.
	agg := m.aggregateByProvider()["z"]
	if agg.LatencySum != 150 || agg.TTFTSum != 30 {
		t.Errorf("aggregate latency=%d ttft=%d want 150/30", agg.LatencySum, agg.TTFTSum)
	}
	// seed round-trips the sums (boot restore).
	m2 := newMetricsStore()
	m2.seed(pmKey{Provider: "z", Model: "glm"}, snap)
	got := m2.snapshot()[pmKey{Provider: "z", Model: "glm"}]
	if got.LatencySum != 150 || got.TTFTSum != 30 {
		t.Errorf("seed round-trip latency=%d ttft=%d want 150/30", got.LatencySum, got.TTFTSum)
	}
}

// TestTimingResponseWriter: the first Write stamps firstByte; a writer that never
// receives bytes leaves hasFirstByte false; Flush delegates so SSE still flushes.
func TestTimingResponseWriter(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTimingResponseWriter(rec)
	if tw.hasFirstByte {
		t.Fatal("firstByte should be unset before any Write")
	}
	time.Sleep(2 * time.Millisecond)
	tw.Write([]byte("hello"))
	if !tw.hasFirstByte {
		t.Error("firstByte not stamped on first Write")
	}
	if tw.firstByte.IsZero() {
		t.Error("firstByte is zero after Write")
	}
	// Subsequent writes do not move firstByte.
	fb := tw.firstByte
	time.Sleep(time.Millisecond)
	tw.Write([]byte("world"))
	if tw.firstByte != fb {
		t.Error("firstByte moved on a later Write")
	}
	// Flusher delegation: the recorder implements http.Flusher (no-op), so this
	// must not panic and the underlying writer must still be usable.
	tw.Flush()
	if rec.Body.String() != "helloworld" {
		t.Errorf("body=%q want helloworld (Flush must not corrupt writes)", rec.Body.String())
	}
}

// TestForward_RecordsLatency: a served request records a non-zero total latency
// and TTFT against its (provider, model) in the metrics store on the hot path.
// The upstream deliberately delays before responding so latency is measurable.
func TestForward_RecordsLatency(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(8 * time.Millisecond) // ensure latency >= a few ms
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"z": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "z", Model: "glm-rt"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "z-key"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req = req.WithContext(context.Background())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	snap := p.metrics.snapshot()[pmKey{Provider: "z", Model: "glm-rt"}]
	if snap.Requests != 1 {
		t.Errorf("requests=%d want 1", snap.Requests)
	}
	if snap.LatencySum == 0 {
		t.Error("latency not recorded (LatencySum=0) for served request")
	}
	if snap.TTFTSum == 0 {
		t.Error("TTFT not recorded (TTFTSum=0); first-byte stamp should fire on body write")
	}
	// Ordering: the recorded latency is the UPSTREAM response time (send →
	// headers received, see proxy.go), while TTFT runs to the first byte
	// written to the CLIENT — necessarily after the headers arrive. Both share
	// the same `start`, so TTFT can never be SMALLER; the reverse (ttft >
	// latency) is normal whenever the body copy lands in a later millisecond
	// (ms truncation under load), not a bug.
	if snap.TTFTSum < snap.LatencySum {
		t.Errorf("ttft=%d < latency=%d (impossible: first client byte precedes upstream headers)", snap.TTFTSum, snap.LatencySum)
	}
}
