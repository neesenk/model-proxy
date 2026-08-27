// Package webauth owns bearer-token authentication for the proxy's two
// serving surfaces, unlocked by the S2 optional-auth layer:
//
//   - admin surface (/ui, /api/*, /metrics, /debug/*): web.auth.admin_token_file
//   - forward surface (/v1/*, anthropic /messages, /v1/models): web.auth.api_keys_file
//
// Both files are line-oriented token lists (one token per line; blank lines and
// #-comments ignored), loaded with a short TTL cache so a stat-per-request cost
// is bounded and file edits (rotation, key revocation) take effect without a
// daemon restart. Comparison is constant-time across the whole token set.
//
// The zero-value/disabled Source (no files configured) accepts nothing and
// reports Enabled()==false — callers treat that as "auth off" (the historical
// loopback-trust behavior). A configured file that cannot be READ fails closed:
// its cached set becomes empty (accept nothing) rather than open; a MISSING
// file is a configuration error surface at validate time, not here.
package webauth

import (
	"crypto/subtle"
	"os"
	"strings"
	"sync"
	"time"
)

// cacheTTL bounds how long a loaded token set stays authoritative. Short
// enough that revocation feels immediate, long enough to keep the forward hot
// path off the filesystem for every request.
const cacheTTL = 10 * time.Second

// Source is a reloadable token set backed by one or more token files.
type Source struct {
	paths []string

	mu      sync.Mutex
	cached  []string
	_loaded time.Time
}

// NewSource builds a Source over the given token files. Zero paths (or all
// empty) yields the disabled Source.
func NewSource(paths ...string) *Source {
	var kept []string
	for _, p := range paths {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	return &Source{paths: kept}
}

// Enabled reports whether any token file is configured.
func (s *Source) Enabled() bool {
	return s != nil && len(s.paths) > 0
}

// tokens returns the cached token set, reloading when the TTL expired.
func (s *Source) tokens() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s._loaded) < cacheTTL && s._loaded.After(time.Time{}.Add(time.Second)) {
		return s.cached
	}
	s.cached = loadTokens(s.paths)
	s._loaded = time.Now()
	return s.cached
}

// loadTokens reads every file and returns the parsed token set. Read failures
// yield an empty contribution (fail closed) — a rotated-away file must never
// open the surface.
func loadTokens(paths []string) []string {
	var out []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
	}
	return out
}

// Accept reports whether the presented token matches the configured set.
// Constant-time over the whole set: every candidate is compared and the match
// bits OR-ed, so timing does not reveal which line hit. An empty presented
// token never matches; a configured-but-empty set accepts nothing.
func (s *Source) Accept(presented string) bool {
	if !s.Enabled() || presented == "" {
		return false
	}
	match := 0
	for _, tok := range s.tokens() {
		match |= subtle.ConstantTimeCompare([]byte(presented), []byte(tok))
	}
	return match == 1
}

// BearerFromRequest extracts the bearer token from an incoming request:
// Authorization: Bearer <token> (OpenAI/Claude convention) or the
// x-api-key header (Anthropic convention). Returns "" when neither carries a
// token.
func BearerFromRequest(header func(string) string) string {
	if h := header("Authorization"); h != "" {
		parts := strings.SplitN(h, " ", 2)
		if len(parts) == 2 && strings.EqualFold(parts[0], "Bearer") {
			return strings.TrimSpace(parts[1])
		}
	}
	if k := header("x-api-key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}
