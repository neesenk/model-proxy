package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Response model normalization. When a route target's real upstream model
// differs from the called (exposed) model name — the provider-alias case —
// upstreams echo THEIR name in the response's model field, and clients learn
// a model id they never called (breaking session bookkeeping that keys on the
// response model). These helpers rewrite the model field of the CLIENT-facing
// bytes back to the called name. The transport only invokes them when the two
// names actually differ; equal names keep the zero-copy passthrough.
//
// Rewrites are structural, never substring replacement: the JSON token stream
// is walked and only the model VALUE is spliced, so a `"model":"x"` literal
// inside generated content is left untouched. Unrewritten bytes are preserved
// exactly (offsets are spliced, not re-encoded).

// NormalizeResponseModel rewrites the top-level "model" string of a
// non-streaming JSON response to model. All three protocols stamp model at
// the top level of their non-stream response object. The rewrite is
// best-effort structural: a body without a top-level string model (including
// non-JSON bytes) is returned unchanged — normalization fixes a WRONG model
// value, it never fabricates one into a body the upstream sent without it.
func NormalizeResponseModel(body []byte, proto Protocol, model string) []byte {
	if model == "" {
		return body
	}
	switch proto {
	case Anthropic, OpenAI, Responses:
	default:
		return body
	}
	if out, ok := spliceTopLevelStringValue(body, "model", model); ok {
		return out
	}
	return body
}

// NormalizeSSEModelStream wraps a client-facing SSE stream so the model field
// of every frame that carries one reads model:
//
//   - openai: top-level "model" of every chunk;
//   - anthropic: nested message.model of the message_start frame (the only
//     frame that carries a model — after it is rewritten the rest of the
//     stream is copied through without further scanning);
//   - responses: nested response.model of the response.created/completed/
//     incomplete/failed snapshots.
//
// Frames without a rewritable model field — including malformed JSON, [DONE],
// comments and event-only frames — pass through byte-identical; normalization
// never drops or reorders stream content. A rewritten payload that spans
// several data: lines keeps one data: line per payload line.
//
// Cost shape (openai rewrites EVERY chunk of an aliased stream): the model
// is JSON-encoded once per stream, frames are assembled and rewritten in
// per-stream buffers, and the model value is located by a byte-level walk
// (scanTopLevelStringValue) — no per-chunk allocation.
func NormalizeSSEModelStream(source io.ReadCloser, proto Protocol, model string) io.ReadCloser {
	if model == "" {
		return source
	}
	switch proto {
	case Anthropic, OpenAI, Responses:
	default:
		return source
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return source
	}
	return &sseModelStream{
		source:  source,
		reader:  bufio.NewReaderSize(source, sseModelReadBuffer),
		proto:   proto,
		model:   model,
		encoded: encoded,
	}
}

// sseModelReadBuffer sizes the frame reader: an openai chunk fits whole, so
// a line rarely needs more than one buffered read.
const sseModelReadBuffer = 32 << 10

type sseModelStream struct {
	source  io.ReadCloser
	reader  *bufio.Reader
	proto   Protocol
	model   string
	encoded []byte // the JSON encoding of model, computed once
	out     []byte // transformed bytes not yet consumed by Read
	raw     bool   // anthropic fast-forward: message_start already rewritten
	eof     bool
	err     error // sticky source read error, returned once out drains

	// Per-stream scratch, reused frame after frame. Read only refills out
	// once it is fully consumed, so a buffer out aliases is never rewritten
	// while still being read.
	frame     []byte // the frame being assembled
	payload   []byte // folded payload of a multi-line data frame
	rewritten []byte // the rewritten payload
	rendered  []byte // the rewritten frame
}

func (s *sseModelStream) Read(p []byte) (int, error) {
	for len(s.out) == 0 {
		if s.raw {
			if s.err != nil {
				return 0, s.err
			}
			return s.reader.Read(p)
		}
		if s.eof {
			return 0, s.err
		}
		frame, err := s.readFrame()
		s.out = s.transform(frame)
		if err != nil {
			s.eof = true
			s.err = err
		}
	}
	n := copy(p, s.out)
	s.out = s.out[n:]
	return n, nil
}

func (s *sseModelStream) Close() error { return s.source.Close() }

