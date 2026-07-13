package provider

import (
	"strings"
	"testing"
	"time"
)

// TestVolcengineSign_DeterministicAndWellFormed checks the V4 signature is
// deterministic for fixed inputs and produces a well-formed Authorization header
// (HMAC-SHA256 / credential scope / signed headers / 64-hex signature). Live
// correctness against Volcengine is verified separately with real AK/SK.
func TestVolcengineSign_DeterministicAndWellFormed(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	ak, sk := "AKtest123", "SKsecret456"
	req1, err := volcengineGet("GetAFPUsage", "2024-01-01", ak, sk, now, "")
	if err != nil {
		t.Fatalf("volcengineGet: %v", err)
	}
	req2, _ := volcengineGet("GetAFPUsage", "2024-01-01", ak, sk, now, "")

	a1 := req1.Header.Get("Authorization")
	a2 := req2.Header.Get("Authorization")
	if a1 != a2 {
		t.Errorf("signature not deterministic:\n %q\n %q", a1, a2)
	}
	if !strings.HasPrefix(a1, "HMAC-SHA256 Credential=AKtest123/20260704/cn-beijing/ark/request, ") {
		t.Errorf("credential scope wrong: %q", a1)
	}
	if !strings.Contains(a1, "SignedHeaders=host;x-date") {
		t.Errorf("signed headers wrong: %q", a1)
	}
	i := strings.LastIndex(a1, "Signature=")
	sig := a1[i+len("Signature="):]
	if len(sig) != 64 {
		t.Errorf("signature length = %d, want 64 (hex sha256): %q", len(sig), sig)
	}
	if got := req1.Header.Get("X-Date"); got != "20260704T120000Z" {
		t.Errorf("X-Date = %q, want 20260704T120000Z", got)
	}
	if h := req1.Header.Get("X-Content-Sha256"); h != "" {
		t.Errorf("X-Content-Sha256 should NOT be set for GET, got %q", h)
	}
	if !strings.Contains(req1.URL.String(), "Action=GetAFPUsage&Version=2024-01-01") {
		t.Errorf("URL missing Action/Version: %q", req1.URL.String())
	}
}
