package guard

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
)

// minSecretLen is the minimum length for a known secret to enter the scan
// set: shorter values are too likely to collide with ordinary text.
const minSecretLen = 12

// maxEncodedSpan bounds the token span expanded around an encoded-literal
// hit, so a pathological run of token bytes cannot make decoding quadratic.
const maxEncodedSpan = 8 << 10

// Known-secret report names. The secret value itself is never returned.
const (
	knownSecret        = "known_secret"
	knownSecretEncoded = "known_secret_encoded"
)

// CustomPattern is a user-supplied secret matcher (config guard.extra_patterns).
// Literal, when non-empty, must be a substring of every possible RE match and
// is used as a prefilter (same invariant as the embedded table). Custom
// patterns match plaintext only — they do not participate in the
// encoded-variant channel.
type CustomPattern struct {
	Name    string
	RE      *regexp.Regexp
	Literal []byte
}

// customRule is a validated CustomPattern.
type customRule struct {
	name    string
	re      *regexp.Regexp
	literal []byte
}

// knownSecretSet holds one known secret and its precomputed encoded variants
// (base64 std/raw/url forms, lowercase hex, url.QueryEscape) together with
// their needle ids in the Scanner's prefilter automaton. Values live in
// memory only and are never logged or serialized.
type knownSecretSet struct {
	raw        []byte
	rawID      int
	encoded    [][]byte
	encodedIDs []int
}

// encodedProbe is one precomputed encoded form of an embedded rule's literal:
// when the variant bytes appear in a body, the surrounding token span is
// decoded and the owning rule's regex is run on the decoded text.
type encodedProbe struct {
	ruleIdx int
	variant []byte
	hexForm bool // hex variant; false = base64 variant
}

// needleKind classifies the prefilter needles in the automaton.
type needleKind uint8

const (
	needleRuleLiteral needleKind = iota
	needleProbe
	needleSecretRaw
	needleSecretEncoded
)

// needleRef maps an automaton needle id back to its owner.
type needleRef struct {
	kind needleKind
	idx  int // rule index, probe index, or secret index
	vidx int // variant index within the secret (needleSecretEncoded only)
}

// Scanner is an immutable per-generation secret scanner: the embedded rule
// table (always present) plus user custom patterns, known-secret exact values
// and extra sensitive paths. Build once (on reload), use read-only
// concurrently.
type Scanner struct {
	rules      []rule
	probes     []encodedProbe
	custom     []customRule
	secrets    []knownSecretSet
	extraPaths [][]byte
	ac         *acMatcher
	refs       []needleRef
}

// Options selects the optional Scanner channels. The zero value disables
// them; NewScanner enables everything (the historical behavior).
type Options struct {
	// Decode enables the encoded-form channels: the encoded-literal probes of
	// the embedded rule table and the encoded variants (base64/hex/url) of
	// known secrets. Plaintext matching is unaffected by this switch.
	Decode bool
}

// NewScanner compiles custom patterns, known secrets and extra paths into an
// immutable Scanner on top of the embedded rule table. The embedded table is
// always active; the returned Scanner is never nil when err is nil.
func NewScanner(custom []CustomPattern, secrets, extraPaths []string) (*Scanner, error) {
	return NewScannerWithOptions(custom, secrets, extraPaths, Options{Decode: true})
}

