package guard

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// minSecretLen is the minimum length for a known secret to enter the scan
// set: shorter values are too likely to collide with ordinary text.
const minSecretLen = 12

// minKnownFrag is the minimum fragment length the split-exfiltration
// (cross-request) channel credits: each piece of a fragmented known secret
// must be at least this long, so a few coincidental bytes of a high-entropy
// value cannot advance a session's fragment progress.
const minKnownFrag = 8

// maxEncodedSpan bounds the token span expanded around an encoded-literal
// hit, so a pathological run of token bytes cannot make decoding quadratic.
const maxEncodedSpan = 8 << 10

// maxDecodesPerScan bounds encoded-channel decode attempts per scan. Real
// bodies need a handful (one per encoded blob); an adversarial body of probe
// variants repeated at byte intervals inside token runs longer than
// maxEncodedSpan would otherwise force one ~maxEncodedSpan decode per hit.
// When the budget runs out the encoded channel stops for the rest of the
// scan — bounded under-detection, never over-detection (宁漏勿滥).
const maxDecodesPerScan = 256

// maxMatchesPerScan bounds the claimed-match table. Verdicts are name-based
// and Redact has already replaced every span it claimed; only adversarial
// repetition of one secret shape approaches the bound, and past it sources
// stop claiming so span processing stays linear in body size.
const maxMatchesPerScan = 1 << 18

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

