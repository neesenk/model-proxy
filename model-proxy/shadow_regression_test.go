package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestShadowReport_Pairing: two primary + two shadow records → one aggregated
// entry with correct metrics.
func TestShadowReport_Pairing(t *testing.T) {
	dir := t.TempDir()
	// Write records: primary "p1" (2xx, 100ms, 200 bytes) + shadow "shadow-p1"
	// (2xx, 150ms, 180 bytes), for the same route/provider pair.
	recs := []requestLogRecord{
		{Ts: "2026-07-19T01:00:00Z", RequestID: "p1", Exposed: "glm", Provider: "primary", Status: 200, LatencyMs: 100, ResponseSize: 200},
		{Ts: "2026-07-19T01:00:01Z", RequestID: "shadow-p1", Exposed: "glm", Provider: "shadowp", Status: 200, LatencyMs: 150, ResponseSize: 180, Shadow: true},
		{Ts: "2026-07-19T01:00:02Z", RequestID: "p2", Exposed: "glm", Provider: "primary", Status: 200, LatencyMs: 90, ResponseSize: 210},
		{Ts: "2026-07-19T01:00:03Z", RequestID: "shadow-p2", Exposed: "glm", Provider: "shadowp", Status: 500, LatencyMs: 200, ResponseSize: 50, Shadow: true},
	}
	var b strings.Builder
	for _, r := range recs {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	os.WriteFile(filepath.Join(dir, "requests-20260719-010000.log"), []byte(b.String()), 0o600)

	entries, err := shadowReport(dir, recordFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d want 1 (one route/primary/shadow triple)", len(entries))
	}
	e := entries[0]
	if e.Samples != 2 {
		t.Errorf("samples=%d want 2", e.Samples)
	}
	// Status match: p1(200) vs shadow(200) match; p2(200) vs shadow(500) mismatch. 1/2 = 0.5.
	if e.StatusMatchRate != 0.5 {
		t.Errorf("status_match=%v want 0.5", e.StatusMatchRate)
	}
	// Avg latency: primary (100+90)/2=95, shadow (150+200)/2=175, diff=80.
	if e.PrimaryLatencyMs != 95 || e.ShadowLatencyMs != 175 || e.LatencyDiffMs != 80 {
		t.Errorf("latency prim=%d shad=%d diff=%d want 95/175/80", e.PrimaryLatencyMs, e.ShadowLatencyMs, e.LatencyDiffMs)
	}
}

// TestShouldShadow: rate=0 → false, rate>=1 → true, rate between → probabilistic.
func TestShouldShadow(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "z", Model: "m"}}},
	}
	// rate >= 1 → always true.
	p := newTestProxy(t, cfg)
	p.shadow.Store(&shadowRuntime{sem: make(chan struct{}, 1), sampRate: 1.0})
	if !p.shouldShadow() {
		t.Error("rate=1.0 should return true")
	}
	// rate <= 0 → always false.
	p.shadow.Store(&shadowRuntime{sem: make(chan struct{}, 1), sampRate: 0})
	if p.shouldShadow() {
		t.Error("rate=0 should return false")
	}
	// nil runtime → false.
	p.shadow.Store(nil)
	if p.shouldShadow() {
		t.Error("nil shadow runtime should return false")
	}
}

