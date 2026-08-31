package guard

import (
	"encoding/json"
	"strings"
	"testing"
)

// defaultRulesScanner scans with the embedded rule table only (no known
// secrets, custom patterns, or extra paths) — the configuration the removed
// package-level Scan/Redact shims delegated to.
var defaultRulesScanner = mustDefaultScanner()

func mustDefaultScanner() *Scanner {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		panic(err) // impossible: no custom input to reject
	}
	return s
}

// Positive fixtures are synthetic secrets shaped like real ones (no real
// credentials anywhere in tests — AGENTS.md credential red line).
var positiveCases = []struct {
	name string // expected pattern type name
	body string
}{
	{"pem_private_key", "-----BEGIN PRIVATE KEY-----\nMIIEvQ...\n-----END PRIVATE KEY-----"},
	{"pem_private_key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpA...\n-----END RSA PRIVATE KEY-----"},
	{"pem_private_key", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaA==\n-----END OPENSSH PRIVATE KEY-----"},
	{"pem_private_key", "-----BEGIN ENCRYPTED PRIVATE KEY-----\nMIIF...\n-----END ENCRYPTED PRIVATE KEY-----"},
	{"pem_private_key", "-----BEGIN PGP PRIVATE KEY BLOCK-----\n\nxu4E...\n-----END PGP PRIVATE KEY BLOCK-----"},
	{"aws_access_key_id", `aws_access_key_id = AKIA` + newFixtureRNG(0xa51a).chars(16, alphaUpper32)},
	{"aws_access_key_id", `ASIA` + newFixtureRNG(0xb51a).chars(16, alphaUpper32) + ` # temporary session key`},
	{"openai_api_key", `"api_key": "sk-` + strings.Repeat("aB3", 16) + `"`},
	{"openai_api_key", `sk-proj-` + strings.Repeat("xY-_9z", 10)},
	{"anthropic_api_key", `sk-ant-api03-` + strings.Repeat("qW7", 30)},
	{"github_token", `token = ghp_` + strings.Repeat("Ab12", 9)},
	{"github_token", `gho_` + strings.Repeat("Cd34", 9)},
	{"github_fine_grained_pat", `github_pat_` + strings.Repeat("11AA", 6) + `_` + strings.Repeat("zz99", 15)},
	{"google_api_key", `key=AIza` + strings.Repeat("Sy0_", 8) + `AbC`},
}

// Negative fixtures: normal code/doc content that must NOT trip the
// high-confidence table.
var negativeCases = []string{
	// Public material / non-private PEM blocks.
	"-----BEGIN PUBLIC KEY-----\nMIIB...\n-----END PUBLIC KEY-----",
	"-----BEGIN CERTIFICATE-----\nMIIF...\n-----END CERTIFICATE-----",
	"-----BEGIN RSA PUBLIC KEY-----\nMIIB...\n-----END RSA PUBLIC KEY-----",
	// The words "private key" in prose/comments are fine.
	`// loadPrivateKey reads the private key from disk`,
	`return fmt.Errorf("invalid private key: %w", err)`,
	// Short / truncated lookalikes.
	`sk-` + strings.Repeat("ab", 6),     // too short
	`sk-ant-` + strings.Repeat("x", 10), // anthropic prefix, too short
	`AKIA` + strings.Repeat("A", 10),    // AWS prefix, too short
	// Regex-shaped but below the entropy threshold (aws_access_key_id: 3.0) —
	// the base64-attachment false-positive shape.
	`AKIA` + strings.Repeat("A", 16),
	`AKIA` + strings.Repeat("AB", 8),
	`ghp_` + strings.Repeat("a1", 10),       // GitHub prefix, too short
	`github_pat_` + strings.Repeat("b2", 5), // fine-grained PAT, too short
	`AIza` + strings.Repeat("c3", 8),        // Google prefix, too short
	// Env-var names and config keys, not values.
	`os.Getenv("OPENAI_API_KEY")`,
	`AWS_ACCESS_KEY_ID=xxxxx AWS_SECRET_ACCESS_KEY=yyyyy`,
	`strings.HasPrefix(tok, "sk-")`,
	// Ordinary identifiers and random blobs without issuer prefixes.
	`const maxRetries = 0x5f3759df`,
	strings.Repeat("aGVsbG8gd29ybGQ=", 8), // plain base64 blob
	`ask-` + strings.Repeat("def", 12),    // near-prefix "ask-"
}

func TestScanPositives(t *testing.T) {
	for _, tc := range positiveCases {
		got := defaultRulesScanner.Scan([]byte(tc.body))
		if len(got) != 1 || got[0] != tc.name {
			t.Errorf("Scan(%q prefix) = %v, want exactly [%s]", tc.body[:min(24, len(tc.body))], got, tc.name)
		}
	}
}

func TestScanNegatives(t *testing.T) {
	for _, body := range negativeCases {
		if got := defaultRulesScanner.Scan([]byte(body)); len(got) != 0 {
			t.Errorf("Scan(%q…) = %v, want no hits", body[:min(32, len(body))], got)
		}
	}
}

func TestScanCleanBody(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"explain the observer pattern in Go"}]}`
	if got := defaultRulesScanner.Scan([]byte(body)); len(got) != 0 {
		t.Errorf("Scan(normal request) = %v, want no hits", got)
	}
}

// An sk-ant- key also fits the generic sk- shape; Scan must report only the
// more specific anthropic_api_key (span claiming, no double report).
func TestScanOverlapClaimsMoreSpecificPattern(t *testing.T) {
	body := `key = sk-ant-api03-` + strings.Repeat("qW7", 30)
	got := defaultRulesScanner.Scan([]byte(body))
	if len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("Scan(sk-ant key) = %v, want exactly [anthropic_api_key]", got)
	}
}

// Multiple different secrets in one body report every type once, in table order.
func TestScanMultipleTypes(t *testing.T) {
	body := "ghp_" + strings.Repeat("Ab12", 9) + "\n-----BEGIN RSA PRIVATE KEY-----\nx"
	got := defaultRulesScanner.Scan([]byte(body))
	want := []string{"github_token", "pem_private_key"}
	if len(got) != len(want) {
		t.Fatalf("Scan = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("Scan[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRedact(t *testing.T) {
	secret := "AKIA" + newFixtureRNG(0xedac).chars(16, alphaUpper32)
	body := `{"model":"m","messages":[{"role":"user","content":"my key is ` + secret + `, and PEM:\n-----BEGIN PRIVATE KEY-----\nABC"}]}`
	out := string(defaultRulesScanner.Redact([]byte(body)))
	if strings.Contains(out, secret) || strings.Contains(out, "BEGIN PRIVATE KEY") {
		t.Errorf("Redact left secret material in body")
	}
	if got := strings.Count(out, RedactPlaceholder); got != 2 {
		t.Errorf("Redact placeholder count = %d, want 2", got)
	}
	if !strings.Contains(out, `"my key is `+RedactPlaceholder+`, and PEM:`) {
		t.Errorf("Redact mangled surrounding JSON content: %s", out)
	}
}