// NewScannerWithOptions is NewScanner with explicit channel switches
// (config guard.decode maps to Options.Decode).
func NewScannerWithOptions(custom []CustomPattern, secrets, extraPaths []string, opts Options) (*Scanner, error) {
	s := &Scanner{rules: embeddedRules}
	if opts.Decode {
		s.probes = embeddedProbes
	}
	seenNames := map[string]bool{}
	for _, c := range custom {
		if c.Name == "" {
			return nil, fmt.Errorf("guard: custom pattern with empty name")
		}
		if c.RE == nil {
			return nil, fmt.Errorf("guard: custom pattern %q has nil regexp", c.Name)
		}
		if seenNames[c.Name] {
			return nil, fmt.Errorf("guard: duplicate custom pattern name %q", c.Name)
		}
		seenNames[c.Name] = true
		s.custom = append(s.custom, customRule{name: c.Name, re: c.RE, literal: c.Literal})
	}
	seenSecrets := map[string]bool{}
	for _, sec := range secrets {
		if len(sec) < minSecretLen || seenSecrets[sec] {
			continue
		}
		seenSecrets[sec] = true
		s.secrets = append(s.secrets, buildSecretSet(sec, opts.Decode))
	}
	seenPaths := map[string]bool{}
	for _, p := range extraPaths {
		p = strings.TrimSpace(p)
		if p == "" || seenPaths[p] {
			continue
		}
		seenPaths[p] = true
		s.extraPaths = append(s.extraPaths, []byte(p))
	}
	s.compilePrefilter()
	return s, nil
}

// compilePrefilter builds the phase-1 automaton over every literal the scan
// pipeline can match on: rule prefilter literals, encoded-channel probe
// variants, and known-secret variants.
func (s *Scanner) compilePrefilter() {
	b := newACBuilder()
	addRef := func(kind needleKind, idx, vidx int, needle []byte) int {
		id := len(s.refs)
		s.refs = append(s.refs, needleRef{kind: kind, idx: idx, vidx: vidx})
		b.add(needle, id)
		return id
	}
	for i, r := range s.rules {
		for _, lit := range r.literals {
			addRef(needleRuleLiteral, i, -1, lit)
		}
	}
	for i, p := range s.probes {
		addRef(needleProbe, i, -1, p.variant)
	}
	for si := range s.secrets {
		sec := &s.secrets[si]
		sec.rawID = addRef(needleSecretRaw, si, -1, sec.raw)
		sec.encodedIDs = make([]int, len(sec.encoded))
		for vi, v := range sec.encoded {
			sec.encodedIDs[vi] = addRef(needleSecretEncoded, si, vi, v)
		}
	}
	s.ac = b.compile()
}

// buildSecretSet precomputes the match variants of one known secret: the raw
// value plus base64 (std/raw/url padded+raw), lowercase hex and
// url.QueryEscape forms. With decode off only the raw value is matched.
func buildSecretSet(secret string, decode bool) knownSecretSet {
	raw := []byte(secret)
	set := knownSecretSet{raw: raw}
	if !decode {
		return set
	}
	seen := map[string]bool{secret: true}
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			set.encoded = append(set.encoded, []byte(v))
		}
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.RawStdEncoding,
		base64.URLEncoding, base64.RawURLEncoding,
	} {
		add(enc.EncodeToString(raw))
	}
	add(hex.EncodeToString(raw))
	add(url.QueryEscape(secret))
	return set
}

// b64Interior returns the characters of enc(pad+lit) that are fully determined
// by lit when lit sits at the given mod-3 byte alignment inside a larger
// base64 stream — i.e. a substring guaranteed to appear in the stream's
// encoding. "" when the determined slice is too short to be a useful
// prefilter.
func b64Interior(enc *base64.Encoding, lit []byte, align int) string {
	padded := make([]byte, 0, align+len(lit))
	padded = append(padded, make([]byte, align)...)
	padded = append(padded, lit...)
	full := enc.EncodeToString(padded)
	start := (8*align + 5) / 6        // ceil(8*align/6)
	end := (8*align + 8*len(lit)) / 6 // floor(8*(align+len(lit))/6)
	if end-start < 4 {
		return ""
	}
	return full[start:end]
}

