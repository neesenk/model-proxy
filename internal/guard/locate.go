package guard

import "sort"

// Strength values for LocatedMatch.Strength (sensitive-path hits only;
// secret matches leave it empty).
const (
	StrengthStrong = "strong"
	StrengthWeak   = "weak"
)

// LocatedMatch is one located occurrence of a requested name: its byte span
// in the scanned body plus, for sensitive-path hits, the per-occurrence
// strong/weak structural classification (see ScanPathsContext). It carries
// coordinates only — never the matched bytes (credential red line).
type LocatedMatch struct {
	Name       string
	Start, End int
	Strength   string
}

// Locate re-runs detection over body and returns the byte spans of every
// occurrence of the requested names (embedded rule names, custom pattern
// names, the known-secret channel names, sensitive-path categories,
// "custom_path"), sorted by offset. Names that do not occur contribute
// nothing. It exists for the admin security-explain surface, which re-scans
// a persisted request body on demand to show WHERE each recorded guard hit
// fired; the live request path keeps using the cheaper name-only scans.
//
// Path occurrences are classified individually: an occurrence intersecting a
// tool-invocation value span (tool_use.input / function arguments) is strong,
// every other is weak — including every occurrence in a body whose JSON
// structure does not walk (宁低勿高, same downgrade rule as ScanPathsContext).
func (s *Scanner) Locate(body []byte, names []string) []LocatedMatch {
	if s == nil || len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	var out []LocatedMatch
	for _, m := range s.findAll(body) {
		if want[m.name] {
			out = append(out, LocatedMatch{Name: m.name, Start: m.start, End: m.end})
		}
	}
	if wantsPathLocation(want) {
		out = append(out, s.locatePathHits(body, want)...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out
}

// wantsPathLocation reports whether any requested name is a sensitive-path
// category (builtin or custom_path).
func wantsPathLocation(want map[string]bool) bool {
	for _, p := range builtinPaths {
		if want[p.category] {
			return true
		}
	}
	return want["custom_path"]
}

// locatePathHits returns one LocatedMatch per occurrence of the requested
// path categories, classified against the body's strong value spans.
func (s *Scanner) locatePathHits(body []byte, want map[string]bool) []LocatedMatch {
	spans := strongValueSpans(body)
	var out []LocatedMatch
	collect := func(cat string, each func(fn func(start, end int))) {
		if !want[cat] {
			return
		}
		each(func(start, end int) {
			strength := StrengthWeak
			if intersectsSpan(spans, start, end) {
				strength = StrengthStrong
			}
			out = append(out, LocatedMatch{Name: cat, Start: start, End: end, Strength: strength})
		})
	}
	for _, p := range builtinPaths {
		p := p
		collect(p.category, func(fn func(start, end int)) { p.eachOccurrence(body, fn) })
	}
	collect("custom_path", func(fn func(start, end int)) {
		for _, lit := range s.extraPaths {
			forEachOccurrence(body, lit, fn)
		}
	})
	return out
}

// RuleInfo returns the regex text and source attribution of a named secret
// rule — the embedded table first, then custom patterns (source "config").
// ok is false for sensitive-path categories and the known-secret channel
// names, which have no single regex to show.
func (s *Scanner) RuleInfo(name string) (regex, source string, ok bool) {
	if s == nil {
		return "", "", false
	}
	for _, r := range s.rules {
		if r.name == name {
			return r.regex, r.source, true
		}
	}
	for _, c := range s.custom {
		if c.name == name {
			return c.re.String(), "config", true
		}
	}
	return "", "", false
}

// MaskSnippet renders the context window around [start,end) for display on
// the admin security-explain surface, as three pre-split pieces — pre / hit /
// post — so callers never map byte offsets across encodings (the web UI
// slices JS UTF-16 strings, not Go bytes). The hit itself is masked only
// when maskHit (secret matches); EVERY OTHER secret match overlapping the
// window is always masked — the context bytes of a path hit can carry an
// unrelated secret, and the request body must be treated as sensitive as a
// whole. A secret span intersecting an unmasked path hit is left as-is
// (masking it would eat the highlighted path literal; path literals are not
// secrets).
func (s *Scanner) MaskSnippet(body []byte, start, end int, maskHit bool, ctxBytes int) (pre, hit, post string) {
	s0 := max(start-ctxBytes, 0)
	e0 := min(end+ctxBytes, len(body))
	type maskedSpan struct{ start, end int }
	var masked []maskedSpan
	if maskHit {
		masked = append(masked, maskedSpan{start, end})
	}
	if s != nil {
		for _, m := range s.findAll(body) {
			if m.end <= s0 || m.start >= e0 {
				continue // outside the window
			}
			if m.start < end && m.end > start {
				continue // intersects the hit itself (handled by maskHit)
			}
			// Clip to the window: the overlap test above keeps secrets that
			// start before s0 or end after e0, and an unclipped span would
			// drive pos past e0 (or behind s0) and panic the flushes.
			masked = append(masked, maskedSpan{max(m.start, s0), min(m.end, e0)})
		}
	}
	sort.Slice(masked, func(i, j int) bool { return masked[i].start < masked[j].start })
	// Assemble left to right into the three output buffers; masked spans are
	// disjoint (findAll claims are, and the maskHit span intersects no kept
	// secret span).
	var preB, hitB, postB []byte
	cur := &preB
	pos := s0
	hitDone := false
	flush := func(upto int) {
		*cur = append(*cur, body[pos:upto]...)
		pos = upto
	}
	for _, m := range masked {
		if !maskHit && !hitDone && pos <= start && m.start >= start {
			// The unmasked hit lies inside this raw gap (masked spans never
			// intersect it): split the gap at the hit boundaries.
			flush(start)
			cur = &hitB
			flush(end)
			cur = &postB
			hitDone = true
		}
		flush(m.start)
		mask := maskSecretBytes(body[m.start:m.end])
		if maskHit && m.start == start {
			hitB = append(hitB, mask...)
			pos = m.end
			cur = &postB
			hitDone = true
			continue
		}
		*cur = append(*cur, mask...)
		pos = m.end
	}
	if !maskHit && !hitDone {
		flush(start)
		cur = &hitB
		flush(end)
		cur = &postB
	}
	flush(e0)
	return string(preB), string(hitB), string(postB)
}

// maskSecretBytes renders a matched secret unrecoverable but recognizable:
// first 4 + "…" + last 2 bytes; very short matches collapse to "***".
func maskSecretBytes(b []byte) string {
	if len(b) < 8 {
		return "***"
	}
	return string(b[:4]) + "…" + string(b[len(b)-2:])
}
