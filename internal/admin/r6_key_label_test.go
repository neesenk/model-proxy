package admin

// r6_key_label_test.go — forward-key labels enter through the admin service
// and land in the line-oriented key file, where the gate parses every
// non-comment line as a credential. The entry points (create, relabel) must
// refuse a label that cannot ride one comment line (a control character —
// CR/LF would inject a credential line) or would flood listings (beyond the
// label limit shared with account labels) BEFORE the file is touched (R6 F1).

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	"model-proxy/internal/webauth"
)

// r6InjectedKey is shaped like a forward key, so a label line that breaks
// onto its own line would be a working credential. Fixed test data.
const r6InjectedKey = "mp-key-0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f0f"

const r6ControlCharMsg = "label must not contain control characters"

// r6SeededKeyFile writes a labeled and a bare seed key; hand-written metadata
// must survive every rejected mutation.
func r6SeededKeyFile(t *testing.T) (string, []byte) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "api_keys")
	original := []byte("# @label seed\n# @created 2026-11-01T00:00:00Z\nseed-key-0\nbare-key-1\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, original
}

// r6RequireSeedsOnly asserts the file bytes are unchanged and the gate still
// reads exactly the two seeds — never the injected string.
func r6RequireSeedsOnly(t *testing.T, path string, original []byte) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		// Count only: a minted key must not reach the test output.
		t.Errorf("rejected label still rewrote the key file: gate now reads %d credentials", len(webauth.ReadTokens(path)))
	}
	gate := webauth.NewSource(path)
	for _, seed := range []string{"seed-key-0", "bare-key-1"} {
		if !gate.Accept(seed) {
			t.Errorf("seed credential %q no longer accepted", seed)
		}
	}
	if gate.Accept(r6InjectedKey) {
		t.Errorf("gate accepted the injected label line as a credential")
	}
}

// r6LabelView renders entries without their tokens: "label|created" per
// entry, in file order.
func r6LabelView(entries []webauth.KeyEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Label+"|"+e.Created)
	}
	return out
}

// TestR6APIKeyLabelsRejectUnsafeInput: every unsafe label is a 400-shaped
// error with a stable message on BOTH entry points, and the key file (hence
// the gate's credential set) is left exactly as it was.
func TestR6APIKeyLabelsRejectUnsafeInput(t *testing.T) {
	const tooLong = "label is longer than 64 characters"
	bad := []struct{ name, label, wantErr string }{
		{"LF injects a credential line", "phone\n" + r6InjectedKey, r6ControlCharMsg},
		{"CRLF injects a credential line", "phone\r\n" + r6InjectedKey, r6ControlCharMsg},
		{"bare CR injects a credential line", "phone\r" + r6InjectedKey, r6ControlCharMsg},
		{"NUL", "ph\x00one", r6ControlCharMsg},
		{"tab", "ph\tone", r6ControlCharMsg},
		{"ESC", "ph\x1b[31mone", r6ControlCharMsg},
		{"DEL", "phone\x7f", r6ControlCharMsg},
		{"C1 NEL", "phone\u0085" + r6InjectedKey, r6ControlCharMsg},
		{"65 ASCII runes", strings.Repeat("a", 65), tooLong},
		{"65 CJK runes", strings.Repeat("字", 65), tooLong},
	}
	entryPoints := []struct {
		name string
		call func(s *Service, label string) error
	}{
		{"create", func(s *Service, label string) error {
			_, err := s.CreateAPIKey(label)
			return err
		}},
		{"relabel", func(s *Service, label string) error {
			return s.SetAPIKeyLabel(webauth.KeyID("seed-key-0"), label)
		}},
	}
	for _, entry := range entryPoints {
		for _, tc := range bad {
			t.Run(entry.name+"/"+tc.name, func(t *testing.T) {
				path, original := r6SeededKeyFile(t)
				err := entry.call(keysService(t, path), tc.label)
				var httpErr *appapi.HTTPError
				if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest || httpErr.Message != tc.wantErr {
					t.Errorf("%s(%q) = %v, want 400 %q", entry.name, tc.label, err, tc.wantErr)
				}
				r6RequireSeedsOnly(t, path, original)
			})
		}
	}
}