// encodedProbe is one precomputed encoded form of one or more embedded
// rules' literals: when the variant bytes appear in a body, the surrounding
// token span is decoded and every owning rule's regex is run on the decoded
// text. Several rules can share a literal (e.g. AKIA in the two AWS rules)
// and therefore a variant, so the probe keeps every owner in table order.
type encodedProbe struct {
	ruleIdxs []int
	variant  []byte
	hexForm  bool // hex variant; false = base64 variant
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
//
// The [start,end) slice is fully lit-determined for ANY literal length by
// construction (start rounds the leading pad-affected char up, end rounds the
// trailing next-byte-affected char down); the threshold below is only a
// usefulness floor. At 3, a 3-byte literal (sk-, hf_, eyJ, A3T, SG.)
// contributes probes at all three alignments: align 0 yields 4 chars, aligns
// 1/2 yield 3 chars, each fully determined by lit (e.g. align 1: chars 2..5
// cover lit[0] low nibble through lit[2] top bits — no pad or follower bit).
// A 3-char base64 probe collides with random text at ~1/262144 per position,
// still specific enough for a prefilter.
func b64Interior(enc *base64.Encoding, lit []byte, align int) string {
	padded := make([]byte, 0, align+len(lit))
	padded = append(padded, make([]byte, align)...)
	padded = append(padded, lit...)
	full := enc.EncodeToString(padded)
	start := (8*align + 5) / 6        // ceil(8*align/6)
	end := (8*align + 8*len(lit)) / 6 // floor(8*(align+len(lit))/6)
	if end-start < 3 {
		return ""
	}
	return full[start:end]
}

// buildProbes precomputes the encoded-channel probes for the embedded rule
// table: for every rule literal, the base64 interior variants (standard and
// URL alphabets × three byte alignments) and the lowercase hex form. Identical
// variants (shared literals across rules, or cross-literal collisions) merge
// into one probe carrying every owning rule, so an encoded hit re-runs all of
// them instead of only the first rule that happened to register the variant.
func buildProbes(rules []rule) []encodedProbe {
	var probes []encodedProbe
	owner := map[string]int{} // variant string → probe index
	add := func(variant string, hexForm bool, ruleIdx int) {
		if i, ok := owner[variant]; ok {
			for _, ri := range probes[i].ruleIdxs {
				if ri == ruleIdx {
					return
				}
			}
			probes[i].ruleIdxs = append(probes[i].ruleIdxs, ruleIdx)
			return
		}
		owner[variant] = len(probes)
		probes = append(probes, encodedProbe{ruleIdxs: []int{ruleIdx}, variant: []byte(variant), hexForm: hexForm})
	}
	for i, r := range rules {
		for _, lit := range r.literals {
			for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
				for align := 0; align < 3; align++ {
					if v := b64Interior(enc, lit, align); v != "" {
						add(v, false, i)
					}
				}
			}
			add(hex.EncodeToString(lit), true, i)
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

// claimSet tracks claimed match spans as sorted disjoint intervals. It
// replaces the former pairwise scan over the match list: an adversarial body
// repeating one secret shape produces millions of candidate spans, and the
// linear overlap check made claiming quadratic. Each source emits spans in
// increasing offset order, so inserts land at (or near) the back and total
// insert cost stays linear per source.
type claimSet struct {
	starts, ends []int
}

// overlaps reports whether [start,end) intersects a claimed interval.
func (c *claimSet) overlaps(start, end int) bool {
	// Intervals are sorted and disjoint, so their ends are sorted too: the
	// first interval ending after start is the only overlap candidate.
	i := sort.Search(len(c.ends), func(i int) bool { return c.ends[i] > start })
	return i < len(c.starts) && c.starts[i] < end
}

// claim records a non-overlapping interval (callers check overlaps first).
func (c *claimSet) claim(start, end int) {
	i := sort.Search(len(c.starts), func(i int) bool { return c.starts[i] > start })
	c.starts = slices.Insert(c.starts, i, start)
	c.ends = slices.Insert(c.ends, i, end)
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

// scanStats counts bounded decode attempts during one findAll pass — a
// package-internal seam for tests asserting that adversarial bodies cannot
// force quadratic decoding.
type scanStats struct{ decodes int }

// findAll returns every non-overlapping secret match in body. Phase 1 is a
// single automaton pass over all literals; phase 2 claims spans in priority
// order: known secrets (plaintext then encoded variants), the embedded rule
// table in file order (plaintext then the encoded channel), then custom
// patterns in declaration order.
func (s *Scanner) findAll(body []byte) []match {
	found, _ := s.findAllCounted(body)
	return found
}

// findAllCounted is findAll plus the decode-attempt counter.
func (s *Scanner) findAllCounted(body []byte) ([]match, scanStats) {
	var stats scanStats
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

	// Phase 2: precise matching / span claiming. Work is bounded per scan:
	// at most maxMatchesPerScan claimed spans and maxDecodesPerScan decode
	// attempts in the encoded channel, so adversarial repetition of one
	// secret shape degrades to under-reporting instead of unbounded work.
	var found []match
	var claimed claimSet
	capped := func() bool { return len(found) >= maxMatchesPerScan }
	claim := func(name string, start, end int) {
		if capped() || claimed.overlaps(start, end) {
			return
		}
		claimed.claim(start, end)
		found = append(found, match{name, start, end})
	}

	// 1. Known secrets: exact values first, then their encoded variants.
	for si := range s.secrets {
		if capped() {
			break
		}
		sec := &s.secrets[si]
		for _, end := range secretHits[sec.rawID] {
			claim(knownSecret, end-len(sec.raw), end)
		}
	}
	for si := range s.secrets {
		if capped() {
			break
		}
		sec := &s.secrets[si]
		for vi, v := range sec.encoded {
			for _, end := range secretHits[sec.encodedIDs[vi]] {
				claim(knownSecretEncoded, end-len(v), end)
			}
		}
	}

	// 2. Embedded rule table, plaintext.
	for i, r := range s.rules {
		if capped() {
			break
		}
		if !ruleHit[i] {
			continue
		}
		r.findEach(body, func(start, end int) bool {
			claim(r.name, start, end)
			return !capped()
		})
	}

	// 3. Encoded channel: an encoded literal hit earns a bounded decode of
	// the surrounding token span; the owning rules' regexes (with their
	// entropy post-filters) must still match the decoded text. Containment
	// dedup is tracked PER PROBE: hits of one probe whose expanded span falls
	// inside that probe's previously processed span are skipped — the decode
	// result for that region is already decided for this probe, and
	// re-decoding per hit would let an adversarial body of repeated probe
	// hits force quadratic work. A GLOBAL last-span would wrongly silence a
	// later probe whose owners differ: two rules hitting the same token run
	// (e.g. a truncated openai shape plus a complete gitlab token in one
	// base64 blob) must each get their own decode. The processed span is
	// recorded whether or not the decode succeeded. Spans that slide inside
	// a run longer than maxEncodedSpan are bounded by the decode budget.
	var lastStart, lastEnd []int
	if probeHits != nil {
		lastStart = make([]int, len(s.probes))
		lastEnd = make([]int, len(s.probes))
	}
	for i, p := range s.probes {
		if capped() || stats.decodes >= maxDecodesPerScan {
			break
		}
		ends := probeHits[i]
		for _, end := range ends {
			if stats.decodes >= maxDecodesPerScan {
				break
			}
			start := end - len(p.variant)
			tok := isB64TokenByte
			if p.hexForm {
				tok = isHexTokenByte
			}
			spanStart, spanEnd := expandSpan(body, start, end, tok)
			if spanStart >= lastStart[i] && spanEnd <= lastEnd[i] {
				continue
			}
			lastStart[i], lastEnd[i] = spanStart, spanEnd
			if claimed.overlaps(spanStart, spanEnd) {
				continue
			}
			var decoded []byte
			var ok bool
			stats.decodes++
			if p.hexForm {
				decoded, ok = decodeHexSpan(body[spanStart:spanEnd])
			} else {
				decoded, ok = decodeB64Span(body[spanStart:spanEnd])
			}
			if !ok {
				continue
			}
			// A shared variant re-runs every owning rule; the first match in
			// table order claims the span.
			for _, ri := range p.ruleIdxs {
				r := s.rules[ri]
				if len(r.findIn(decoded)) > 0 {
					found = append(found, match{r.name, spanStart, spanEnd})
					claimed.claim(spanStart, spanEnd)
					break
				}
			}
		}
	}

	// 4. Custom patterns (plaintext only), streamed: an adversarial body can
	// yield millions of matches, which FindAllIndex would materialize.
	for _, c := range s.custom {
		if capped() {
			break
		}
		if len(c.literal) > 0 && !bytes.Contains(body, c.literal) {
			continue
		}
		pos := 0
		for pos <= len(body) {
			loc := c.re.FindIndex(body[pos:])
			if loc == nil {
				break
			}
			claim(c.name, pos+loc[0], pos+loc[1])
			if capped() {
				break
			}
			if next := pos + loc[1]; next > pos+loc[0] {
				pos = next
			} else {
				pos = pos + loc[0] + 1 // empty match: advance to make progress
			}
		}
	}

	return found, stats
}

// HasKnownSecrets reports whether the scanner carries any known-secret values
// (proxy-managed credentials). ScanKnown on a scanner without secrets can
// never hit; callers use this to skip per-session aggregation work entirely.
func (s *Scanner) HasKnownSecrets() bool { return len(s.secrets) > 0 }

// ScanKnown runs ONLY the known-secret channel — exact values and their
// encoded (base64/hex/url) variants — skipping the embedded rule table,
// custom patterns and sensitive paths. It returns "known_secret" and/or
// "known_secret_encoded" (in that order), never secret bytes.
//
// Used by the split-exfiltration pass, which re-scans a session window
// concatenation where rule-table matches were already reported per request;
// only a known credential reassembled across requests is new signal. Phase 1
// is the same single automaton pass as findAll (non-secret needles are
// discarded in the callback), so a clean body costs one prefilter sweep.
func (s *Scanner) ScanKnown(body []byte) []string {
	if len(s.secrets) == 0 {
		return nil
	}
	var secretHits map[int][]int // needle id → occurrence end offsets
	s.ac.search(body, func(id, end int) {
		ref := s.refs[id]
		if ref.kind != needleSecretRaw && ref.kind != needleSecretEncoded {
			return
		}
		if secretHits == nil {
			secretHits = map[int][]int{}
		}
		secretHits[id] = append(secretHits[id], end)
	})
	if secretHits == nil {
		return nil
	}
	var raw, encoded bool
	for si := range s.secrets {
		sec := &s.secrets[si]
		if len(secretHits[sec.rawID]) > 0 {
			raw = true
		}
		for _, id := range sec.encodedIDs {
			if len(secretHits[id]) > 0 {
				encoded = true
			}
		}
	}
	var names []string
	if raw {
		names = append(names, knownSecret)
	}
	if encoded {
		names = append(names, knownSecretEncoded)
	}
	return names
}

// ScanKnownFragment advances per-secret split-fragment progress with one
// request body and reports whether this body COMPLETES a known secret whose
// earlier fragments arrived in previous requests of the session.
//
// Why not exact matching on a window+body concatenation: every body the
// proxy forwards starts with '{' (ExtractModel requires a leading JSON
// object), so two requests can never place fragments byte-contiguously
// across the junction — realistic splits put each fragment somewhere inside
// one request's JSON. This channel therefore tracks, per known secret
// (indexed like progress), how long a prefix has been seen IN ORDER across
// the session's requests: a body containing the next ≥minKnownFrag-byte
// piece extends the progress; reaching the full length is fragmented. A
// body containing the COMPLETE secret resets that secret's progress — the
// per-request channel (Scan) reports it, and "fragmented" must stay
// reserved for splits a single-request scan cannot see.
//
// progress is the session state returned by the previous call (nil on first
// request or after a scanner-generation change / window truncation); the
// returned slice is the state to store for the next request. Limitations
// (documented): raw form only (no encoded variants), at most one fragment
// credited per request, every fragment ≥ minKnownFrag bytes, fragments must
// arrive in order.
func (s *Scanner) ScanKnownFragment(body []byte, progress []int) (bool, []int) {
	if len(s.secrets) == 0 {
		return false, nil
	}
	next := make([]int, len(s.secrets))
	fragmented := false
	for i := range s.secrets {
		raw := s.secrets[i].raw
		n := len(raw)
		if n < 2*minKnownFrag {
			// Too short to split into two creditable fragments: only the
			// per-request channel covers it.
			continue
		}
		if bytes.Contains(body, raw) {
			continue // complete in this body — per-request channel's signal
		}
		p := 0
		if i < len(progress) {
			p = progress[i]
		}
		if p > 0 && p < n {
			if j := longestContainedPrefix(raw[p:], body); j > 0 {
				if p+j == n {
					fragmented = true // completed across requests; progress stays 0
					continue
				}
				next[i] = p + j
				continue
			}
		}
		if j := longestContainedPrefix(raw, body); j > 0 && j < n {
			next[i] = j
		}
	}
	return fragmented, next
}

// longestContainedPrefix returns the longest prefix length of frag
// (≥ minKnownFrag) that appears in body, or 0. The min-length prefilter
// keeps this to one bytes.Contains on bodies with no fragment at all.
func longestContainedPrefix(frag, body []byte) int {
	if len(frag) < minKnownFrag || !bytes.Contains(body, frag[:minKnownFrag]) {
		return 0
	}
	j := minKnownFrag
	for j < len(frag) && bytes.Contains(body, frag[:j+1]) {
		j++
	}
	return j
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
//
// Deliberate tradeoff: an encoded-channel hit redacts the ENTIRE expanded
// token span (up to maxEncodedSpan = 8KiB around the probe hit), not just the
// decoded secret's byte range — the encoded position of the secret inside the
// span is not tracked, and partial rewriting would risk leaving secret
// fragments behind. A base64/hex blob carrying a secret is therefore treated
// as tainted as a whole; over-redacting an attachment is acceptable, leaking
// is not (宁滥勿缺 on the redaction side, opposite of the detection side).
func (s *Scanner) Redact(body []byte) []byte {
	found := s.findAll(body)
	if len(found) == 0 {
		return body
	}
	// findAll claims spans in priority order, not byte order — sort by offset
	// so the reconstruction walks the body left to right. sort.Slice (not the
	// former insertion sort): the claimed table can hold six figures of spans
	// under adversarial input.
	sort.Slice(found, func(i, j int) bool { return found[i].start < found[j].start })
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
