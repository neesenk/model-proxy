package guard

import (
	"strings"
	"testing"
)

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
	{"aws_access_key_id", `aws_access_key_id = AKIA` + strings.Repeat("A1", 8)},
	{"aws_access_key_id", `ASIA` + strings.Repeat("Z9", 8) + ` # temporary session key`},
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
	`sk-` + strings.Repeat("ab", 6),         // too short
	`sk-ant-` + strings.Repeat("x", 10),     // anthropic prefix, too short
	`AKIA` + strings.Repeat("A", 10),        // AWS prefix, too short
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
		got := Scan([]byte(tc.body))
		if len(got) != 1 || got[0] != tc.name {
			t.Errorf("Scan(%q prefix) = %v, want exactly [%s]", tc.body[:min(24, len(tc.body))], got, tc.name)
		}
	}
}

func TestScanNegatives(t *testing.T) {
	for _, body := range negativeCases {
		if got := Scan([]byte(body)); len(got) != 0 {
			t.Errorf("Scan(%q…) = %v, want no hits", body[:min(32, len(body))], got)
		}
	}
}

func TestScanCleanBody(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"explain the observer pattern in Go"}]}`
	if got := Scan([]byte(body)); len(got) != 0 {
		t.Errorf("Scan(normal request) = %v, want no hits", got)
	}
}

// An sk-ant- key also fits the generic sk- shape; Scan must report only the
// more specific anthropic_api_key (span claiming, no double report).
func TestScanOverlapClaimsMoreSpecificPattern(t *testing.T) {
	body := `key = sk-ant-api03-` + strings.Repeat("qW7", 30)
	got := Scan([]byte(body))
	if len(got) != 1 || got[0] != "anthropic_api_key" {
		t.Errorf("Scan(sk-ant key) = %v, want exactly [anthropic_api_key]", got)
	}
}

// Multiple different secrets in one body report every type once, in table order.
func TestScanMultipleTypes(t *testing.T) {
	body := "ghp_" + strings.Repeat("Ab12", 9) + "\n-----BEGIN RSA PRIVATE KEY-----\nx"
	got := Scan([]byte(body))
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
	secret := "AKIA" + strings.Repeat("A1", 8)
	body := `{"model":"m","messages":[{"role":"user","content":"my key is ` + secret + `, and PEM:\n-----BEGIN PRIVATE KEY-----\nABC"}]}`
	out := string(Redact([]byte(body)))
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
	out := Redact(body)
	if string(out) != string(body) {
		t.Errorf("Redact(clean) = %q, want unchanged %q", out, body)
	}
}

// A redacted JSON body must stay parseable-shaped: the placeholder substitutes
// inside the string without breaking the quotes around it.
func TestRedactKeepsJSONShape(t *testing.T) {
	body := `{"model":"m","key":"sk-` + strings.Repeat("aB3", 16) + `"}`
	out := string(Redact([]byte(body)))
	want := `{"model":"m","key":"` + RedactPlaceholder + `"}`
	if out != want {
		t.Errorf("Redact = %q, want %q", out, want)
	}
}
