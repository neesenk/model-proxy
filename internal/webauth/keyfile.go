package webauth

// keyfile.go — management (not gating) for the forward-surface key file:
// labeled entries, atomic rewrite, token generation, and stable key ids.
//
// The on-disk format stays the documented line format (one token per line,
// blank lines and #-comments ignored) so hand edits and the serving gate keep
// working unchanged; a key's metadata rides comment lines immediately above
// the token:
//
//	# @label phone
//	# @created 2026-10-09T15:04:05Z
//	mp-key-0123abcd…
//
// Bare token lines (the historical shape) are entries without metadata. Key
// identity for logs/aggregation is sha256(token)[:8] — never the token, which
// must not appear in request logs, stats, or API responses (the full token is
// returned exactly once, at creation).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// KeyEntry is one forward key with its optional metadata.
type KeyEntry struct {
	Token   string
	Label   string
	Created string // RFC3339, "" when absent
}

// KeyID is the stable, non-secret identity of one token: first 8 hex chars of
// its sha256. Safe for logs, API responses and aggregation keys.
func KeyID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])[:8]
}

// GenerateToken mints a new forward key: "mp-key-" + 24 random bytes hex.
// The prefix marks leaked values as proxy forward keys during incident triage.
func GenerateToken() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "mp-key-" + hex.EncodeToString(buf), nil
}

// labelLine / createdLine are the metadata comment conventions. They must
// never collide with free-form user comments in a way that silently attaches
// metadata: only lines starting exactly with these markers parse as metadata.
const (
	labelMarker   = "# @label "
	createdMarker = "# @created "
)

// ReadKeyEntries parses one key file into labeled entries. A missing file is
// an empty list (same fail-closed convention as the gate); metadata comment
// lines attach to the next token line, everything else is ignored verbatim.
func ReadKeyEntries(path string) []KeyEntry {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []KeyEntry
	var pendingLabel, pendingCreated string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "":
			// blank: metadata pairing does not survive blank lines
			pendingLabel, pendingCreated = "", ""
		case strings.HasPrefix(line, labelMarker):
			pendingLabel = strings.TrimSpace(strings.TrimPrefix(line, labelMarker))
		case strings.HasPrefix(line, createdMarker):
			pendingCreated = strings.TrimSpace(strings.TrimPrefix(line, createdMarker))
		case strings.HasPrefix(line, "#"):
			// unrelated comment: no pairing effect
		default:
			out = append(out, KeyEntry{Token: line, Label: pendingLabel, Created: pendingCreated})
			pendingLabel, pendingCreated = "", ""
		}
	}
	return out
}

// WriteKeyEntries atomically rewrites the key file (0600, same dir) in the
// canonical format. Parent directories are created 0700.
func WriteKeyEntries(path string, entries []KeyEntry) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("key file path is empty")
	}
	var b strings.Builder
	for _, e := range entries {
		if e.Label != "" {
			fmt.Fprintf(&b, "%s%s\n", labelMarker, e.Label)
		}
		if e.Created != "" {
			fmt.Fprintf(&b, "%s%s\n", createdMarker, e.Created)
		}
		fmt.Fprintf(&b, "%s\n", e.Token)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Resolve returns the configured token that the presented credential matches
// ("" when none). The comparison walks the whole set exactly like Accept, so
// which-key attribution leaks no more timing than the gate itself.
func Resolve(s *Source, presented string) string {
	if s == nil || presented == "" {
		return ""
	}
	toks, err := s.tokens()
	if err != nil {
		return ""
	}
	for _, tok := range toks {
		if subtleEqual(tok, presented) {
			return tok
		}
	}
	return ""
}

// Now is the clock seam for Created stamps (tests pin it).
var Now = time.Now

type keyIDCtx struct{}

// WithKeyID carries the resolved forward-key id on the request context from
// the auth gate (app transport) to the logging path (forward pipeline); the
// raw token never travels beyond the gate.
func WithKeyID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, keyIDCtx{}, id)
}

// KeyIDFrom extracts the gate-resolved forward-key id, "" when the request
// was unauthenticated (loopback deployments) or arrived on another surface.
func KeyIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(keyIDCtx{}).(string)
	return id
}

// subtleEqual is the constant-time comparison Accept uses (aliased so the
// gate and Resolve share one implementation).
func subtleEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
