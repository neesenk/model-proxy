package guard

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
)

// FuzzScanSecretsAndPaths pins the scan pipeline against adversarial request
// bodies: arbitrary bytes must never panic, results must be deterministic,
// and the credential red line must hold — no report channel and no redacted
// body may ever contain the planted secret's material. A body that carries
// the secret in plaintext must be detected (known_secret) and redacted.
// The exploratory run doubles as the ReDoS posture check: a catastrophic
// pattern would show up as a fuzz timeout, not a failing assert.
func FuzzScanSecretsAndPaths(f *testing.F) {
	secret := syntheticSecret("fuzzpool-", 32)
	s, err := NewScanner(nil, Known(secret), nil)
	if err != nil {
		f.Fatalf("NewScanner: %v", err)
	}
	seeds := []string{
		"just ordinary prose about distributed systems and Go",
		"token: " + secret,
		`{"messages":[{"role":"user","content":"cat ~/.ssh/id_rsa"}]}`,
		"sk-ant-api03-" + strings.Repeat("qW7", 30),
		"blob " + secret + " end",
		`{"nested":{"deep":{"v":"` + secret + `"}}}`,
		strings.Repeat("no secrets here, only prose. ", 200),
		"\x00\xff binary \x01 junk",
		"PATH=/etc/passwd cat id_rsa .aws/credentials",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		secrets, strong, weak := s.ScanSecretsAndPaths(body)
		secrets2, strong2, weak2 := s.ScanSecretsAndPaths(body)
		if !reflect.DeepEqual(secrets, secrets2) || !reflect.DeepEqual(strong, strong2) || !reflect.DeepEqual(weak, weak2) {
			t.Fatal("ScanSecretsAndPaths not deterministic")
		}
		names, redacted := s.ScanAndRedact(body)
		names2, redacted2 := s.ScanAndRedact(body)
		if !reflect.DeepEqual(names, names2) || !bytes.Equal(redacted, redacted2) {
			t.Fatal("ScanAndRedact not deterministic")
		}
		for _, channel := range [][]string{secrets, strong, weak, names} {
			for _, name := range channel {
				if strings.Contains(name, secret) {
					t.Fatalf("report channel leaks secret material: %q", name)
				}
			}
		}
		if len(names) == 0 && !bytes.Equal(redacted, body) {
			t.Fatal("no matches yet redaction mutated the body")
		}
		if bytes.Contains(body, []byte(secret)) {
			if got := s.ScanKnown(body); !containsStr(got, knownSecret) {
				t.Fatalf("planted secret present but ScanKnown = %v, want %s", got, knownSecret)
			}
			if bytes.Contains(redacted, []byte(secret)) {
				t.Fatal("redacted body still contains the planted secret")
			}
		}
	})
}

func containsStr(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