// TestReload_ShadowDisabledStopsFiring (regression #6): the shadow sample rate /
// concurrency cap / client must update on reload. Disabling shadow via
// shadow_sample_rate: 0 + reload must stop firing shadow requests immediately —
// pre-fix the sample rate was cached at startup, so paid shadow requests kept
// firing until restart.
func TestReload_ShadowDisabledStopsFiring(t *testing.T) {
	var candHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mainUp.Close()
	candUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer candUp.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	reqDir := filepath.Join(t.TempDir(), "requests")
	base := fmt.Sprintf("listen: 127.0.0.1:0\n"+
		"providers:\n"+
		"  main:\n    openai_base_url: %s\n    provider_id: static\n"+
		"  cand:\n    openai_base_url: %s\n    provider_id: static\n"+
		"routes:\n  m:\n    - {provider: main, model: m}\n"+
		"shadow:\n  m:\n    provider: cand\n    model: m\n"+
		"request_log:\n  enabled: true\n  dir: %s\n", mainUp.URL, candUp.URL, reqDir)
	write := func(extra string) {
		if err := os.WriteFile(cfgPath, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("shadow_sample_rate: 1.0\n")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	p.initRequestLog(cfg.RequestLog) // shadow only fires when reqLog is active
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	send := func() {
		resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// 1) shadow enabled → the candidate IS hit (fire-and-forget, so poll).
	send()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && candHits.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if first := candHits.Load(); first != 1 {
		t.Fatalf("shadow should have fired once after first send; candHits=%d", first)
	}

	// 2) disable shadow via reload (sample_rate: 0).
	write("shadow_sample_rate: 0.0\n")
	if err := p.reload(cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	send()
	// Admission to the shadow semaphore happens synchronously before the
	// fire-and-forget goroutine starts. An empty gate after send therefore proves
	// this request was not admitted, without a timing-based absence assertion.
	if inFlight := len(p.shadow.Load().sem); inFlight != 0 {
		t.Fatalf("after disabling shadow via reload, in-flight shadow admissions=%d, want 0", inFlight)
	}
	if got := candHits.Load(); got != 1 {
		t.Errorf("after disabling shadow via reload, candHits=%d, want 1 (shadow kept firing — sample rate not reload-aware)", got)
	}
}

// TestShadow_PooledProvider (regression for the unified resolver, #10): a shadow
// target that names a POOLED parent must still be sampled. Pre-fix runShadow did
// provs[shadow.Provider] (nil for a parent) → "provider not available" → shadow
// silently stopped the moment a second account was added. After the fix the
// resolver picks one of the parent's virtuals.
func TestShadow_PooledProvider(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu-shadow", "zhipu", "SA", "SB")

	var shadowHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mainUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"main":         {OpenAIBaseURL: mainUp.URL, Provider: "static"},
			"zhipu-shadow": {OpenAIBaseURL: shadowUp.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{"m": {{Provider: "main", Model: "m"}}},
		Shadow: map[string]ShadowTarget{"m": {Provider: "zhipu-shadow", Model: "glm"}},
	}
	p := newTestProxy(t, cfg)
	p.initRequestLog(RequestLogConfig{Enabled: true, Dir: filepath.Join(t.TempDir(), "requests")})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Shadow is fire-and-forget; poll for the hit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && shadowHits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if shadowHits.Load() == 0 {
		t.Fatal("pooled shadow target (zhipu-shadow) never sampled — resolver did not resolve it to a virtual")
	}
}

// TestShadow_ConvertFail_Closed (regression #C): when a cross-protocol shadow
// request's conversion fails, the shadow must be SKIPPED — not sent with the
// unconverted body (which would ship an Anthropic body to an OpenAI endpoint or
// vice versa). Pre-fix runShadow logged the error and forwarded the raw body.
func TestShadow_ConvertFail_Closed(t *testing.T) {
	var shadowHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`)) // primary 2xx so the shadow dispatch fires
	}))
	defer mainUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"main":        {OpenAIBaseURL: mainUp.URL, Provider: "static"},
			"shadow-prov": {AnthropicBaseURL: shadowUp.URL, Provider: "static"}, // cross-proto (anthropic) shadow
		},
		Routes: map[string][]RouteTarget{"m": {{Provider: "main", Model: "m"}}},
		Shadow: map[string]ShadowTarget{"m": {Provider: "shadow-prov", Model: "sm", Protocol: "anthropic"}},
	}
	p := newTestProxy(t, cfg)
	p.providers["main"] = &testProv{key: "main"}
	p.providers["shadow-prov"] = &testProv{key: "shadow-prov"}
	p.initRequestLog(RequestLogConfig{Enabled: true, Dir: filepath.Join(t.TempDir(), "requests")})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// extractModel returns "m" (fast path reads 3 tokens), but the full JSON is
	// malformed → the shadow's openai→anthropic convertRequest fails.
	resp, err := http.Post(px.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m","input":[BAD`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Admission is synchronous; wait for the admitted conversion attempt to
	// finish, then assert the fail-closed path never reached the backend.
	deadline := time.Now().Add(time.Second)
	for len(p.shadow.Load().sem) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if inFlight := len(p.shadow.Load().sem); inFlight != 0 {
		t.Fatalf("shadow conversion attempt did not finish before deadline; in-flight=%d", inFlight)
	}
	if got := shadowHits.Load(); got != 0 {
		t.Errorf("shadow backend hit %d time(s) with an unconverted body after convert failure (fail-open); want 0", got)
	}
}

// TestRunShadow_NilRuntimeConfig: a zero-value runtimeSnapshot (a future call
// site forgetting to populate targetAttempt.runtime) must log + return instead
// of panicking on runtime.cfg deep in runShadow.
func TestRunShadow_NilRuntimeConfig(t *testing.T) {
	p := newTestProxy(t, &Config{Providers: map[string]Provider{}})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	p.runShadow(runtimeSnapshot{}, nil, "anthropic", "anthropic", "m", "g",
		ShadowTarget{Provider: "p", Model: "m"}, []byte(`{}`), "rid")
	if !strings.Contains(buf.String(), "runtime snapshot has no config") {
		t.Fatalf("expected the nil-cfg guard log, got %q", buf.String())
	}
}

// TestCmdShadowReport_InProcess: the `shadow report` CLI renders the daemon's
// /api/shadow-report response. Covers cmdShadow + cmdShadowReport.
func TestCmdShadowReport_InProcess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"enabled": true,
			"entries": []shadowReportEntry{
				{Route: "glm", PrimaryProvider: "zhipu", ShadowProvider: "codex", Samples: 5, StatusMatchRate: 0.8, PrimaryLatencyMs: 100, ShadowLatencyMs: 150, LatencyDiffMs: 50, PrimarySizeAvg: 200, ShadowSizeAvg: 180},
			},
		})
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: http://x}\nroutes:\n  glm: [{provider: zhipu, model: glm}]\n")

	out := captureStdout(t, func() { cmdShadow([]string{"report", "--config", cfgPath}) })
	for _, want := range []string{"glm", "zhipu", "codex", "SAMPLES", "5", "80%"} {
		if !strings.Contains(out, want) {
			t.Errorf("shadow report output missing %q:\n%s", want, out)
		}
	}
}

// TestShadowReport_Unpaired: records without a counterpart are skipped.
func TestShadowReport_Unpaired(t *testing.T) {
	dir := t.TempDir()
	recs := []requestLogRecord{
		{Ts: "2026-07-19T01:00:00Z", RequestID: "orphan", Exposed: "glm", Provider: "p", Status: 200, LatencyMs: 50},
		{Ts: "2026-07-19T01:00:01Z", RequestID: "shadow-orphan", Exposed: "glm", Provider: "s", Status: 500, Shadow: true},
		// A lone primary with no shadow.
		{Ts: "2026-07-19T01:00:02Z", RequestID: "lonely", Exposed: "glm", Provider: "p", Status: 200, LatencyMs: 10},
	}
	var b strings.Builder
	for _, r := range recs {
		line, _ := json.Marshal(r)
		b.Write(line)
		b.WriteByte('\n')
	}
	os.WriteFile(filepath.Join(dir, "requests-20260719-010000.log"), []byte(b.String()), 0o600)
	entries, err := shadowReport(dir, recordFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	// "orphan" + "shadow-orphan" pair up; "lonely" doesn't.
	if len(entries) != 1 {
		t.Errorf("entries=%d want 1 (only orphan pair; lonely unpaired)", len(entries))
	}
}

// TestHandleShadowReport_API: the /api/shadow-report endpoint returns
// enabled=false when request_log is off, and entries when on.
func TestHandleShadowReport_API(t *testing.T) {
	// Off → enabled=false.
	w := newWebServer(newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}), "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/shadow-report", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("shadow-report off: code=%d body=%s", rec.Code, rec.Body.String())
	}
}
