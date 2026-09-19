package mcp

import (
	"bufio"
	"bytes"
	"io"
	"net/url"
	"strings"
)

// sse.go — legacy HTTP+SSE transport (protocol 2024-11-05) helpers: the
// client GETs the SSE stream, the server names its POST endpoint in the first
// "endpoint" event, and the gateway rewrites that URL back to itself while
// binding both channels to one upstream session.

// ResolveEndpointURL resolves the data payload of a legacy-SSE endpoint event
// against the SSE stream's own URL (payloads may be relative). "" when the
// payload or base cannot be parsed.
func ResolveEndpointURL(sseURL, data string) string {
	rel, err := url.Parse(strings.TrimSpace(data))
	if err != nil {
		return ""
	}
	base, err := url.Parse(sseURL)
	if err != nil {
		return ""
	}
	return base.ResolveReference(rel).String()
}

// EndpointRewriter rewrites the data payload of the first "endpoint" event in
// a legacy-SSE stream, passing all other bytes through unchanged. The rewrite
// runs at most once (compliant streams open with the endpoint event; the
// rewriter stays lenient and simply never rewrites if none appears).
type EndpointRewriter struct {
	r       *bufio.Reader
	rewrite func(string) string
	pend    []byte
	done    bool
	await   bool // last line was "event: endpoint" — next data line is the URL
	eof     bool
}

// NewEndpointRewriter wraps src; rewrite receives the raw data payload and
// returns its replacement (called at most once, on the first endpoint event).
func NewEndpointRewriter(src io.Reader, rewrite func(string) string) *EndpointRewriter {
	return &EndpointRewriter{r: bufio.NewReader(src), rewrite: rewrite}
}

// Read serves the stream line-wise so SSE frame boundaries (and the client's
// event parser) survive the rewrite; after the endpoint event the stream is a
// plain passthrough. It returns as soon as one line is available — a
// streaming reader must never block trying to fill p (SSE streams stay open
// for minutes between events).
func (e *EndpointRewriter) Read(p []byte) (int, error) {
	if len(e.pend) == 0 && !e.eof {
		line, err := e.r.ReadBytes('\n')
		if len(line) > 0 {
			e.pend = append(e.pend, e.maybeRewrite(line)...)
		}
		if err != nil {
			e.eof = true
			if len(e.pend) == 0 {
				return 0, err
			}
		}
	}
	if len(e.pend) == 0 && e.eof {
		return 0, io.EOF
	}
	n := copy(p, e.pend)
	e.pend = e.pend[n:]
	return n, nil
}

// maybeRewrite applies the rewrite to the data line of the first "endpoint"
// event (tracked by event name, per the SSE event model). It preserves the
// original line terminator and the whitespace immediately after "data:" (the
// SSE field separator), replacing only the actual data value instead of
// trimming the whole line.
func (e *EndpointRewriter) maybeRewrite(line []byte) []byte {
	if e.done {
		return line
	}
	// Split off the terminator so we can reconstruct the line unchanged for
	// non-matching bytes and preserve \r\n vs \n for the rewritten line.
	body, term := line, []byte{}
	switch {
	case bytes.HasSuffix(body, []byte("\r\n")):
		body, term = body[:len(body)-2], []byte("\r\n")
	case bytes.HasSuffix(body, []byte("\n")):
		body, term = body[:len(body)-1], []byte("\n")
	}
	if bytes.HasPrefix(body, []byte("event:")) {
		eventValue := strings.TrimSpace(string(body[len("event:"):]))
		e.await = eventValue == "endpoint"
		return line
	}
	if !e.await || !bytes.HasPrefix(body, []byte("data:")) {
		return line
	}
	// Separate the SSE separator whitespace from the data value so the value
	// is what gets rewritten while the separator is preserved verbatim.
	rest := body[len("data:"):]
	value := bytes.TrimLeft(rest, " \t")
	separator := rest[:len(rest)-len(value)]
	out := e.rewrite(string(value))
	e.done = true
	e.await = false
	return append(append(append([]byte("data:"), separator...), []byte(out)...), term...)
}
