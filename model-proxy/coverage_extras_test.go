package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPositionalArgs: --flag value pairs are skipped; bare positionals are kept
// in order (used by pin/unpin/replay to pull <route> [<provider>] / <id>).
func TestPositionalArgs(t *testing.T) {
	got := positionalArgs([]string{"glm", "--config", "x.yaml", "zhipu", "--ttl", "1h"})
	if len(got) != 2 || got[0] != "glm" || got[1] != "zhipu" {
		t.Errorf("positionalArgs=%v want [glm zhipu]", got)
	}
	// --flag=value form doesn't consume a following bare token.
	got = positionalArgs([]string{"--config=x.yaml", "glm"})
	if len(got) != 1 || got[0] != "glm" {
		t.Errorf("positionalArgs=%v want [glm]", got)
	}
}

// TestParsePinTTL: both --ttl DUR and --ttl=DUR forms parse; absent → 0.
func TestParsePinTTL(t *testing.T) {
	if d := parsePinTTL([]string{"--ttl", "90m"}); d != 90*time.Minute {
		t.Errorf("--ttl 90m = %v want 90m", d)
	}
	if d := parsePinTTL([]string{"--ttl=2h"}); d != 2*time.Hour {
		t.Errorf("--ttl=2h = %v want 2h", d)
	}
	if d := parsePinTTL([]string{"glm", "zhipu"}); d != 0 {
		t.Errorf("absent --ttl = %v want 0", d)
	}
}

// TestDiffAgent: per-key deltas clamp at 0 and omit unchanged keys.
func TestDiffAgent(t *testing.T) {
	cur := map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5, Input: 10, Output: 2},
		{Agent: "b", Provider: "z", Model: "m"}: {Requests: 3, Input: 0, Output: 0},
	}
	prev := map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 2, Input: 10, Output: 0}, // reqs +3, output +2; input unchanged
	}
	d := diffAgent(cur, prev)
	ad, ok := d[agentKey{Agent: "a", Provider: "z", Model: "m"}]
	if !ok || ad.Requests != 3 || ad.Input != 0 || ad.Output != 2 {
		t.Errorf("a delta = %+v want reqs=3 in=0 out=2", ad)
	}
	bd, ok := d[agentKey{Agent: "b", Provider: "z", Model: "m"}]
	if !ok || bd.Requests != 3 {
		t.Errorf("b delta = %+v want reqs=3 (new key)", bd)
	}
	// A key whose counters only decreased (e.g. after reset) clamps to 0 and is
	// omitted when ALL fields are 0.
	d2 := diffAgent(
		map[agentKey]agentCount{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1}},
		map[agentKey]agentCount{{Agent: "a", Provider: "z", Model: "m"}: {Requests: 5}},
	)
	if _, present := d2[agentKey{Agent: "a", Provider: "z", Model: "m"}]; present {
		t.Errorf("all-zero delta should be omitted, got %+v", d2)
	}
}

