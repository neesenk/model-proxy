package protocol

import (
	"bufio"
	"bytes"
	"encoding/json"
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
// never drops or reorders stream content.
func NormalizeSSEModelStream(source io.ReadCloser, proto Protocol, model string) io.ReadCloser {
	if model == "" {
		return source
	}
	switch proto {
	case Anthropic, OpenAI, Responses:
	default:
		return source
	}
	return &sseModelStream{source: source, reader: bufio.NewReader(source), proto: proto, model: model}
}

type sseModelStream struct {
	source io.ReadCloser
	reader *bufio.Reader
	proto  Protocol
	model  string
	out    []byte // transformed bytes not yet consumed by Read
	raw    bool   // anthropic fast-forward: message_start already rewritten
	eof    bool
	err    error // sticky source read error, returned once out drains
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
// dispatching line). A partial frame at EOF is returned with io.EOF; a source
// error mid-frame returns what was read plus the error.
func (s *sseModelStream) readFrame() ([]byte, error) {
	var frame []byte
	for {
		line, err := s.reader.ReadBytes('\n')
		frame = append(frame, line...)
		if err != nil {
			return frame, err
		}
		if trimmed := bytes.TrimRight(line, "\r\n"); len(trimmed) == 0 {
			return frame, nil
		}
	}
}

// transform rewrites the frame's folded data payload when it carries a model
// field for this protocol. Unchanged frames return the original bytes.
func (s *sseModelStream) transform(frame []byte) []byte {
	var headers []byte
	var payload []byte
	dataSeen := false
	trailingBlank := false
	for _, line := range bytes.SplitAfter(frame, []byte("\n")) {
		trimmed := bytes.TrimRight(line, "\r\n")
		switch {
		case len(trimmed) == 0:
			trailingBlank = true
		case bytes.HasPrefix(trimmed, []byte("data:")):
			content := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
			if dataSeen {
				payload = append(payload, '\n')
			}
			payload = append(payload, content...)
			dataSeen = true
		default:
			headers = append(headers, line...)
		}
	}
	if !dataSeen || len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return frame
	}
	var rewritten []byte
	var ok bool
	switch s.proto {
	case Anthropic:
		rewritten, ok = spliceNestedModelValue(payload, "message", s.model)
		if ok {
			// message_start is the only anthropic frame carrying a model; the
			// rest of the stream no longer needs scanning.
			s.raw = true
		}
	case Responses:
		rewritten, ok = spliceNestedModelValue(payload, "response", s.model)
	default: // OpenAI
		rewritten, ok = spliceTopLevelStringValue(payload, "model", s.model)
	}
	if !ok {
		return frame
	}
	out := make([]byte, 0, len(headers)+len(rewritten)+8)
	out = append(out, headers...)
	out = append(out, "data: "...)
	out = append(out, rewritten...)
	out = append(out, '\n')
	if trailingBlank {
		out = append(out, '\n')
	}
	return out
}

// spliceTopLevelStringValue replaces the value of the FIRST top-level key
// named key when that value is a JSON string, splicing the original bytes so
// everything around the value is preserved exactly. ok=false for any other
// layout (key absent, non-string value, malformed JSON).
func spliceTopLevelStringValue(body []byte, key, value string) (out []byte, ok bool) {
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
		if name != key {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return nil, false
			}
			continue
		}
		valueStart := jsonValueStartAfterKey(dec, body)
		if valueStart < 0 || valueStart >= len(body) || body[valueStart] != '"' {
			return nil, false
		}
		valTok, err := dec.Token()
		if err != nil {
			return nil, false
		}
		if _, isString := valTok.(string); !isString {
			return nil, false
		}
		valueEnd := int(dec.InputOffset())
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, false
		}
		out = make([]byte, 0, len(body)-(valueEnd-valueStart)+len(encoded))
		out = append(out, body[:valueStart]...)
		out = append(out, encoded...)
		out = append(out, body[valueEnd:]...)
		return out, true
	}
	return nil, false
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
