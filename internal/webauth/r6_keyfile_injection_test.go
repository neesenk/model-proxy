package webauth

// r6_keyfile_injection_test.go — the write side of the key file can never
// emit a credential line from metadata: loadTokens treats every non-comment,
// non-blank line as a token, so a label or created stamp carrying CR/LF would
// land as its own credential (R6 F1).

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// r6InjectedKey is shaped like a forward key, so a metadata line that breaks
// onto its own line is indistinguishable from a real credential to the gate.
// Fixed test data — never a minted key.
const r6InjectedKey = "mp-key-0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

// r6CredentialLines counts the file's credential lines independently of the
// parser under test: every trimmed non-blank line that is not a comment.
func r6CredentialLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// TestR6WriteKeyEntriesNeutralizesLineBreaksInMetadata: CR and LF in a label
// or created stamp become spaces (trimmed), so the entry count survives the
// round trip, the credential-line count equals the entry count, and the gate
// never sees the injected string as a token.
func TestR6WriteKeyEntriesNeutralizesLineBreaksInMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys")
	if err := WriteKeyEntries(path, []KeyEntry{
		{Token: "tok-one", Label: "phone\n" + r6InjectedKey},
		{Token: "tok-two", Label: "laptop\r" + r6InjectedKey + "\r\n tail", Created: "2026-11-01T00:00:00Z\n" + r6InjectedKey},
		{Token: "tok-three", Label: "\n" + r6InjectedKey + "\n", Created: "\r\n"},
		{Token: "tok-four", Label: "手机 📱", Created: "2026-11-02T00:00:00Z"},
	}); err != nil {
		t.Fatal(err)
	}

	want := []KeyEntry{
		{Token: "tok-one", Label: "phone " + r6InjectedKey},
		{Token: "tok-two", Label: "laptop " + r6InjectedKey + "   tail", Created: "2026-11-01T00:00:00Z " + r6InjectedKey},
		{Token: "tok-three", Label: r6InjectedKey},
		{Token: "tok-four", Label: "手机 📱", Created: "2026-11-02T00:00:00Z"},
	}
	if again := ReadKeyEntries(path); !reflect.DeepEqual(again, want) {
		t.Errorf("entries after rewrite = %d (%v), want exactly %d — a metadata line became an entry",
			len(again), again, len(want))
	}

	// Canonical bytes: each metadata field stays on its own comment line and a
	// line-break-only created stamp ("\r\n") is dropped.
	wantFile := "# @label phone " + r6InjectedKey + "\n" +
		"tok-one\n" +
		"# @label laptop " + r6InjectedKey + "   tail\n" +
		"# @created 2026-11-01T00:00:00Z " + r6InjectedKey + "\n" +
		"tok-two\n" +
		"# @label " + r6InjectedKey + "\n" +
		"tok-three\n" +
		"# @label 手机 📱\n" +
		"# @created 2026-11-02T00:00:00Z\n" +
		"tok-four\n"
	if raw, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(raw) != wantFile {
		t.Errorf("file = %q, want %q", raw, wantFile)
	}

	wantTokens := []string{"tok-one", "tok-two", "tok-three", "tok-four"}
	if got := r6CredentialLines(t, path); !reflect.DeepEqual(got, wantTokens) {
		t.Errorf("credential lines = %q, want exactly the %d entry tokens %v", got, len(want), wantTokens)
	}
	if got := ReadTokens(path); !reflect.DeepEqual(got, wantTokens) {
		t.Errorf("ReadTokens = %q, want %v", got, wantTokens)
	}
	if got, err := loadTokens([]string{path}); err != nil || !reflect.DeepEqual(got, wantTokens) {
		t.Errorf("loadTokens = %q, %v, want %v", got, err, wantTokens)
	}

	gate := NewSource(path)
	if gate.Accept(r6InjectedKey) {
		t.Error("gate accepted the injected metadata line as a credential")
	}
	if !gate.Accept("tok-three") {
		t.Error("gate no longer accepts the entry whose label carried the injection")
	}
}

// TestR6WriteKeyEntriesKeepsMultilingualMetadataVerbatim: a label with spaces,
// Chinese and emoji round-trips byte-identically — the line-break backstop
// touches CR/LF only.
func TestR6WriteKeyEntriesKeepsMultilingualMetadataVerbatim(t *testing.T) {
	path := filepath.Join(t.TempDir(), "api_keys")
	want := []KeyEntry{
		{Token: "tok-one", Label: "手机 📱 平板"},
		{Token: "tok-two", Label: "laptop (work) — 笔记本", Created: "2026-11-02T00:00:00Z"},
	}
	if err := WriteKeyEntries(path, want); err != nil {
		t.Fatal(err)
	}
	if got := ReadKeyEntries(path); !reflect.DeepEqual(got, want) {
		t.Errorf("multilingual round trip = %+v, want %+v", got, want)
	}
	wantFile := "# @label 手机 📱 平板\ntok-one\n# @label laptop (work) — 笔记本\n# @created 2026-11-02T00:00:00Z\ntok-two\n"
	if raw, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(raw) != wantFile {
		t.Errorf("file = %q, want %q", raw, wantFile)
	}
}