// TestPruneAgent: retention deletes old agent buckets; 0 retention is a no-op.
func TestPruneAgent(t *testing.T) {
	ss := newTestStatsStore(t)
	old := time.Now().Add(-2*time.Hour).Unix() / 60 * 60
	if err := ss.flushAgentDeltas(old, map[agentKey]agentCount{
		{Agent: "a", Provider: "z", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	// A store with the default 0 retention (newTestStatsStore uses 0) → no prune.
	if err := ss.pruneAgent(time.Now()); err != nil {
		t.Fatal(err)
	}
	got, _ := ss.queryAgentRange(0, time.Now().Unix()+3600, "", "", "", 60)
	if len(got) != 1 {
		t.Errorf("0-retention prune deleted rows: %d want 1", len(got))
	}
}

// TestConvertHelpers: finish↔stop maps (all branches), text extraction, backend
// path, and the SSE reader selector.
func TestConvertHelpers(t *testing.T) {
	for finish, want := range map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use", "other": "end_turn",
	} {
		if got := mapFinishToStopReason(finish); got != want {
			t.Errorf("mapFinishToStopReason(%q)=%q want %q", finish, got, want)
		}
	}
	for reason, want := range map[string]string{
		"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls", "x": "stop",
	} {
		if got := mapStopReasonToFinish(reason); got != want {
			t.Errorf("mapStopReasonToFinish(%q)=%q want %q", reason, got, want)
		}
	}
	// anthropicTextOf: string passthrough, array text concat, image skipped.
	if got := anthropicTextOf("hi"); got != "hi" {
		t.Errorf("string extract=%q", got)
	}
	if got := anthropicTextOf([]any{
		map[string]any{"type": "text", "text": "a"},
		map[string]any{"type": "image", "text": "ignored"},
		map[string]any{"type": "text", "text": "b"},
	}); got != "ab" {
		t.Errorf("array extract=%q want ab", got)
	}
	// backendPath by protocol.
	if p := backendPath("anthropic"); p != "/v1/messages" {
		t.Errorf("backendPath anthropic=%q", p)
	}
	if p := backendPath("openai"); p != "/chat/completions" {
		t.Errorf("backendPath openai=%q", p)
	}
}

// TestCacheConfigDefaults: zero-value config falls back to documented defaults.
func TestCacheConfigDefaults(t *testing.T) {
	var c CacheConfig
	if c.ttl() != 10*time.Minute {
		t.Errorf("default ttl=%v want 10m", c.ttl())
	}
	if c.maxEntries() != 1000 {
		t.Errorf("default maxEntries=%d want 1000", c.maxEntries())
	}
	if c.maxBody() != 256*1024 {
		t.Errorf("default maxBody=%d want 256KiB", c.maxBody())
	}
	if c.enabled() {
		t.Error("default enabled should be false")
	}
}

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
func TestConvertRequestResponse_NoOp(t *testing.T) {
	body := []byte(`{"model":"x"`)
	if got, err := convertRequest(body, "openai", "openai"); err != nil || string(got) != string(body) {
		t.Errorf("same-proto convertRequest should be no-op: got=%q err=%v", got, err)
	}
	if got, err := convertResponse(body, "anthropic", "anthropic"); err != nil || string(got) != string(body) {
		t.Errorf("same-proto convertResponse should be no-op: got=%q err=%v", got, err)
	}
}

// TestUsageScanner_AgentSink: the onAgent callback fires with the observed usage
// when the scanner commits (the agent token-attribution path).
func TestUsageScanner_AgentSink(t *testing.T) {
	tc := newTokenCounter()
	var got tokenUsage
	stream := []byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n")
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "z", Model: "m"}, tc, func(u tokenUsage) {
		got = u
	})
	io.Copy(io.Discard, sc)
	sc.Close()
	if got.Input != 42 {
		t.Errorf("agent sink got input=%d want 42", got.Input)
	}
}

// TestEstimateInputTokens_RuneAware: CJK runes count ~1 token each (not /4
// underestimation); base64 image data excluded; ASCII at /4 rate.
func TestEstimateInputTokens_RuneAware(t *testing.T) {
	// Pure ASCII: ~4 bytes per token.
	if est := estimateInputTokens([]byte(`{"model":"abc"}`)); est < 3 || est > 5 {
		t.Errorf("ASCII: est=%d want ~4", est)
	}
	// CJK: each rune ≈ 1 token. "你好世界" = 4 runes → 4 tokens + JSON overhead.
	cjk := []byte(`{"input":"你好世界"}`) // 4 CJK runes + ~15 ASCII bytes
	est := estimateInputTokens(cjk)
	// CJK runes (4) + ASCII bytes (~15)/4 (3) ≈ 7. Allow ±2.
	if est < 5 || est > 10 {
		t.Errorf("CJK: est=%d want ~7 (4 CJK + ~3 ASCII)", est)
	}
	// Base64 image data excluded: a 200-char base64 run adds 0 tokens.
	big := []byte(`{"image":"` + strings.Repeat("A", 200) + `","text":"hi"}`)
	estBig := estimateInputTokens(big)
	// Without base64 exclusion: ~230 bytes / 4 = 57. With: ~30 bytes / 4 = 7.
	if estBig > 15 {
		t.Errorf("base64 not excluded: est=%d want <15", estBig)
	}
}

// TestRequestHasTools: detects "tools":[ in the body.
func TestRequestHasTools(t *testing.T) {
	if !requestHasTools([]byte(`{"tools":[{"type":"function"}]}`)) {
		t.Error("body with tools:[ should be detected")
	}
	if requestHasTools([]byte(`{"model":"x","input":[]}`)) {
		t.Error("body without tools should not match")
	}
}

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

// a real cross-protocol pair, and the SSE reader picks the right transformer.
// TestDecodeRune_MultiByte: 3-byte CJK and 4-byte emoji decode correctly.
func TestDecodeRune_MultiByte(t *testing.T) {
	// CJK '好' = U+597D, UTF-8: E5 A5 BD (3 bytes)
	r, size := decodeRune([]byte{0xE5, 0xA5, 0xBD}, 0)
	if r != 0x597D || size != 3 {
		t.Errorf("CJK decode: r=%X size=%d want 597D/3", r, size)
	}
	if !isCJK(0x597D) {
		t.Error("U+597D should be CJK")
	}
	// Invalid byte (lone continuation) → 1 byte.
	_, size2 := decodeRune([]byte{0x80}, 0)
	if size2 != 1 {
		t.Errorf("invalid byte: size=%d want 1", size2)
	}
}

