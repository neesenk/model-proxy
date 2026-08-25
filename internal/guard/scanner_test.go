package guard

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// All secrets here are synthetic repeated/PRNG strings — no real credentials.

func mustScanner(t *testing.T, custom []CustomPattern, secrets, extraPaths []string) *Scanner {
	t.Helper()
	s, err := NewScanner(custom, secrets, extraPaths)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}
	if s == nil {
		t.Fatal("NewScanner returned nil scanner with nil error")
	}
	return s
}

// syntheticSecret returns a deterministic high-entropy secret-shaped string.
func syntheticSecret(prefix string, n int) string {
	rng := newFixtureRNG(0xbeef + uint64(n))
	return prefix + rng.chars(n, alphaAlnum)
}

func TestNewScannerNeverNilOnSuccess(t *testing.T) {
	s := mustScanner(t, nil, nil, nil)
	if got := s.Scan([]byte("hello world")); len(got) != 0 {
		t.Errorf("clean body: Scan = %v, want no hits", got)
	}
	// Embedded table is always active, even with no secrets/custom patterns.
	key := "sk-ant-api03-" + strings.Repeat("qW7", 30)
	if got := s.Scan([]byte(key)); len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("embedded rule: Scan = %v, want [anthropic_api_key]", got)
	}
}

func TestNewScannerRejectsBadCustomPatterns(t *testing.T) {
	re := regexp.MustCompile(`x+`)
	cases := []struct {
		name    string
		custom  []CustomPattern
		wantErr string
	}{
		{"empty name", []CustomPattern{{Name: "", RE: re}}, "empty name"},
		{"nil regexp", []CustomPattern{{Name: "foo"}}, "nil regexp"},
		{"duplicate name", []CustomPattern{{Name: "foo", RE: re}, {Name: "foo", RE: re}}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewScanner(tc.custom, nil, nil)
			if err == nil {
				t.Fatalf("NewScanner = nil error, want %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("NewScanner error = %v, want substring %q", err, tc.wantErr)
			}
			if s != nil {
				t.Errorf("NewScanner returned non-nil scanner on error")
			}
		})
	}
}

func TestKnownSecretPlaintext(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	body := "please explain this token: " + secret + " thanks"
	got := s.Scan([]byte(body))
	if len(got) != 1 || got[0] != knownSecret {
		t.Fatalf("Scan = %v, want [%s]", got, knownSecret)
	}
	if strings.Contains(got[0], secret) {
		t.Errorf("reported name contains secret material")
	}
	out := string(s.Redact([]byte(body)))
	if strings.Contains(out, secret) {
		t.Errorf("Redact left the known secret in the body")
	}
	if !strings.Contains(out, RedactPlaceholder) {
		t.Errorf("Redact output missing placeholder: %q", out)
	}
}

func TestKnownSecretMinLength(t *testing.T) {
	short := syntheticSecret("k", 8) // 9 chars < 12
	s := mustScanner(t, nil, []string{short}, nil)
	if got := s.Scan([]byte("value: " + short)); len(got) != 0 {
		t.Errorf("short secret must not be collected: Scan = %v", got)
	}
	exact := syntheticSecret("k", 11) // exactly 12 chars
	s2 := mustScanner(t, nil, []string{exact}, nil)
	if got := s2.Scan([]byte("value: " + exact)); len(got) != 1 || got[0] != knownSecret {
		t.Errorf("12-char secret must be collected: Scan = %v", got)
	}
}

