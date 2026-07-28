package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

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
