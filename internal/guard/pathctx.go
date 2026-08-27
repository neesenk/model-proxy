package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"sort"
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
// pays one streaming json.Decoder pass to locate the strong value spans, so a
// clean body costs exactly what ScanPaths costs today. Every pass is linear
// (nesting depth is bounded by encoding/json's own maxNestingDepth).
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

func ctxKeyClass(key string) int {
	switch key {
	case "arguments":
		return ctxArguments
	case "input":
		return ctxInput
	case "content":
		return ctxContent
	case "output":
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
// undecided when the object closes classify weak (宁低勿高).
type ctxObjFrame struct {
	typ, role       string
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
			return true, f.typ == "tool_use"
		}
	case ctxContent:
		if f.hasTyp && f.typ == "tool_result" {
			return true, true
		}
		if f.hasRole && f.role == "tool" {
			return true, true
		}
		if f.hasTyp && f.hasRole {
			return true, false
		}
	case ctxOutput:
		if f.hasTyp {
			return true, f.typ == "function_call_output"
		}
	}
	return false, false
}

// ctxWalker is a one-pass streaming JSON walk collecting the byte spans of
// strong value positions. Offsets come from Decoder.InputOffset, so spans
// refer to the raw body (JSON escapes included) — exactly the coordinate
// space the path literals matched in. Nesting depth is bounded by
// encoding/json's maxNestingDepth (10000), which also bounds the recursion.
type ctxWalker struct {
	dec   *json.Decoder
	spans [][2]int
}

var errCtxDelim = errors.New("guard: unexpected delimiter in JSON walk")

// strongValueSpans returns the merged, sorted spans of tool-argument and
// tool-result value positions in body, or nil when body is not a single
// walkable JSON value (callers then classify every path hit weak).
func strongValueSpans(body []byte) [][2]int {
	w := &ctxWalker{dec: json.NewDecoder(bytes.NewReader(body))}
	if _, _, err := w.value(); err != nil || len(w.spans) == 0 {
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

// value consumes exactly one JSON value and returns the offset just past it;
// a primitive string value is also returned (the "type"/"role" markers need
// it). The caller captures the value's start via dec.InputOffset BEFORE the
// call, so a value span also covers the preceding ':' and whitespace — a
// path hit can never lie in that gap alone (literals are trimmed non-empty),
// so intersection semantics stay exact for real occurrences.
func (w *ctxWalker) value() (string, int, error) {
	tok, err := w.dec.Token()
	if err != nil {
		return "", 0, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			err = w.object()
		case '[':
			err = w.array()
		default:
			err = errCtxDelim
		}
		if err != nil {
			return "", 0, err
		}
		return "", int(w.dec.InputOffset()), nil
	case string:
		return t, int(w.dec.InputOffset()), nil
	default:
		return "", int(w.dec.InputOffset()), nil
	}
}

func (w *ctxWalker) object() error {
	f := &ctxObjFrame{}
	for w.dec.More() {
		tok, err := w.dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		cls := ctxKeyClass(key)
		start := int(w.dec.InputOffset())
		strVal, end, err := w.value()
		if err != nil {
			return err
		}
		if key == "type" || key == "role" {
			// Last occurrence wins (JSON semantics); every new marker
			// re-resolves the pending candidates of this object.
			if key == "type" {
				f.typ, f.hasTyp = strVal, true
			} else {
				f.role, f.hasRole = strVal, true
			}
			f.resolvePending(w)
		}
		if cls != ctxNone {
			if decided, strong := f.decide(cls); decided {
				if strong {
					w.spans = append(w.spans, [2]int{start, end})
				}
			} else {
				f.pending = append(f.pending, ctxCandidate{cls: cls, start: start, end: end})
			}
		}
	}
	_, err := w.dec.Token() // '}'
	return err
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
	for w.dec.More() {
		if _, _, err := w.value(); err != nil {
			return err
		}
	}
	_, err := w.dec.Token() // ']'
	return err
}
