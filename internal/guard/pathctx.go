package guard

import (
	"bytes"
	"errors"
	"sort"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// ScanPathsContext is the context-aware form of ScanPaths: the same
// sensitive-path table, but every hit is classified by WHERE in the request
// body the path appears.
//
//   - strong (high confidence): the path sits in a tool-call / tool-result
//     value position — the structural signature of an agent READING a file
//     through a tool, which is the MCP Tool Poisoning attack shape. Recognized
//     positions, matched by key name only (no full protocol schema; the three
//     chat protocols share this one key set): an object's "input" value when
//     the object has type="tool_use" (anthropic); a "content" value when the
//     object has type="tool_result" (anthropic) or role="tool" (openai tool
//     message); an "arguments" value unconditionally (openai
//     tool_calls[].function.arguments nests it under a typeless function
//     object, responses function_call.arguments puts it directly on the
//     typed object — both use that exact key); an "output" value when the
//     object has type="function_call_output" (responses). The openai
//     arguments string is escaped JSON; the whole string token is the strong
//     span, so a path inside the embedded JSON is strong without parsing a
//     second JSON level.
//   - weak (low confidence): everywhere else — ordinary prose, user messages.
//     Coding agents legitimately discuss .env & friends in text all the time.
//
// A body that is not one walkable JSON value classifies EVERY hit weak
// (宁低勿高 — a structure-recognition failure must never upgrade a hit to
// strong).
//
// Cost: the ScanPaths literal gate runs first; only a body WITH a path hit
// pays one JSON structure pass to locate the strong value spans, so a clean
// body costs exactly what ScanPaths costs today. Every pass is linear
// (nesting depth is bounded by the walker's maxCtxDepth, mirroring
// encoding/json's maxNestingDepth).
//
// strong and weak are disjoint — a category with at least one strong
// occurrence reports strong only. Both lists follow ScanPaths ordering: the
// builtin table order, then "custom_path" for extra_paths hits.
func (s *Scanner) ScanPathsContext(body []byte) (strong, weak []string) {
	// Phase 1: literal gate — identical cost to ScanPaths on clean bodies,
	// which therefore never touch the JSON structure walk.
	gate := false
	for _, p := range builtinPaths {
		if p.hit(body) {
			gate = true
			break
		}
	}
	if !gate {
		for _, lit := range s.extraPaths {
			if bytes.Contains(body, lit) {
				gate = true
				break
			}
		}
	}
	if !gate {
		return nil, nil
	}

	// Phase 2: locate the strong value spans once (nil on any walk failure →
	// everything classifies weak), then classify each occurrence by span
	// intersection.
	spans := strongValueSpans(body)

	classify := func(cat string, each func(fn func(start, end int))) {
		isStrong, isWeak := false, false
		each(func(start, end int) {
			if isStrong {
				return
			}
			if intersectsSpan(spans, start, end) {
				isStrong = true
			} else {
				isWeak = true
			}
		})
		out := &weak
		if isStrong {
			out = &strong
		} else if !isWeak {
			return
		}
		if len(*out) == 0 || (*out)[len(*out)-1] != cat {
			*out = append(*out, cat)
		}
	}
	for _, p := range builtinPaths {
		p := p
		classify(p.category, func(fn func(start, end int)) { p.eachOccurrence(body, fn) })
	}
	classify("custom_path", func(fn func(start, end int)) {
		for _, lit := range s.extraPaths {
			forEachOccurrence(body, lit, fn)
		}
	})
	return strong, weak
}

// eachOccurrence calls fn for every boundary-respecting occurrence of the
// rule's literals in body (the occurrence-level form of hit).
func (p pathRule) eachOccurrence(body []byte, fn func(start, end int)) {
	for _, lit := range p.literals {
		if !p.boundary {
			forEachOccurrence(body, lit, fn)
			continue
		}
		forEachOccurrence(body, lit, func(start, end int) {
			beforeOK := start == 0 || !isPathChar(body[start-1])
			afterOK := end == len(body) || !isPathChar(body[end])
			if beforeOK && afterOK {
				fn(start, end)
			}
		})
	}
}

// intersectsSpan reports whether [start,end) intersects any span. spans are
// disjoint, merged and sorted by start, so their ends are sorted too and one
// binary search suffices.
func intersectsSpan(spans [][2]int, start, end int) bool {
	i := sort.Search(len(spans), func(i int) bool { return spans[i][1] > start })
	return i < len(spans) && spans[i][0] < end
}

// --- tool-position structure recognition ---

// Context classes for object keys whose VALUE may be a tool-call/tool-result
// position (the protocol key set is documented on ScanPathsContext).
const (
	ctxNone = iota
	ctxArguments
	ctxInput
	ctxContent
	ctxOutput
)

// Key and marker literals compared against raw/decoded body subslices without
// allocation (bytes.Equal on a package-level slice never copies).
var (
	keyArguments = []byte("arguments")
	keyInput     = []byte("input")
	keyContent   = []byte("content")
	keyOutput    = []byte("output")
	keyType      = []byte("type")
	keyRole      = []byte("role")

	markerToolUse            = []byte("tool_use")
	markerToolResult         = []byte("tool_result")
	markerTool               = []byte("tool")
	markerFunctionCallOutput = []byte("function_call_output")
)

func ctxKeyClass(key []byte) int {
	switch {
	case bytes.Equal(key, keyArguments):
		return ctxArguments
	case bytes.Equal(key, keyInput):
		return ctxInput
	case bytes.Equal(key, keyContent):
		return ctxContent
	case bytes.Equal(key, keyOutput):
		return ctxOutput
	}
	return ctxNone
}

// ctxCandidate is a key-value span awaiting the enclosing object's type/role
// markers (an encoder may emit "input" before "type").
type ctxCandidate struct {
	cls        int
	start, end int
}

// ctxObjFrame tracks what one JSON object has revealed so far: its type/role
// markers and the candidate value spans still waiting for them. Candidates
// undecided when the object closes classify weak (宁低勿高). Marker values are
// decoded string contents — a body subslice when the token had no escapes, so
// the common case allocates nothing.
type ctxObjFrame struct {
	typ, role       []byte
	hasTyp, hasRole bool
	pending         []ctxCandidate
}

// decide resolves a candidate against the markers known so far; decided=false
// keeps it pending until more pairs arrive.
func (f *ctxObjFrame) decide(cls int) (decided, strong bool) {
	switch cls {
	case ctxArguments:
		return true, true
	case ctxInput:
		if f.hasTyp {
			return true, bytes.Equal(f.typ, markerToolUse)
		}
	case ctxContent:
		if f.hasTyp && bytes.Equal(f.typ, markerToolResult) {
			return true, true
		}
		if f.hasRole && bytes.Equal(f.role, markerTool) {
			return true, true
		}
		if f.hasTyp && f.hasRole {
			return true, false
		}
	case ctxOutput:
		if f.hasTyp {
			return true, bytes.Equal(f.typ, markerFunctionCallOutput)
		}
	}
	return false, false
}

// ctxWalker is a one-pass JSON walk over the raw body collecting the byte
// spans of strong value positions. Spans refer to the raw body (JSON escapes
// included) — exactly the coordinate space the path literals matched in.
// String contents are skipped in place; only object keys and "type"/"role"
// marker values are ever decoded, and those decode to a body subslice when
// the token carries no escapes, so the common large-body walk allocates
// nothing (the previous json.Decoder.Token walk materialized every string
// token — ~5x the body size in allocations on a 4MiB request).
//
// The grammar check must stay exactly as strict as the encoding/json walk it
// replaces: raw control characters in strings, invalid escapes, malformed
// numbers (including float64-overflowing ones like 1e999, which
// Decoder.Token rejects) and nesting deeper than maxCtxDepth all fail the
// walk, and the caller then classifies every hit weak (宁低勿高: a
// structure-recognition failure must never upgrade a hit to strong).
// Invalid UTF-8 bytes inside strings and lone \u surrogates are accepted,
// mirroring Decoder.Token.
type ctxWalker struct {
	body  []byte
	pos   int
	depth int
	spans [][2]int
}

// maxCtxDepth mirrors encoding/json's maxNestingDepth.
const maxCtxDepth = 10000

var errCtxSyntax = errors.New("guard: invalid JSON structure in context walk")

// strongValueSpans returns the merged, sorted spans of tool-argument and
// tool-result value positions in body, or nil when body does not start with
// one walkable JSON value (callers then classify every path hit weak).
// Content after that first value is not inspected, matching the previous
// json.Decoder-based walk.
func strongValueSpans(body []byte) [][2]int {
	w := &ctxWalker{body: body}
	if _, err := w.value(false); err != nil || len(w.spans) == 0 {
		return nil
	}
	sort.Slice(w.spans, func(i, j int) bool { return w.spans[i][0] < w.spans[j][0] })
	merged := w.spans[:1]
	for _, sp := range w.spans[1:] {
		last := &merged[len(merged)-1]
		if sp[0] <= last[1] {
			if sp[1] > last[1] {
				last[1] = sp[1]
			}
		} else {
			merged = append(merged, sp)
		}
	}
	return merged
}

// value consumes exactly one JSON value starting at w.pos and leaves w.pos
// just past it. When the value is a string and collectString is true the
// decoded content is returned (the "type"/"role" markers need it); any other
// value kind returns nil. The caller captures the value's start BEFORE the
// call, so a value span also covers the preceding ':' and whitespace — a
// path hit can never lie in that gap alone (literals are trimmed non-empty),
// so intersection semantics stay exact for real occurrences.
func (w *ctxWalker) value(collectString bool) ([]byte, error) {
	w.skipWS()
	if w.pos >= len(w.body) {
		return nil, errCtxSyntax
	}
	switch c := w.body[w.pos]; {
	case c == '{':
		return nil, w.object()
	case c == '[':
		return nil, w.array()
	case c == '"':
		return w.scanString(collectString)
	case c == 't':
		return nil, w.scanLiteral("true")
	case c == 'f':
		return nil, w.scanLiteral("false")
	case c == 'n':
		return nil, w.scanLiteral("null")
	case c == '-' || (c >= '0' && c <= '9'):
		return nil, w.scanNumber()
	}
	return nil, errCtxSyntax
}

func (w *ctxWalker) object() error {
	w.depth++
	if w.depth > maxCtxDepth {
		return errCtxSyntax
	}
	defer func() { w.depth-- }()
	w.pos++ // '{'
	f := &ctxObjFrame{}
	w.skipWS()
	if w.pos < len(w.body) && w.body[w.pos] == '}' {
		w.pos++
		return nil
	}
	for {
		w.skipWS()
		if w.pos >= len(w.body) || w.body[w.pos] != '"' {
			return errCtxSyntax
		}
		key, err := w.scanString(true)
		if err != nil {
			return err
		}
		// The span starts just past the key token (covering the ':' and
		// whitespace before the value), matching the previous
		// Decoder.InputOffset coordinates.
		start := w.pos
		w.skipWS()
		if w.pos >= len(w.body) || w.body[w.pos] != ':' {
			return errCtxSyntax
		}
		w.pos++
		isType, isRole := bytes.Equal(key, keyType), bytes.Equal(key, keyRole)
		strVal, err := w.value(isType || isRole)
		if err != nil {
			return err
		}
		end := w.pos
		if isType || isRole {
			// Last occurrence wins (JSON semantics); every new marker
			// re-resolves the pending candidates of this object.
			if isType {
				f.typ, f.hasTyp = strVal, true
			} else {
				f.role, f.hasRole = strVal, true
			}
			f.resolvePending(w)
		}
		if cls := ctxKeyClass(key); cls != ctxNone {
			if decided, strong := f.decide(cls); decided {
				if strong {
					w.spans = append(w.spans, [2]int{start, end})
				}
			} else {
				f.pending = append(f.pending, ctxCandidate{cls: cls, start: start, end: end})
			}
		}
		w.skipWS()
		if w.pos >= len(w.body) {
			return errCtxSyntax
		}
		switch w.body[w.pos] {
		case ',':
			w.pos++
		case '}':
			w.pos++
			return nil
		default:
			return errCtxSyntax
		}
	}
}

// resolvePending re-decides every pending candidate after the object learned
// a type/role marker, keeping the still-undecided ones.
func (f *ctxObjFrame) resolvePending(w *ctxWalker) {
	kept := f.pending[:0]
	for _, c := range f.pending {
		if decided, strong := f.decide(c.cls); decided {
			if strong {
				w.spans = append(w.spans, [2]int{c.start, c.end})
			}
		} else {
			kept = append(kept, c)
		}
	}
	f.pending = kept
}

func (w *ctxWalker) array() error {
	w.depth++
	if w.depth > maxCtxDepth {
		return errCtxSyntax
	}
	defer func() { w.depth-- }()
	w.pos++ // '['
	w.skipWS()
	if w.pos < len(w.body) && w.body[w.pos] == ']' {
		w.pos++
		return nil
	}
	for {
		if _, err := w.value(false); err != nil {
			return err
		}
		w.skipWS()
		if w.pos >= len(w.body) {
			return errCtxSyntax
		}
		switch w.body[w.pos] {
		case ',':
			w.pos++
		case ']':
			w.pos++
			return nil
		default:
			return errCtxSyntax
		}
	}
}

func (w *ctxWalker) skipWS() {
	for w.pos < len(w.body) {
		switch w.body[w.pos] {
		case ' ', '\t', '\n', '\r':
			w.pos++
		default:
			return
		}
	}
}

// scanLiteral consumes exactly lit at w.pos.
func (w *ctxWalker) scanLiteral(lit string) error {
	if len(w.body)-w.pos < len(lit) {
		return errCtxSyntax
	}
	for i := 0; i < len(lit); i++ {
		if w.body[w.pos+i] != lit[i] {
			return errCtxSyntax
		}
	}
	w.pos += len(lit)
	return nil
}

// scanNumber consumes one JSON number at w.pos. The grammar check is strict
// (no digit runs after a leading "0", mandatory digits after '.' and the
// exponent sign), and — like Decoder.Token, which decodes numbers to
// float64 — values outside float64 range (1e999) fail the walk.
func (w *ctxWalker) scanNumber() error {
	body := w.body
	i := w.pos
	if i < len(body) && body[i] == '-' {
		i++
	}
	if i >= len(body) {
		return errCtxSyntax
	}
	if body[i] == '0' {
		i++
	} else if body[i] >= '1' && body[i] <= '9' {
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
		}
	} else {
		return errCtxSyntax
	}
	if i < len(body) && body[i] == '.' {
		i++
		if i >= len(body) || body[i] < '0' || body[i] > '9' {
			return errCtxSyntax
		}
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
		}
	}
	if i < len(body) && (body[i] == 'e' || body[i] == 'E') {
		i++
		if i < len(body) && (body[i] == '+' || body[i] == '-') {
			i++
		}
		if i >= len(body) || body[i] < '0' || body[i] > '9' {
			return errCtxSyntax
		}
		for i < len(body) && body[i] >= '0' && body[i] <= '9' {
			i++
		}
	}
	if _, err := strconv.ParseFloat(string(body[w.pos:i]), 64); err != nil {
		return errCtxSyntax
	}
	w.pos = i
	return nil
}

