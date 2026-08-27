package webauth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTokenFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDisabledSourceAcceptsNothing(t *testing.T) {
	var nilSource *Source
	if nilSource.Enabled() || nilSource.Accept("anything") {
		t.Fatal("nil source must be disabled and accept nothing")
	}
	if NewSource().Enabled() || NewSource("").Enabled() {
		t.Fatal("empty-path source must be disabled")
	}
	if NewSource().Accept("anything") {
		t.Fatal("disabled source must accept nothing")
	}
}

func TestAcceptParsesCommentsAndBlankLines(t *testing.T) {
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "keys", "# team keys\nsk-one\n\nsk-two\n   \n")
	s := NewSource(path)

	if !s.Accept("sk-one") || !s.Accept("sk-two") {
		t.Fatal("configured tokens must be accepted")
	}
	for _, bad := range []string{"", "sk-three", "# team keys", "sk-on", "SK-ONE"} {
		if s.Accept(bad) {
			t.Fatalf("Accept(%q) must be false", bad)
		}
	}
}

func TestUnreadableFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	// Path exists as a directory → read fails → empty set → accept nothing.
	s := NewSource(filepath.Join(dir, "missing-or-dir"))
	if s.Enabled() != true {
		t.Fatal("a configured path reports enabled even when unreadable")
	}
	if s.Accept("sk-anything") {
		t.Fatal("unreadable token file must fail closed, not open")
	}
}

func TestRevocationTakesEffectAfterTTL(t *testing.T) {
	dir := t.TempDir()
	path := writeTokenFile(t, dir, "keys", "sk-old\n")
	s := NewSource(path)
	if !s.Accept("sk-old") {
		t.Fatal("initial token must be accepted")
	}
	// Revoke: replace the file, then force cache expiry.
	writeTokenFile(t, dir, "keys", "sk-new\n")
	s.mu.Lock()
	s._loaded = time.Now().Add(-cacheTTL - time.Second)
	s.mu.Unlock()
	if s.Accept("sk-old") {
		t.Fatal("revoked token accepted after cache expiry")
	}
	if !s.Accept("sk-new") {
		t.Fatal("new token rejected after cache expiry")
	}
}

func TestBearerFromRequest(t *testing.T) {
	mk := func(auth, apiKey string) func(string) string {
		return func(name string) string {
			switch name {
			case "Authorization":
				return auth
			case "x-api-key":
				return apiKey
			}
			return ""
		}
	}
	cases := []struct {
		auth, xapi, want string
	}{
		{"Bearer sk-a", "", "sk-a"},
		{"bearer sk-a", "", "sk-a"},   // scheme is case-insensitive
		{"Bearer  sk-a ", "", "sk-a"}, // token whitespace trimmed
		{"Basic Zm9v", "", ""},
		{"NotBearer sk-a", "", ""},
		{"", "sk-b", "sk-b"}, // anthropic x-api-key fallback
		{"Bearer sk-a", "sk-b", "sk-a"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := BearerFromRequest(mk(tc.auth, tc.xapi)); got != tc.want {
			t.Errorf("BearerFromRequest(auth=%q, x-api-key=%q) = %q, want %q", tc.auth, tc.xapi, got, tc.want)
		}
	}
}
