package webauth

// keyfile_test.go — the management half of the forward key file: label
// comment pairing, atomic rewrite round-trip, generation, key ids, and the
// gate→log context carrier.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestKeyEntryRoundTripKeepsLabelsAndBareTokens(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api_keys")
	labeled := "# hand note\n# @label phone\n# @created 2026-10-09T15:04:05Z\nmp-key-aaa111\n\n# @label laptop\nmp-key-bbb222\nbare-key-no-meta\n"
	if err := os.WriteFile(path, []byte(labeled), 0o600); err != nil {
		t.Fatal(err)
	}
	entries := ReadKeyEntries(path)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 3: %+v", len(entries), entries)
	}
	if entries[0].Token != "mp-key-aaa111" || entries[0].Label != "phone" || entries[0].Created != "2026-10-09T15:04:05Z" {
		t.Errorf("labeled entry = %+v", entries[0])
	}
	// A hand-written comment is NOT metadata; the next bare line stays bare.
	if entries[0].Label == "hand note" {
		t.Error("free-form comment leaked into label")
	}
	if entries[1].Label != "laptop" || entries[1].Created != "" {
		t.Errorf("second entry = %+v, want label-only", entries[1])
	}
	if entries[2].Label != "" || entries[2].Token != "bare-key-no-meta" {
		t.Errorf("bare entry = %+v", entries[2])
	}

	// Rewrite must round-trip the same metadata and stay loadable by the GATE
	// (loadTokens: one token per line, comments ignored).
	if err := WriteKeyEntries(path, entries); err != nil {
		t.Fatal(err)
	}
	again := ReadKeyEntries(path)
	if len(again) != 3 || again[0].Label != "phone" || again[1].Label != "laptop" || again[2].Label != "" {
		t.Fatalf("rewrite round-trip drifted: %+v", again)
	}
	src := NewSource(path)
	if !src.Accept("mp-key-bbb222") || !src.Accept("bare-key-no-meta") {
		t.Error("gate no longer accepts rewritten tokens")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, want 0600", info.Mode().Perm())
	}
}

func TestReadKeyEntriesMissingFileIsEmpty(t *testing.T) {
	if got := ReadKeyEntries(filepath.Join(t.TempDir(), "absent")); got != nil {
		t.Errorf("missing file = %+v, want nil", got)
	}
	if got := ReadKeyEntries(""); got != nil {
		t.Errorf("empty path = %+v, want nil", got)
	}
}

func TestGenerateTokenShapeAndUniqueness(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 32; i++ {
		tok, err := GenerateToken()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(tok, "mp-key-") || len(tok) != len("mp-key-")+48 {
			t.Fatalf("token shape = %q", tok)
		}
		if seen[tok] {
			t.Fatalf("duplicate token generated: %q", tok[:12]+"…")
		}
		seen[tok] = true
	}
}

func TestKeyIDStableShortNonReversibleShape(t *testing.T) {
	a, b := KeyID("mp-key-x"), KeyID("mp-key-x")
	if a != b || len(a) != 8 {
		t.Fatalf("KeyID not stable/8-hex: %q vs %q", a, b)
	}
	if a == KeyID("mp-key-y") {
		t.Fatal("different tokens must yield different ids")
	}
	if strings.Contains(a, "mp-key") {
		t.Fatal("id must not embed the token")
	}
}

func TestResolveMatchesTheConfiguredSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	os.WriteFile(path, []byte("one\ntwo\n"), 0o600)
	src := NewSource(path)
	if got := Resolve(src, "two"); got != "two" {
		t.Errorf("Resolve(two) = %q", got)
	}
	if got := Resolve(src, "nope"); got != "" {
		t.Errorf("Resolve(nope) = %q, want empty", got)
	}
	if got := Resolve(nil, "two"); got != "" {
		t.Errorf("Resolve(nil source) = %q, want empty", got)
	}
}

func TestKeyIDContextRoundTrip(t *testing.T) {
	if got := KeyIDFrom(context.Background()); got != "" {
		t.Errorf("KeyIDFrom(empty) = %q", got)
	}
	ctx := WithKeyID(context.Background(), "abcd1234")
	if got := KeyIDFrom(ctx); got != "abcd1234" {
		t.Errorf("KeyIDFrom = %q", got)
	}
	// WriteKeyEntries stamps Created via the Now seam.
	old := Now
	Now = func() time.Time { return time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { Now = old })
	p := filepath.Join(t.TempDir(), "k2")
	if err := WriteKeyEntries(p, []KeyEntry{{Token: "t1", Label: "x", Created: Now().Format(time.RFC3339)}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "# @created 2026-10-09T12:00:00Z") {
		t.Errorf("created stamp missing:\n%s", data)
	}
}
