package protocol

import (
	"strings"
	"testing"
)

// TestConvertRequestDiagnostics: lossy-but-degradable request conversions
// collect structured diagnostics with stable codes alongside the legacy log.
func TestConvertRequestDiagnostics(t *testing.T) {
	diag := NewDiagnostics()
	body := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop":"END"}`)
	out, err := ConvertRequestWithOptions(body, Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: diag})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"input"`) {
		t.Fatalf("conversion broken: %s", out)
	}
	if !diag.HasCode("stop_dropped") {
		t.Errorf("stop_dropped missing: %+v", diag.Items())
	}

	// chat→anthropic: a percent-encoded data URI degrades observably.
	diagA := NewDiagnostics()
	outA, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[
			{"type":"text","text":"hi"},
			{"type":"image_url","image_url":{"url":"data:text/html,%3Cb%3E"}}]}]}`),
		Protocol("openai"), Protocol("anthropic"),
		RequestOptions{ImageOK: true, Diag: diagA})
	if err != nil {
		t.Fatal(err)
	}
	_ = outA
	if !diagA.HasCode("data_uri_dropped") {
		t.Errorf("data_uri_dropped missing: %+v", diagA.Items())
	}
}

// TestConvertRequestStrictLossy: strict mode refuses lossy conversions
// through the capability-scanner channel (target skip + 400 envelope reuse).
func TestConvertRequestStrictLossy(t *testing.T) {
	diag := NewDiagnostics()
	_, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"stop":"END"}`),
		Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: diag, StrictLossy: true})
	unsupported, ok := AsUnsupported(err)
	if !ok {
		t.Fatalf("strict lossy error = %v, want unsupported channel", err)
	}
	if !strings.HasPrefix(unsupported.Feature, "strict_lossy:") ||
		!strings.Contains(unsupported.Feature, "stop_dropped") {
		t.Fatalf("unsupported feature = %q", unsupported.Feature)
	}

	// A clean conversion passes strict mode untouched.
	clean, err := ConvertRequestWithOptions(
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`),
		Protocol("openai"), Protocol("responses"),
		RequestOptions{ImageOK: true, Diag: NewDiagnostics(), StrictLossy: true})
	if err != nil {
		t.Fatalf("clean conversion refused under strict: %v", err)
	}
	if !strings.Contains(string(clean), `"input"`) {
		t.Fatalf("clean conversion broken: %s", clean)
	}
}