// readFrame accumulates one SSE frame (up to and including the blank
// dispatching line) into the stream's frame buffer. A partial frame at EOF is
// returned with io.EOF; a source error mid-frame returns what was read plus
// the error.
func (s *sseModelStream) readFrame() ([]byte, error) {
	s.frame = s.frame[:0]
	lineStart := 0
	for {
		chunk, err := s.reader.ReadSlice('\n')
		s.frame = append(s.frame, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // a line longer than the buffer: keep accumulating it
		}
		if err != nil {
			return s.frame, err
		}
		if len(bytes.TrimRight(s.frame[lineStart:], "\r\n")) == 0 {
			return s.frame, nil
		}
		lineStart = len(s.frame)
	}
}

// nextFrameLine splits off the first line of rest, keeping its newline.
func nextFrameLine(rest []byte) (line, remainder []byte) {
	if i := bytes.IndexByte(rest, '\n'); i >= 0 {
		return rest[:i+1], rest[i+1:]
	}
	return rest, nil
}

// transform rewrites the frame's folded data payload when it carries a model
// field for this protocol. Unchanged frames return the original bytes. A
// rewritten frame is laid out as: the non-data lines in their original order,
// one "data: " line per payload line, and a blank line when the frame had
// one or ended with a newline.
func (s *sseModelStream) transform(frame []byte) []byte {
	var single []byte // the content of a sole data line, used in place
	dataLines := 0
	headers := false
	trailingBlank := len(frame) > 0 && frame[len(frame)-1] == '\n'
	for rest := frame; len(rest) > 0; {
		var line []byte
		line, rest = nextFrameLine(rest)
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			trailingBlank = true
		case bytes.HasPrefix(trimmed, []byte("data:")):
			content := bytes.TrimSpace(trimmed[len("data:"):])
			switch dataLines {
			case 0:
				single = content
			case 1:
				s.payload = append(append(append(s.payload[:0], single...), '\n'), content...)
			default:
				s.payload = append(append(s.payload, '\n'), content...)
			}
			dataLines++
		default:
			headers = true
		}
	}
	payload := single
	if dataLines > 1 {
		payload = s.payload
	}
	if dataLines == 0 || len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return frame
	}
	switch s.proto {
	case Anthropic:
		if !mayHoldJSONKey(payload, `"message"`) {
			return frame
		}
		rewritten, ok := spliceNestedModelValue(payload, "message", s.model)
		if !ok {
			return frame
		}
		// message_start is the only anthropic frame carrying a model; the
		// rest of the stream no longer needs scanning.
		s.raw = true
		s.rewritten = append(s.rewritten[:0], rewritten...)
	case Responses:
		if !mayHoldJSONKey(payload, `"response"`) {
			return frame
		}
		rewritten, ok := spliceNestedModelValue(payload, "response", s.model)
		if !ok {
			return frame
		}
		s.rewritten = append(s.rewritten[:0], rewritten...)
	default: // OpenAI
		if !mayHoldJSONKey(payload, `"model"`) {
			return frame
		}
		start, end, ok := locateTopLevelStringValue(payload, "model")
		if !ok {
			return frame
		}
		s.rewritten = append(append(append(s.rewritten[:0], payload[:start]...), s.encoded...), payload[end:]...)
	}

	out := s.rendered[:0]
	if headers {
		for rest := frame; len(rest) > 0; {
			var line []byte
			line, rest = nextFrameLine(rest)
			if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) > 0 && !bytes.HasPrefix(trimmed, []byte("data:")) {
				out = append(out, line...)
			}
		}
	}
	for rest := s.rewritten; ; {
		line := rest
		i := bytes.IndexByte(rest, '\n')
		if i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		}
		out = append(out, "data: "...)
		out = append(out, line...)
		out = append(out, '\n')
		if i < 0 {
			break
		}
	}
	if trailingBlank {
		out = append(out, '\n')
	}
	s.rendered = out
	return out
}

// mayHoldJSONKey reports whether payload can contain a member whose key
// decodes exactly to the quoted name: either the name appears literally, or
// an escape could spell it. The decoder walks match keys exactly (no case
// folding), so a payload with neither cannot be rewritten.
func mayHoldJSONKey(payload []byte, quoted string) bool {
	return bytes.Contains(payload, []byte(quoted)) || bytes.Contains(payload, []byte(`\u`))
}