// scanString consumes the string token at w.pos (which must sit on the
// opening quote) and advances just past the closing quote. When collect is
// true it returns the decoded content: a subslice of the body when the token
// has no escapes, otherwise freshly unescaped bytes. Raw control characters
// and invalid escapes fail the walk exactly like encoding/json; invalid
// UTF-8 bytes pass through untouched, which Decoder.Token tolerates.
//
// Escape-free runs (the common case for large prompt/tool strings) are
// located with SIMD bytes.IndexByte scans, leaving only a single-comparison
// control-character check per byte.
func (w *ctxWalker) scanString(collect bool) ([]byte, error) {
	body := w.body
	content := w.pos + 1
	i := content
	var out []byte
	for {
		q := bytes.IndexByte(body[i:], '"')
		if q < 0 {
			return nil, errCtxSyntax
		}
		end := i + q
		run := end
		hasEsc := false
		if e := bytes.IndexByte(body[i:end], '\\'); e >= 0 {
			run = i + e
			hasEsc = true
		}
		for j := i; j < run; j++ {
			c := body[j]
			if c < 0x20 {
				return nil, errCtxSyntax
			}
			if out != nil {
				out = append(out, c)
			}
		}
		i = run
		if !hasEsc {
			w.pos = end + 1
			if !collect {
				return nil, nil
			}
			if out == nil {
				return body[content:end], nil
			}
			return out, nil
		}
		r, size, err := scanEscape(body, i)
		if err != nil {
			return nil, err
		}
		if collect {
			if out == nil {
				out = append([]byte{}, body[content:i]...)
			}
			out = utf8.AppendRune(out, r)
		}
		i += size
	}
}

