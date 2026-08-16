// Package guard owns the outbound request-body secret scan (DLP-lite): a small
// table of high-confidence secret patterns (private-key PEM headers, cloud/API
// tokens) matched against the raw client body before it is forwarded upstream.
//
// The table deliberately errs on the side of missing a leak ("宁漏勿滥"): every
// pattern requires an issuer-specific prefix AND a minimum payload length, so
// ordinary code content (variable names, short ids, docs) does not match. It is
// a safety net for obvious accidents, not a general DLP engine.
//
// Scan/Redact report pattern TYPE NAMES only. Matched secret bytes are never
// returned beyond the redacted body itself, so callers can log/count hits
// without any credential reaching logs, events, or test output.
package guard

import "regexp"

// RedactPlaceholder replaces every matched secret in the redacted body.
const RedactPlaceholder = "[REDACTED]"

// pattern is one named high-confidence secret matcher.
type pattern struct {
	name string
	re   *regexp.Regexp
}

// patterns is the closed secret-pattern table, ordered most-specific first:
// when matches overlap (e.g. an sk-ant- key also fits the generic sk- shape),
// the earlier pattern claims the byte span and later patterns neither
// double-report nor double-redact it.
var patterns = []pattern{
	{"anthropic_api_key", regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{20,}`)},
	{"openai_api_key", regexp.MustCompile(`\bsk-(?:proj-|svcacct-)?[A-Za-z0-9_-]{20,}`)},
	{"github_token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`)},
	{"github_fine_grained_pat", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}`)},
	{"aws_access_key_id", regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{"google_api_key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"pem_private_key", regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY(?: BLOCK)?-----`)},
}

// match is one claimed secret occurrence: which pattern and where.
type match struct {
	name       string
	start, end int
}

// findAll returns every non-overlapping secret match in body, scanning the
// pattern table in order and claiming byte spans so a more specific pattern
// wins over a looser one on the same bytes.
func findAll(body []byte) []match {
	var found []match
	for _, p := range patterns {
		for _, loc := range p.re.FindAllIndex(body, -1) {
			if overlaps(found, loc[0], loc[1]) {
				continue
			}
			found = append(found, match{name: p.name, start: loc[0], end: loc[1]})
		}
	}
	return found
}

// overlaps reports whether [start,end) intersects any already-claimed match.
func overlaps(found []match, start, end int) bool {
	for _, m := range found {
		if start < m.end && end > m.start {
			return true
		}
	}
	return false
}

// Scan returns the deduplicated type names of the secret patterns found in
// body, in pattern-table order (an sk-ant- key reports anthropic_api_key only,
// not also the looser openai_api_key shape). An empty result means the body is
// clean (as far as this high-confidence table can tell).
func Scan(body []byte) []string {
	hit := map[string]bool{}
	for _, m := range findAll(body) {
		hit[m.name] = true
	}
	var names []string
	for _, p := range patterns {
		if hit[p.name] {
			names = append(names, p.name)
		}
	}
	return names
}

// Redact returns body with every matched secret replaced by RedactPlaceholder.
// A clean body is returned unchanged. The replacement is a pure byte-substring
// substitution, so a match inside a JSON string stays valid JSON.
func Redact(body []byte) []byte {
	found := findAll(body)
	if len(found) == 0 {
		return body
	}
	// findAll claims spans in pattern order, not byte order — sort by offset so
	// the reconstruction walks the body left to right.
	for i := 1; i < len(found); i++ {
		for j := i; j > 0 && found[j-1].start > found[j].start; j-- {
			found[j-1], found[j] = found[j], found[j-1]
		}
	}
	out := make([]byte, 0, len(body))
	pos := 0
	for _, m := range found {
		out = append(out, body[pos:m.start]...)
		out = append(out, RedactPlaceholder...)
		pos = m.end
	}
	return append(out, body[pos:]...)
}
