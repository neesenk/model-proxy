package mcp

import (
	"bufio"
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
// event (tracked by event name, per the SSE event model).
func (e *EndpointRewriter) maybeRewrite(line []byte) []byte {
	if e.done {
		return line
	}
	trimmed := strings.TrimSpace(string(line))
	if strings.HasPrefix(trimmed, "event:") {
		e.await = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:")) == "endpoint"
		return line
	}
	if !e.await || !strings.HasPrefix(trimmed, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
	out := e.rewrite(payload)
	e.done = true
	e.await = false
	return []byte("data: " + out + "\n")
}
