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
// loopback-trust behavior). A configured file that has gone MISSING fails
// closed: the set becomes empty (accept nothing) rather than open. Other read
// errors are treated as transient: the last successfully loaded set keeps
// serving, and with no prior cache the caller rejects.
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

	mu        sync.Mutex
	cached    []string
	haveCache bool
	_loaded   time.Time
	// _errorSince starts a non-sliding stale-cache grace window after the
	// first transient reload failure. It is deliberately separate from
	// _loaded: refreshing the success timestamp on every failure would keep
	// revoked tokens valid forever while the file remains unreadable.
	_errorSince time.Time
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

// tokens returns the cached token set, reloading when the TTL expired. A
// MISSING file fails closed: the set becomes empty (accept nothing). Any other
// read error is treated as transient: the last successfully loaded set keeps
// serving for a fixed, non-sliding cacheTTL grace measured from the first
// failure, so a blip does not invalidate every token — including the forward
// surface's 401s. With no prior cache, or after the grace, the caller rejects.
func (s *Source) tokens() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.haveCache && now.Sub(s._loaded) < cacheTTL {
		return s.cached, nil
	}
	toks, err := loadTokens(s.paths)
	if err != nil {
		if s.haveCache {
			if s._errorSince.IsZero() {
				s._errorSince = now
			}
			if now.Sub(s._errorSince) < cacheTTL {
				return s.cached, nil
			}
		}
		return nil, err
	}
	s.cached = toks
	s.haveCache = true
	s._loaded = now
	s._errorSince = time.Time{}
	return s.cached, nil
}

// loadTokens reads every file and returns the parsed token set. A missing file
// (os.ErrNotExist) yields an EMPTY set with no error — fail closed — because a
// rotated-away file must never keep the surface open. Any other read failure is
// returned to the caller as a transient error.
func loadTokens(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, nil
			}
			return nil, err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
	}
	return out, nil
}

// Accept reports whether the presented token matches the configured set.
// Constant-time over the whole set: every candidate is compared and the match
// bits OR-ed, so timing does not reveal which line hit. An empty presented
// token never matches; a configured-but-empty set accepts nothing.
func (s *Source) Accept(presented string) bool {
	if !s.Enabled() || presented == "" {
		return false
	}
	toks, err := s.tokens()
	if err != nil {
		// Cold cache plus a transient read error: no trustworthy set exists,
		// so reject rather than guess.
		return false
	}
	match := 0
	for _, tok := range toks {
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