// spliceTopLevelStringValue replaces the value of the FIRST top-level key
// named key when that value is a JSON string, splicing the original bytes so
// everything around the value is preserved exactly. ok=false for any other
// layout (key absent, non-string value, malformed JSON before it).
func spliceTopLevelStringValue(body []byte, key, value string) (out []byte, ok bool) {
	start, end, ok := locateTopLevelStringValue(body, key)
	if !ok {
		return nil, false
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, false
	}
	out = make([]byte, 0, len(body)-(end-start)+len(encoded))
	out = append(out, body[:start]...)
	out = append(out, encoded...)
	out = append(out, body[end:]...)
	return out, true
}

// locateTopLevelStringValue returns the byte range of the string value of the
// first top-level member named key. The semantics are the decoder walk's
// (decodeTopLevelStringValue); the byte-level scan answers whenever it can do
// so exactly and defers to the decoder otherwise.
func locateTopLevelStringValue(body []byte, key string) (start, end int, ok bool) {
	if start, end, ok, decided := scanTopLevelStringValue(body, key); decided {
		return start, end, ok
	}
	return decodeTopLevelStringValue(body, key)
}

// decodeTopLevelStringValue is the authoritative walk: a json.Decoder token
// walk of the top-level object that decodes each key (escapes included) and
// compares it exactly; earlier members' values must decode, the matched value
// must be a JSON string, and nothing after it is examined.
func decodeTopLevelStringValue(body []byte, key string) (start, end int, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return 0, 0, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		name, _ := keyTok.(string)
		if name != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return 0, 0, false
			}
			continue
		}
		valueStart := jsonValueStartAfterKey(dec, body)
		if valueStart < 0 || valueStart >= len(body) || body[valueStart] != '"' {
			return 0, 0, false
		}
		valTok, err := dec.Token()
		if err != nil {
			return 0, 0, false
		}
		if _, isString := valTok.(string); !isString {
			return 0, 0, false
		}
		return valueStart, int(dec.InputOffset()), true
	}
	return 0, 0, false
}

// scanTopLevelStringValue is the decoder walk's fast path over the members
// real upstream payloads carry before "model": keys without escapes and
// scalar values (escape- and control-free strings, numbers, true/false/null).
// Whenever it reaches a verdict (decided) the verdict is exactly the decoder
// walk's; anything outside that grammar met before the verdict — an escape, a
// container, a malformed token — leaves the decision to the decoder.
func scanTopLevelStringValue(body []byte, key string) (start, end int, ok, decided bool) {
	i := skipJSONSpace(body, 0)
	if i >= len(body) || body[i] != '{' {
		return 0, 0, false, false
	}
	i = skipJSONSpace(body, i+1)
	if i < len(body) && body[i] == '}' {
		return 0, 0, false, true // empty object: no such member
	}
	for {
		keyEnd, simple := scanSimpleJSONString(body, i)
		if !simple {
			return 0, 0, false, false
		}
		name := body[i+1 : keyEnd-1]
		i = skipJSONSpace(body, keyEnd)
		if i >= len(body) || body[i] != ':' {
			return 0, 0, false, false
		}
		i = skipJSONSpace(body, i+1)
		if i >= len(body) {
			return 0, 0, false, false
		}
		if string(name) == key {
			if body[i] != '"' {
				return 0, 0, false, true // the first such member is not a string
			}
			valueEnd, simple := scanSimpleJSONString(body, i)
			if !simple {
				return 0, 0, false, false
			}
			return i, valueEnd, true, true
		}
		next, scalar := skipSimpleJSONScalar(body, i)
		if !scalar {
			return 0, 0, false, false
		}
		i = skipJSONSpace(body, next)
		if i >= len(body) {
			return 0, 0, false, false
		}
		switch body[i] {
		case ',':
			i = skipJSONSpace(body, i+1)
		case '}':
			return 0, 0, false, true // walked the whole object: no such member
		default:
			return 0, 0, false, false
		}
	}
}

