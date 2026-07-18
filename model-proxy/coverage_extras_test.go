package main

import (
	"bytes"
	"io"
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

// TestNeedsConversion + convertSSEReader direction: conversion only triggers on
// a real cross-protocol pair, and the SSE reader picks the right transformer.
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