// scanEscape decodes the escape sequence starting at body[i] (body[i] must
// be '\\') and returns the decoded rune and the sequence length in bytes.
// A high surrogate directly followed by a low-surrogate escape combines;
// lone surrogates decode to utf8.RuneError, mirroring how encoding/json
// unquotes them.
func scanEscape(body []byte, i int) (rune, int, error) {
	if i+1 >= len(body) {
		return 0, 0, errCtxSyntax
	}
	switch c := body[i+1]; c {
	case '"', '\\', '/':
		return rune(c), 2, nil
	case 'b':
		return '\b', 2, nil
	case 'f':
		return '\f', 2, nil
	case 'n':
		return '\n', 2, nil
	case 'r':
		return '\r', 2, nil
	case 't':
		return '\t', 2, nil
	case 'u':
	default:
		return 0, 0, errCtxSyntax
	}
	r1, ok := scanHex4(body, i+2)
	if !ok {
		return 0, 0, errCtxSyntax
	}
	if !utf16.IsSurrogate(r1) {
		return r1, 6, nil
	}
	if r1 >= 0xDC00 || i+8 > len(body) || body[i+6] != '\\' || body[i+7] != 'u' {
		return utf8.RuneError, 6, nil
	}
	r2, ok := scanHex4(body, i+8)
	if !ok {
		return 0, 0, errCtxSyntax
	}
	if r2 < 0xDC00 || r2 > 0xDFFF {
		return utf8.RuneError, 6, nil
	}
	return utf16.DecodeRune(r1, r2), 12, nil
}

func scanHex4(body []byte, i int) (rune, bool) {
	if i+4 > len(body) {
		return 0, false
	}
	var r rune
	for _, c := range body[i : i+4] {
		var v byte
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	return r, true
}