func TestRedactCleanBodyUnchanged(t *testing.T) {
	body := []byte(`{"model":"m","messages":[]}`)
	out := defaultRulesScanner.Redact(body)
	if string(out) != string(body) {
		t.Errorf("Redact(clean) = %q, want unchanged %q", out, body)
	}
}

// A redacted JSON body must stay parseable-shaped: the placeholder substitutes
// inside the string without breaking the quotes around it.
func TestRedactKeepsJSONShape(t *testing.T) {
	body := `{"model":"m","key":"sk-` + strings.Repeat("aB3", 16) + `"}`
	out := string(defaultRulesScanner.Redact([]byte(body)))
	want := `{"model":"m","key":"` + RedactPlaceholder + `"}`
	if out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}

// TestScanDetectsKeyBuriedAfterLargeCleanPrefix: the literal prefilter must
// not weaken detection for secrets placed deep inside a large body — the
// realistic accident shape (key pasted into a long prompt tail).
func TestScanDetectsKeyBuriedAfterLargeCleanPrefix(t *testing.T) {
	clean := strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1536) // ~64KB
	for _, tc := range positiveCases {
		body := []byte(clean + tc.body)
		got := defaultRulesScanner.Scan(body)
		if len(got) != 1 || got[0] != tc.name {
			t.Errorf("%s buried after 64KB clean prefix: Scan = %v", tc.name, got)
		}
		if redacted := string(defaultRulesScanner.Redact(body)); strings.Contains(redacted, strings.TrimSpace(tc.body)) ||
			len(redacted) >= len(body) {
			t.Errorf("%s buried after 64KB clean prefix: Redact did not replace the secret", tc.name)
		}
	}
}