func TestKnownSecretEncodedVariants(t *testing.T) {
	secret := syntheticSecret("pool/key+=", 40) // special chars exercise url escaping
	s := mustScanner(t, nil, []string{secret}, nil)
	variants := map[string]string{
		"b64-std":    base64.StdEncoding.EncodeToString([]byte(secret)),
		"b64-raw":    base64.RawStdEncoding.EncodeToString([]byte(secret)),
		"b64-url":    base64.URLEncoding.EncodeToString([]byte(secret)),
		"b64-rawurl": base64.RawURLEncoding.EncodeToString([]byte(secret)),
		"hex":        hex.EncodeToString([]byte(secret)),
		"urlenc":     url.QueryEscape(secret),
	}
	for name, v := range variants {
		if v == secret {
			continue // dedup'd variant identical to raw
		}
		body := "payload: " + v + " end"
		got := s.Scan([]byte(body))
		if len(got) != 1 || got[0] != knownSecretEncoded {
			t.Errorf("%s variant: Scan = %v, want [%s]", name, got, knownSecretEncoded)
		}
		out := string(s.Redact([]byte(body)))
		if strings.Contains(out, v) || strings.Contains(out, secret) {
			t.Errorf("%s variant: Redact left encoded secret bytes", name)
		}
	}
	// Names must never carry secret material.
	for _, m := range s.Scan([]byte(secret)) {
		if strings.Contains(m, secret[:12]) {
			t.Errorf("name %q leaks secret material", m)
		}
	}
}

// A known secret that also matches an embedded rule reports only
// known_secret: exact values win over pattern shapes.
func TestKnownSecretClaimsBeforeEmbeddedRules(t *testing.T) {
	secret := "sk-ant-api03-" + strings.Repeat("qW7", 30) // anthropic-shaped pool key
	s := mustScanner(t, nil, []string{secret}, nil)
	got := s.Scan([]byte("key = " + secret))
	if len(got) != 1 || got[0] != knownSecret {
		t.Errorf("Scan = %v, want exactly [%s]", got, knownSecret)
	}
}

// Encoded channel: a rule literal inside a base64 blob earns a bounded decode;
// the owning rule's regex must match the decoded text. Covers all three byte
// alignments and the std/url alphabets.
func TestEncodedChannelBase64Alignments(t *testing.T) {
	secret := "glpat-" + newFixtureRNG(0xc0de).chars(20, alphaWord)
	s := mustScanner(t, nil, nil, nil)
	encodings := map[string]*base64.Encoding{
		"std":    base64.StdEncoding,
		"raw":    base64.RawStdEncoding,
		"url":    base64.URLEncoding,
		"rawurl": base64.RawURLEncoding,
	}
	for align := 0; align < 3; align++ {
		for encName, enc := range encodings {
			blob := enc.EncodeToString(append([]byte(strings.Repeat("x", align)), secret...))
			body := "data: " + blob + " end"
			got := s.Scan([]byte(body))
			if len(got) != 1 || got[0] != "gitlab_pat" {
				t.Errorf("align=%d enc=%s: Scan = %v, want [gitlab_pat]", align, encName, got)
			}
			out := string(s.Redact([]byte(body)))
			if strings.Contains(out, blob) || strings.Contains(out, secret) {
				t.Errorf("align=%d enc=%s: Redact left encoded/plain secret bytes", align, encName)
			}
			if !strings.Contains(out, "data: "+RedactPlaceholder+" end") {
				t.Errorf("align=%d enc=%s: Redact mangled surrounding text: %q", align, encName, out)
			}
		}
	}
}

// Encoded channel: hex form of a secret value.
func TestEncodedChannelHex(t *testing.T) {
	secret := "glpat-" + newFixtureRNG(0xfeed).chars(20, alphaWord)
	s := mustScanner(t, nil, nil, nil)
	blob := hex.EncodeToString([]byte(secret))
	body := "hex: " + blob + " end"
	if got := s.Scan([]byte(body)); len(got) != 1 || got[0] != "gitlab_pat" {
		t.Fatalf("Scan = %v, want [gitlab_pat]", got)
	}
	out := string(s.Redact([]byte(body)))
	if strings.Contains(out, blob) || strings.Contains(out, secret) {
		t.Errorf("Redact left hex/plain secret bytes")
	}
}

// Encoded channel keeps the rule's entropy post-filter: a base64 blob whose
// decoded text matches the regex shape but scores below the entropy threshold
// is not claimed.
func TestEncodedChannelEntropyFilter(t *testing.T) {
	low := "glpat-" + strings.Repeat("a", 20)
	blob := base64.StdEncoding.EncodeToString([]byte(low))
	s := mustScanner(t, nil, nil, nil)
	if got := s.Scan([]byte("data: " + blob)); len(got) != 0 {
		t.Errorf("Scan = %v, want no hits (entropy filter on decoded text)", got)
	}
}