func skipJSONSpace(body []byte, i int) int {
	for i < len(body) && isNormalizeJSONSpace(body[i]) {
		i++
	}
	return i
}

// scanSimpleJSONString returns the index just past the string token at i,
// with simple=false when there is no string at i, it holds an escape or a raw
// control byte (the decoder judges those), or it is unterminated.
func scanSimpleJSONString(body []byte, i int) (end int, simple bool) {
	if i >= len(body) || body[i] != '"' {
		return 0, false
	}
	for j := i + 1; j < len(body); j++ {
		switch c := body[j]; {
		case c == '"':
			return j + 1, true
		case c == '\\' || c < 0x20:
			return 0, false
		}
	}
	return 0, false
}

// skipSimpleJSONScalar returns the index just past a simple scalar value at
// i (see scanSimpleJSONString; numbers per the JSON grammar; the three
// literals). Containers and anything else report scalar=false.
func skipSimpleJSONScalar(body []byte, i int) (next int, scalar bool) {
	switch c := body[i]; {
	case c == '"':
		return scanSimpleJSONString(body, i)
	case c == '-' || (c >= '0' && c <= '9'):
		return scanJSONNumber(body, i)
	case bytes.HasPrefix(body[i:], []byte("true")):
		return i + len("true"), true
	case bytes.HasPrefix(body[i:], []byte("false")):
		return i + len("false"), true
	case bytes.HasPrefix(body[i:], []byte("null")):
		return i + len("null"), true
	}
	return 0, false
}

// scanJSONNumber consumes one number per the JSON grammar
// -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)?; the caller rejects any byte
// after it other than whitespace, ',' or '}' (so "01" is not a number).
func scanJSONNumber(body []byte, i int) (next int, ok bool) {
	isDigit := func(c byte) bool { return c >= '0' && c <= '9' }
	if i < len(body) && body[i] == '-' {
		i++
	}
	switch {
	case i >= len(body):
		return 0, false
	case body[i] == '0':
		i++
	case body[i] >= '1' && body[i] <= '9':
		for i < len(body) && isDigit(body[i]) {
			i++
		}
	default:
		return 0, false
	}
	if i < len(body) && body[i] == '.' {
		i++
		if i >= len(body) || !isDigit(body[i]) {
			return 0, false
		}
		for i < len(body) && isDigit(body[i]) {
			i++
		}
	}
	if i < len(body) && (body[i] == 'e' || body[i] == 'E') {
		i++
		if i < len(body) && (body[i] == '+' || body[i] == '-') {
			i++
		}
		if i >= len(body) || !isDigit(body[i]) {
			return 0, false
		}
		for i < len(body) && isDigit(body[i]) {
			i++
		}
	}
	return i, true
}

// spliceNestedModelValue replaces objectKey.model where objectKey is a
// top-level key whose value is a JSON object holding a string model —
// anthropic's message_start (message.model) and the responses snapshots
// (response.model). Bytes outside the model value are preserved exactly.
func spliceNestedModelValue(body []byte, objectKey, model string) (out []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		name, _ := keyTok.(string)
		if name != objectKey {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, false
		}
		if len(raw) == 0 || raw[0] != '{' {
			return nil, false
		}
		end := int(dec.InputOffset())
		start := end - len(raw)
		inner, ok := spliceTopLevelStringValue(raw, "model", model)
		if !ok {
			return nil, false
		}
		out = make([]byte, 0, len(body)-len(raw)+len(inner))
		out = append(out, body[:start]...)
		out = append(out, inner...)
		out = append(out, body[end:]...)
		return out, true
	}
	return nil, false
}

// jsonValueStartAfterKey locates the first byte of the value for the key the
// decoder just returned: InputOffset sits just past the key's closing quote,
// so skip whitespace, the colon and following whitespace. -1 on any surprise.
func jsonValueStartAfterKey(dec *json.Decoder, body []byte) int {
	pos := int(dec.InputOffset())
	for pos < len(body) && isNormalizeJSONSpace(body[pos]) {
		pos++
	}
	if pos >= len(body) || body[pos] != ':' {
		return -1
	}
	pos++
	for pos < len(body) && isNormalizeJSONSpace(body[pos]) {
		pos++
	}
	return pos
}

func isNormalizeJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
