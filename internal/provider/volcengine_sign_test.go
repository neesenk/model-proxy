package provider

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// TestVolcengineSignV4_KnownAnswer pins the exact signature hex for a fixed
// request, recomputing the documented V4 algorithm inline (raw crypto/hmac +
// sha256 over explicit inputs) rather than calling the production helpers: a
// regression in the canonical request, the credential scope, the signing-key
// chain (SK→kDate→kRegion→kService→kSigning, terminator "request") or the
// header assembly changes the expected hex and turns this red.
func TestVolcengineSignV4_KnownAnswer(t *testing.T) {
	const (
		method         = "GET"
		host           = "open.volcengineapi.com"
		path           = "/"
		canonicalQuery = "Action=GetAFPUsage&Version=2024-01-01"
		ak             = "AKtest123"
		sk             = "SKsecret456"
		region         = "cn-beijing"
		service        = "ark"
	)
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	const xDate = "20260704T120000Z"
	const shortDate = "20260704"

	// --- Independent V4 recomputation (Volcengine docs 6369/67268 + 67270) ---
	hmacStep := func(key []byte, data string) []byte {
		m := hmac.New(sha256.New, key)
		m.Write([]byte(data))
		return m.Sum(nil)
	}
	hashHex := func(s string) string {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:])
	}
	// Each canonical header keeps its trailing \n (the blank line before
	// SignedHeaders in the canonical request).
	canonicalHeaders := "host:" + host + "\n" + "x-date:" + xDate + "\n"
	signedHeaders := "host;x-date"
	canonicalRequest := strings.Join([]string{
		method,
		path,
		canonicalQuery,
		canonicalHeaders,
		signedHeaders,
		hashHex(""), // GET, empty payload
	}, "\n")
	credentialScope := shortDate + "/" + region + "/" + service + "/request"
	stringToSign := strings.Join([]string{
		"HMAC-SHA256",
		xDate,
		credentialScope,
		hashHex(canonicalRequest),
	}, "\n")
	kDate := hmacStep([]byte(sk), shortDate)
	kRegion := hmacStep(kDate, region)
	kService := hmacStep(kRegion, service)
	kSigning := hmacStep(kService, "request")
	wantSig := hex.EncodeToString(hmacStep(kSigning, stringToSign))
	// Pinned known answer for this exact vector; the inline recomputation must
	// reproduce it, so a test-side rewrite cannot silently follow a production
	// regression for these fixed inputs.
	const pinnedSig = "6957b8ecab7b279251342ca4ea07ce0801ef095c6becacc97968ff81746dea8a"
	if wantSig != pinnedSig {
		t.Errorf("independent recomputation = %q, want the pinned known answer %q", wantSig, pinnedSig)
	}
	wantAuth := "HMAC-SHA256 Credential=" + ak + "/" + credentialScope +
		", SignedHeaders=" + signedHeaders + ", Signature=" + wantSig

	gotXDate, gotAuth := volcengineSignV4(method, host, path, canonicalQuery, []byte(""), now, ak, sk, region, service)
	if gotXDate != xDate {
		t.Errorf("X-Date = %q, want %q", gotXDate, xDate)
	}
	if gotAuth != wantAuth {
		t.Errorf("Authorization:\n got %q\nwant %q", gotAuth, wantAuth)
	}
}