// Encoded channel must not fire on arbitrary base64 content.
func TestEncodedChannelCleanBase64(t *testing.T) {
	blob := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("the quick brown fox. ", 20)))
	s := mustScanner(t, nil, nil, nil)
	if got := s.Scan([]byte("data: " + blob)); len(got) != 0 {
		t.Errorf("Scan = %v, want no hits", got)
	}
}

func TestCustomPatterns(t *testing.T) {
	custom := []CustomPattern{
		{Name: "myvendor_key", RE: regexp.MustCompile(`\bmv-[A-Za-z0-9]{8,}`), Literal: []byte("mv-")},
		{Name: "other_key", RE: regexp.MustCompile(`\bok-[A-Za-z0-9]{8,}`)}, // no literal: always runs
	}
	s := mustScanner(t, custom, nil, nil)
	if got := s.Scan([]byte(`key = "mv-abc12345"`)); len(got) != 1 || got[0] != "myvendor_key" {
		t.Errorf("custom with literal: Scan = %v", got)
	}
	if got := s.Scan([]byte(`key = "ok-abc12345"`)); len(got) != 1 || got[0] != "other_key" {
		t.Errorf("custom without literal: Scan = %v", got)
	}
	if got := s.Scan([]byte(`key = "mv-short"`)); len(got) != 0 {
		t.Errorf("custom too-short: Scan = %v", got)
	}
	out := string(s.Redact([]byte(`key = "mv-abc12345"`)))
	if strings.Contains(out, "mv-abc12345") {
		t.Errorf("Redact left custom secret in body")
	}
}

// Custom patterns do not participate in the encoded channel.
func TestCustomPatternNoEncodedChannel(t *testing.T) {
	custom := []CustomPattern{
		{Name: "myvendor_key", RE: regexp.MustCompile(`\bmv-[A-Za-z0-9]{8,}`), Literal: []byte("mv-")},
	}
	s := mustScanner(t, custom, nil, nil)
	blob := base64.StdEncoding.EncodeToString([]byte("mv-abc12345xyz"))
	if got := s.Scan([]byte("data: " + blob)); len(got) != 0 {
		t.Errorf("custom patterns must not get encoded variants: Scan = %v", got)
	}
}