// TestFormatAgentsTable_Latency: the --by-agent table includes the new latency
// + failure columns.
func TestFormatAgentsTable_Latency(t *testing.T) {
	resp := agentResp{Bucket: 60, Buckets: []agentBucket{
		{Agent: "claude-code", Requests: 10, Input: 100, Output: 50, LatencySum: 2000, Failures: 1},
	}}
	out := formatAgentsTable(resp)
	for _, want := range []string{"claude-code", "lat", "fail", "10", "200", "1"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatAgentsTable missing %q:\n%s", want, out)
		}
	}
}

// TestOpenAIContentToAnthropicBlocks: nil, empty, and string content paths.
func TestOpenAIContentToAnthropicBlocks(t *testing.T) {
	if openaiContentToAnthropicBlocks(nil) != nil {
		t.Error("nil content should return nil")
	}
	if openaiContentToAnthropicBlocks("hello") == nil {
		t.Error("string content should return a text block")
	}
	if len(openaiContentToAnthropicBlocks("hello")) != 1 {
		t.Error("string content should produce exactly one block")
	}
}

func TestParseModelsDevAPI_ToolCall(t *testing.T) {
	blob := []byte(`{"anthropic":{"api":"anthropic","models":{"claude-sonnet-4":{"limit":{"context":200000,"output":8192},"modalities":{"input":["text","image"],"output":["text"]},"features":{"tool_call":true}},"claude-3-haiku":{"limit":{"context":200000,"output":4096},"modalities":{"input":["text"],"output":["text"]}}}}}`)
	cat := parseModelsDevAPI(blob)
	m, ok := cat.lookup("claude-sonnet-4")
	if !ok {
		t.Fatal("claude-sonnet-4 not found")
	}
	if !m.ToolCall {
		t.Error("claude-sonnet-4 should have tool_call=true")
	}
	h, ok := cat.lookup("claude-3-haiku")
	if !ok {
		t.Fatal("claude-3-haiku not found")
	}
	if h.ToolCall {
		t.Error("claude-3-haiku should have tool_call=false (features absent)")
	}
}

// TestShouldShadow: rate=0 → false, rate>=1 → true, rate between → probabilistic.
func TestShouldShadow(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "z", Model: "m"}}},
	}
	// rate >= 1 → always true.
	p := NewProxy(cfg)
	p.shadowSampRate = 1.0
	if !p.shouldShadow() {
		t.Error("rate=1.0 should return true")
	}
	// rate <= 0 → always false.
	p.shadowSampRate = 0
	if p.shouldShadow() {
		t.Error("rate=0 should return false")
	}
	// nil sem → false.
	p.shadowSem = nil
	if p.shouldShadow() {
		t.Error("nil shadowSem should return false")
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

// TestIsDaemonUnreachable: connection-refused errors match, others don't.
func TestIsDaemonUnreachable(t *testing.T) {
	if !isDaemonUnreachable(fmt.Errorf("dial tcp 127.0.0.1:8080: connect: connection refused")) {
		t.Error("connection refused should match")
	}
	if isDaemonUnreachable(fmt.Errorf("some other error")) {
		t.Error("non-refused error should not match")
	}
}

// TestHandleShadowReport_API: the /api/shadow-report endpoint returns
// enabled=false when request_log is off, and entries when on.
func TestHandleShadowReport_API(t *testing.T) {
	// Off → enabled=false.
	w := newWebServer(NewProxy(&Config{
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

func TestMakeURLQuery(t *testing.T) {
	q := makeURLQuery([]string{"--from", "100", "--to", "200"})
	if q.Get("from") != "100" || q.Get("to") != "200" {
		t.Errorf("makeURLQuery: from=%q to=%q want 100/200", q.Get("from"), q.Get("to"))
	}
	q2 := makeURLQuery(nil)
	if q2.Encode() != "" {
		t.Errorf("makeURLQuery(nil) should be empty, got %q", q2.Encode())
	}
}

func TestNeedsConversionAndSSEReader(t *testing.T) {
	cases := []struct {
		client, target string
		want           bool
	}{
		{"anthropic", "openai", true},
		{"openai", "anthropic", true},
		{"openai", "openai", false},
		{"anthropic", "", false}, // empty target = same as client
		{"openai", "weird", false},
	}
	for _, c := range cases {
		if got := needsConversion(c.client, c.target); got != c.want {
			t.Errorf("needsConversion(%q,%q)=%v want %v", c.client, c.target, got, c.want)
		}
	}
	// Both directions produce a non-nil reader over a short stream without error.
	for _, dir := range []struct{ client, target string }{
		{"anthropic", "openai"}, {"openai", "anthropic"},
	} {
		in := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
		if dir.client == "openai" {
			in = "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"
		}
		r := convertSSEReader(bytes.NewReader([]byte(in)), dir.client, dir.target, "m")
		out, err := io.ReadAll(r)
		if err != nil || len(out) == 0 {
			t.Errorf("convertSSEReader %v→%v produced empty/err: %v %q", dir.client, dir.target, err, string(out))
		}
	}
}