// buildProbes precomputes the encoded-channel probes for the embedded rule
// table: for every rule literal, the base64 interior variants (standard and
// URL alphabets × three byte alignments) and the lowercase hex form.
func buildProbes(rules []rule) []encodedProbe {
	var probes []encodedProbe
	seen := map[string]bool{}
	for i, r := range rules {
		for _, lit := range r.literals {
			for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
				for align := 0; align < 3; align++ {
					v := b64Interior(enc, lit, align)
					if v != "" && !seen[v] {
						seen[v] = true
						probes = append(probes, encodedProbe{ruleIdx: i, variant: []byte(v)})
					}
				}
			}
			h := hex.EncodeToString(lit)
			if !seen[h] {
				seen[h] = true
				probes = append(probes, encodedProbe{ruleIdx: i, variant: []byte(h), hexForm: true})
			}
		}
	}
	return probes
}

// embeddedProbes is the encoded-channel probe set for the embedded table,
// shared read-only by every Scanner.
var embeddedProbes = buildProbes(embeddedRules)

// match is one claimed secret occurrence: which rule and where.
type match struct {
	name       string
	start, end int
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

// isB64TokenByte reports whether c can be part of a base64/base64url token
// (including padding).
func isB64TokenByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '+' || c == '/' || c == '=' || c == '_' || c == '-'
}

// isHexTokenByte reports whether c is a hex digit.
func isHexTokenByte(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// expandSpan grows [start,end) left and right while bytes satisfy tok, capped
// at maxEncodedSpan total.
func expandSpan(body []byte, start, end int, tok func(byte) bool) (int, int) {
	for start > 0 && end-start < maxEncodedSpan && tok(body[start-1]) {
		start--
	}
	for end < len(body) && end-start < maxEncodedSpan && tok(body[end]) {
		end++
	}
	return start, end
}

// decodeB64Span decodes a base64/base64url token span, tolerating either
// alphabet and optional padding.
func decodeB64Span(span []byte) ([]byte, bool) {
	s := strings.TrimRight(string(span), "=")
	if s == "" || strings.Contains(s, "=") {
		return nil, false
	}
	encs := []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding}
	if strings.ContainsAny(s, "-_") {
		encs[0], encs[1] = encs[1], encs[0]
	}
	for _, enc := range encs {
		if out, err := enc.DecodeString(s); err == nil {
			return out, true
		}
	}
	return nil, false
}

// decodeHexSpan decodes an even-length hex token span.
func decodeHexSpan(span []byte) ([]byte, bool) {
	if len(span) == 0 || len(span)%2 != 0 {
		return nil, false
	}
	out, err := hex.DecodeString(string(span))
	if err != nil {
		return nil, false
	}
	return out, true
}