// TestRedactCaptureGroupSpanKeepsJSONValid: gitleaks-derived rules capture the
// secret in group 1 and consume a trailing context byte (quote/whitespace/;)
// in a NON-captured group of the full match. The claim/redact span must be the
// group span: claiming the full match makes Redact eat the closing quote of a
// JSON string value, and the corrupted body forwarded upstream is invalid JSON
// (it also breaks the ScanPathsContext structure walk downstream — see the
// app-level TestGuardRedactKeepsPathBlockChain). For every representative
// rule: the redacted body must stay valid JSON, carry no secret bytes, and
// equal the exact group-span substitution.
func TestRedactCaptureGroupSpanKeepsJSONValid(t *testing.T) {
	rng := newFixtureRNG(0x9a1e)
	c := func(n int, alphabet string) string { return rng.chars(n, alphabet) }
	cases := []struct {
		name   string // expected pattern type name
		secret string
	}{
		{"stripe_access_token", "sk_live_" + c(24, alphaAlnum)},
		{"huggingface_access_token", "hf_" + c(34, "abcdefghijklmnopqrstuvwxyz")},
		{"npm_access_token", "npm_" + c(36, alphaLower36)},
		{"grafana_service_account_token", "glsa_" + c(32, alphaAlnum) + "_" + c(8, alphaHex)},
	}
	for _, tc := range cases {
		// Secret at the end of a JSON string value, mid-object and as the
		// last value: both shapes put a closing quote right after the secret,
		// which the full-match span would consume.
		for _, body := range []string{
			`{"api_key":"` + tc.secret + `","other":1}`,
			`{"api_key":"` + tc.secret + `"}`,
		} {
			if got := defaultRulesScanner.Scan([]byte(body)); len(got) != 1 || got[0] != tc.name {
				t.Errorf("%s: Scan = %v, want exactly [%s]", tc.name, got, tc.name)
			}
			out := defaultRulesScanner.Redact([]byte(body))
			if !json.Valid(out) {
				t.Errorf("%s: redacted body is not valid JSON: %s", tc.name, out)
			}
			if strings.Contains(string(out), tc.secret) {
				t.Errorf("%s: redacted body still carries the secret", tc.name)
			}
			want := strings.Replace(body, tc.secret, RedactPlaceholder, 1)
			if string(out) != want {
				t.Errorf("%s: Redact = %q, want exact group-span substitution %q", tc.name, out, want)
			}
		}
	}
}

// TestRedactNoCaptureGroupClaimsFullMatch: a rule whose regex has no capture
// group keeps claiming the FULL match — the regex itself is the secret shape,
// so nothing less would remove the secret. gitlab_pat is the entropy-carrying
// no-group representative (openai_api_key covers the no-entropy case in
// TestRedactKeepsJSONShape).
func TestRedactNoCaptureGroupClaimsFullMatch(t *testing.T) {
	secret := "glpat-" + newFixtureRNG(0xf011).chars(20, alphaWord)
	body := `{"token":"` + secret + `","other":1}`
	out := string(defaultRulesScanner.Redact([]byte(body)))
	want := `{"token":"` + RedactPlaceholder + `","other":1}`
	if out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
	if !json.Valid([]byte(out)) || strings.Contains(out, secret) {
		t.Errorf("redacted body invalid or still carries the secret: %q", out)
	}
}

func BenchmarkScanCleanBody64K(b *testing.B) {
	body := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1536))
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if names := defaultRulesScanner.Scan(body); len(names) != 0 {
			b.Fatalf("clean body reported %v", names)
		}
	}
}

func BenchmarkScanBody64KWithKey(b *testing.B) {
	body := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 1536) +
		`"api_key": "sk-` + strings.Repeat("aB3", 16) + `"`)
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if names := defaultRulesScanner.Scan(body); len(names) != 1 || names[0] != "openai_api_key" {
			b.Fatalf("key body reported %v", names)
		}
	}
}