// TestR6APIKeyLabelLimitIsSharedWithAccounts: the limit is the account-label
// limit (accounts.MaxLabelLen = 64) and counts runes, not bytes — 64 CJK
// characters pass on both entry points.
func TestR6APIKeyLabelLimitIsSharedWithAccounts(t *testing.T) {
	if accounts.MaxLabelLen != 64 {
		t.Fatalf("accounts.MaxLabelLen = %d, want the documented 64", accounts.MaxLabelLen)
	}
	path, _ := r6SeededKeyFile(t)
	s := keysService(t, path)
	limit := strings.Repeat("字", accounts.MaxLabelLen)

	created, err := s.CreateAPIKey(limit)
	if err != nil {
		t.Fatalf("64-rune label refused on create: %v", err)
	}
	if created.Label != limit {
		t.Errorf("created label = %q, want the submitted %q", created.Label, limit)
	}
	if err := s.SetAPIKeyLabel(webauth.KeyID("seed-key-0"), limit); err != nil {
		t.Fatalf("64-rune label refused on relabel: %v", err)
	}
	entries := webauth.ReadKeyEntries(path)
	if len(entries) != 3 || entries[0].Label != limit || entries[1].Token != "bare-key-1" ||
		entries[2].Token != created.Token || entries[2].Label != limit {
		t.Errorf("file after limit-sized labels = %v, want both labels stored verbatim", r6LabelView(entries))
	}
	if !webauth.NewSource(path).Accept(created.Token) {
		t.Error("gate does not accept the key created with a limit-sized label")
	}
}

// TestR6APIKeyLabelTrimsBeforeValidating: surrounding whitespace (line breaks
// included) is trimmed first, so only control characters INSIDE the label are
// refused — a trailing newline cannot inject anything, and padding does not
// count toward the limit.
func TestR6APIKeyLabelTrimsBeforeValidating(t *testing.T) {
	path, _ := r6SeededKeyFile(t)
	s := keysService(t, path)

	created, err := s.CreateAPIKey("  phone\n")
	if err != nil {
		t.Fatalf("trailing newline refused on create: %v", err)
	}
	if created.Label != "phone" {
		t.Errorf("created label = %q, want the trimmed %q", created.Label, "phone")
	}
	// A line-break-only label is the unlabeled form.
	unlabeled, err := s.CreateAPIKey("\n")
	if err != nil {
		t.Fatalf("line-break-only label refused on create: %v", err)
	}
	if unlabeled.Label != "" {
		t.Errorf("line-break-only label = %q, want empty", unlabeled.Label)
	}
	padded := " \t" + strings.Repeat("a", accounts.MaxLabelLen) + " \n"
	seedID := webauth.KeyID("seed-key-0")
	if err := s.SetAPIKeyLabel(seedID, padded); err != nil {
		t.Fatalf("padded limit-sized label refused on relabel: %v", err)
	}
	entries := webauth.ReadKeyEntries(path)
	if len(entries) != 4 || entries[0].Label != strings.Repeat("a", accounts.MaxLabelLen) ||
		entries[0].Created != "2026-11-01T00:00:00Z" || entries[2].Token != created.Token ||
		entries[2].Label != "phone" || entries[3].Token != unlabeled.Token || entries[3].Label != "" {
		t.Errorf("file after trimmed labels = %v", r6LabelView(entries))
	}

	// The label is validated before the id lookup: an unknown id carrying an
	// unsafe label is a 400, never a 404 that reads as "retry with another id".
	err = s.SetAPIKeyLabel("deadbeef", "x\n"+r6InjectedKey)
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusBadRequest || httpErr.Message != r6ControlCharMsg {
		t.Errorf("unknown id + unsafe label = %v, want 400 %q", err, r6ControlCharMsg)
	}
}

// TestR6APIKeyMultilingualLabelsUnchanged: spaces, Chinese and emoji keep
// working end to end — create stores and echoes the label, the listing
// returns it, relabel swaps it, and the gate accepts the minted key.
func TestR6APIKeyMultilingualLabelsUnchanged(t *testing.T) {
	path, _ := r6SeededKeyFile(t)
	s := keysService(t, path)

	created, err := s.CreateAPIKey("手机 📱 平板")
	if err != nil {
		t.Fatalf("multilingual label refused on create: %v", err)
	}
	if created.Label != "手机 📱 平板" {
		t.Errorf("created label = %q, want %q", created.Label, "手机 📱 平板")
	}
	const renamed = "laptop (work) — 笔记本 😀"
	if err := s.SetAPIKeyLabel(webauth.KeyID("bare-key-1"), renamed); err != nil {
		t.Fatalf("multilingual label refused on relabel: %v", err)
	}
	entries := webauth.ReadKeyEntries(path)
	if len(entries) != 3 || entries[0].Label != "seed" || entries[1].Label != renamed ||
		entries[2].Label != "手机 📱 平板" || entries[2].Token != created.Token {
		t.Errorf("file after multilingual labels = %v", r6LabelView(entries))
	}
	listed, err := s.APIKeys()
	if err != nil || len(listed.Keys) != 3 || listed.Keys[1].Label != renamed || listed.Keys[2].Label != "手机 📱 平板" {
		t.Errorf("listing = %+v, %v", listed.Keys, err)
	}
	gate := webauth.NewSource(path)
	if !gate.Accept(created.Token) || !gate.Accept("bare-key-1") {
		t.Error("gate stopped accepting a key after multilingual labeling")
	}
}