// findAll returns every non-overlapping secret match in body. Phase 1 is a
// single automaton pass over all literals; phase 2 claims spans in priority
// order: known secrets (plaintext then encoded variants), the embedded rule
// table in file order (plaintext then the encoded channel), then custom
// patterns in declaration order.
func (s *Scanner) findAll(body []byte) []match {
	// Phase 1: literal prefilter, one pass for all needles.
	ruleHit := make([]bool, len(s.rules))
	var probeHits map[int][]int  // probe index → occurrence end offsets
	var secretHits map[int][]int // needle id → occurrence end offsets
	s.ac.search(body, func(id, end int) {
		ref := s.refs[id]
		switch ref.kind {
		case needleRuleLiteral:
			ruleHit[ref.idx] = true
		case needleProbe:
			if probeHits == nil {
				probeHits = map[int][]int{}
			}
			probeHits[ref.idx] = append(probeHits[ref.idx], end)
		case needleSecretRaw, needleSecretEncoded:
			if secretHits == nil {
				secretHits = map[int][]int{}
			}
			secretHits[id] = append(secretHits[id], end)
		}
	})

	// Phase 2: precise matching / span claiming.
	var found []match

	// 1. Known secrets: exact values first, then their encoded variants.
	claimOccurrences := func(needle []byte, ends []int, name string) {
		for _, end := range ends {
			start := end - len(needle)
			if !overlaps(found, start, end) {
				found = append(found, match{name, start, end})
			}
		}
	}
	for si := range s.secrets {
		sec := &s.secrets[si]
		claimOccurrences(sec.raw, secretHits[sec.rawID], knownSecret)
	}
	for si := range s.secrets {
		sec := &s.secrets[si]
		for vi, v := range sec.encoded {
			claimOccurrences(v, secretHits[sec.encodedIDs[vi]], knownSecretEncoded)
		}
	}

	// 2. Embedded rule table, plaintext.
	for i, r := range s.rules {
		if !ruleHit[i] {
			continue
		}
		for _, span := range r.findIn(body) {
			if !overlaps(found, span[0], span[1]) {
				found = append(found, match{r.name, span[0], span[1]})
			}
		}
	}

	// 3. Encoded channel: an encoded literal hit earns a bounded decode of
	// the surrounding token span; the owning rule's regex (with its entropy
	// post-filter) must still match the decoded text.
	for i, p := range s.probes {
		ends := probeHits[i]
		if len(ends) == 0 {
			continue
		}
		r := s.rules[p.ruleIdx]
		for _, end := range ends {
			start := end - len(p.variant)
			tok := isB64TokenByte
			if p.hexForm {
				tok = isHexTokenByte
			}
			spanStart, spanEnd := expandSpan(body, start, end, tok)
			if overlaps(found, spanStart, spanEnd) {
				continue
			}
			var decoded []byte
			var ok bool
			if p.hexForm {
				decoded, ok = decodeHexSpan(body[spanStart:spanEnd])
			} else {
				decoded, ok = decodeB64Span(body[spanStart:spanEnd])
			}
			if !ok {
				continue
			}
			if len(r.findIn(decoded)) > 0 {
				found = append(found, match{r.name, spanStart, spanEnd})
			}
		}
	}

	// 4. Custom patterns (plaintext only).
	for _, c := range s.custom {
		if len(c.literal) > 0 && !bytes.Contains(body, c.literal) {
			continue
		}
		for _, loc := range c.re.FindAllIndex(body, -1) {
			if !overlaps(found, loc[0], loc[1]) {
				found = append(found, match{c.name, loc[0], loc[1]})
			}
		}
	}

	return found
}

// Scan returns the deduplicated type names of the secrets found in body:
// "known_secret"/"known_secret_encoded" first, then embedded rule names in
// table order, then custom pattern names in declaration order. An empty
// result means the body is clean (as far as this high-confidence table can
// tell). Secret values are never returned.
func (s *Scanner) Scan(body []byte) []string {
	found := s.findAll(body)
	if len(found) == 0 {
		return nil
	}
	hit := map[string]bool{}
	for _, m := range found {
		hit[m.name] = true
	}
	var names []string
	if hit[knownSecret] {
		names = append(names, knownSecret)
	}
	if hit[knownSecretEncoded] {
		names = append(names, knownSecretEncoded)
	}
	for _, r := range s.rules {
		if hit[r.name] {
			names = append(names, r.name)
		}
	}
	for _, c := range s.custom {
		if hit[c.name] {
			names = append(names, c.name)
		}
	}
	return names
}

// Redact returns body with every matched secret replaced by
// RedactPlaceholder — including encoded spans and encoded known-secret
// variants. A clean body is returned unchanged. The replacement is a pure
// byte-substring substitution, so a match inside a JSON string stays valid
// JSON. Sensitive-path hits are never redacted.
func (s *Scanner) Redact(body []byte) []byte {
	found := s.findAll(body)
	if len(found) == 0 {
		return body
	}
	// findAll claims spans in priority order, not byte order — sort by offset
	// so the reconstruction walks the body left to right.
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

// shannon returns the Shannon entropy of b in bits per byte.
func shannon(b []byte) float64 {
	if len(b) == 0 {
		return 0
	}
	var freq [256]int
	for _, c := range b {
		freq[c]++
	}
	n := float64(len(b))
	var h float64
	for _, f := range freq {
		if f > 0 {
			p := float64(f) / n
			h -= p * math.Log2(p)
		}
	}
	return h
}