// Overlapping spans: the first claimant in priority order wins, the loser is
// neither reported nor double-redacted.
func TestOverlapFirstClaimWins(t *testing.T) {
	// An embedded rule (anthropic, earlier in the table) beats the looser
	// openai shape on the same bytes.
	body := "key = sk-ant-api03-" + strings.Repeat("qW7", 30)
	s := mustScanner(t, nil, nil, nil)
	if got := s.Scan([]byte(body)); len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("Scan = %v, want [anthropic_api_key]", got)
	}
	// Custom loses to embedded on the same span.
	custom := []CustomPattern{
		{Name: "sk_catchall", RE: regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]+`), Literal: []byte("sk-ant-")},
	}
	s2 := mustScanner(t, custom, nil, nil)
	if got := s2.Scan([]byte(body)); len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("embedded must claim before custom: Scan = %v", got)
	}
	out := string(s2.Redact([]byte(body)))
	if strings.Count(out, RedactPlaceholder) != 1 {
		t.Errorf("overlapping span redacted twice: %q", out)
	}
}

// Scan name order: known secrets first, then embedded table order, then
// custom declaration order.
func TestScanNameOrder(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	custom := []CustomPattern{
		{Name: "myvendor_key", RE: regexp.MustCompile(`\bmv-[A-Za-z0-9]{8,}`), Literal: []byte("mv-")},
	}
	s := mustScanner(t, custom, []string{secret}, nil)
	body := secret + "\n-----BEGIN PRIVATE KEY-----\nx\nmv-abc12345\n" + "sk-ant-api03-" + strings.Repeat("qW7", 30)
	want := []string{knownSecret, "anthropic_api_key", "pem_private_key", "myvendor_key"}
	got := s.Scan([]byte(body))
	if len(got) != len(want) {
		t.Fatalf("Scan = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Scan = %v, want %v", got, want)
		}
	}
}

// Multiple occurrences of the same secret collapse to one reported name.
func TestScanDedupesRepeatedHits(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	body := secret + " and again " + secret
	if got := s.Scan([]byte(body)); len(got) != 1 || got[0] != knownSecret {
		t.Errorf("Scan = %v, want exactly [%s]", got, knownSecret)
	}
	out := string(s.Redact([]byte(body)))
	if strings.Contains(out, secret) || strings.Count(out, RedactPlaceholder) != 2 {
		t.Errorf("Redact = %q, want both occurrences replaced", out)
	}
}

// Redact must not leak any form of any secret in a mixed body.
func TestRedactNoSecretLeak(t *testing.T) {
	secret := syntheticSecret("pool/key+=", 40)
	s := mustScanner(t, nil, []string{secret}, nil)
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	ruleSecret := "glpat-" + newFixtureRNG(0xf00d).chars(20, alphaWord)
	ruleBlob := base64.StdEncoding.EncodeToString([]byte("xx" + ruleSecret))
	body := strings.Join([]string{"a", secret, b64, ruleSecret, ruleBlob, "z"}, " | ")
	out := string(s.Redact([]byte(body)))
	for _, leak := range []string{secret, b64, ruleSecret, ruleBlob,
		hex.EncodeToString([]byte(secret)), url.QueryEscape(secret)} {
		if strings.Contains(out, leak) {
			t.Errorf("Redact output still contains %q", leak[:min(24, len(leak))])
		}
	}
}

// --- Options{Decode} switch (config guard.decode) ---

// Decode off: no encoded-literal probes are built, a base64'd pool key does
// NOT hit, but its plaintext form still does.
func TestScannerDecodeOffDisablesEncodedChannels(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s, err := NewScannerWithOptions(nil, []string{secret}, nil, Options{Decode: false})
	if err != nil {
		t.Fatalf("NewScannerWithOptions: %v", err)
	}
	if len(s.probes) != 0 {
		t.Errorf("decode off: probes = %d, want 0", len(s.probes))
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	if got := s.Scan([]byte("payload: " + b64 + " end")); len(got) != 0 {
		t.Errorf("decode off: base64 form must not hit: Scan = %v", got)
	}
	if got := s.Scan([]byte("payload: " + hex.EncodeToString([]byte(secret)) + " end")); len(got) != 0 {
		t.Errorf("decode off: hex form must not hit: Scan = %v", got)
	}
	got := s.Scan([]byte("payload: " + secret + " end"))
	if len(got) != 1 || got[0] != knownSecret {
		t.Errorf("decode off: plaintext must still hit: Scan = %v, want [%s]", got, knownSecret)
	}
	// The embedded encoded channel is off too: a base64 blob containing a rule
	// literal no longer earns a decode.
	ruleKey := "glpat-" + newFixtureRNG(0xc0de).chars(20, alphaWord)
	blob := base64.StdEncoding.EncodeToString([]byte(ruleKey))
	if got := s.Scan([]byte("data: " + blob + " end")); len(got) != 0 {
		t.Errorf("decode off: encoded rule form must not hit: Scan = %v", got)
	}
	// Plaintext embedded-rule matching is unaffected.
	if got := s.Scan([]byte("data: " + ruleKey + " end")); len(got) != 1 || got[0] != "gitlab_pat" {
		t.Errorf("decode off: plaintext rule must still hit: Scan = %v, want [gitlab_pat]", got)
	}
}

// NewScanner keeps its historical signature and defaults to decode on.
func TestNewScannerDefaultsDecodeOn(t *testing.T) {
	secret := syntheticSecret("poolkey-", 32)
	s := mustScanner(t, nil, []string{secret}, nil)
	if len(s.probes) == 0 {
		t.Error("decode on: probes must be built")
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(secret))
	if got := s.Scan([]byte("payload: " + b64 + " end")); len(got) != 1 || got[0] != knownSecretEncoded {
		t.Errorf("decode on: base64 form must hit: Scan = %v, want [%s]", got, knownSecretEncoded)
	}
}
