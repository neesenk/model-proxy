package targetexec

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const contextOverflowPeek = 64 << 10

func isSSE(header http.Header) bool {
	for _, contentType := range header.Values("content-type") {
		if strings.Contains(contentType, "text/event-stream") {
			return true
		}
	}
	return false
}

func peekResponseBody(response *http.Response, limit int) []byte {
	peek, _ := io.ReadAll(io.LimitReader(response.Body, int64(limit)))
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), response.Body), response.Body}
	return peek
}

// peekHead performs exactly ONE bounded Read from the response body and
// replays what it returned: the commit path still sees a complete byte
// stream. One read — never a fill loop — is what makes it safe on the SUCCESS
// path: a partial first chunk (an upstream that streams, or blocks mid-answer
// waiting for the client) is a complete input for a byte-0 prefix verdict,
// and waiting for more bytes would deadlock a passthrough stream whose
// upstream is blocked until the client acks. A 0-byte read (nothing buffered
// yet, or EOF/error) means "undecided", never "empty body": the caller
// treats undecided as not-a-verdict and proceeds to commit.
func peekHead(response *http.Response, limit int) []byte {
	buf := make([]byte, limit)
	n, _ := response.Body.Read(buf)
	head := buf[:n]
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), response.Body), response.Body}
	return head
}

func sniffSSEFraming(response *http.Response) bool {
	const window = 16
	buf := make([]byte, window)
	n, err := response.Body.Read(buf)
	for n < window && err == nil && sniffUndecided(buf[:n]) {
		m, readErr := response.Body.Read(buf[n:])
		n += m
		err = readErr
		if m == 0 && readErr == nil {
			break
		}
	}
	peek := buf[:n]
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), response.Body), response.Body}
	peek = bytes.TrimSpace(peek)
	if bytes.HasPrefix(peek, []byte(":")) {
		return true
	}
	for _, marker := range [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:")} {
		if bytes.HasPrefix(peek, marker) {
			return true
		}
	}
	return false
}

func sniffUndecided(peek []byte) bool {
	peek = bytes.TrimSpace(peek)
	if len(peek) == 0 {
		return true
	}
	for _, marker := range [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:")} {
		if len(peek) < len(marker) && bytes.HasPrefix(marker, peek) {
			return true
		}
	}
	return false
}

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, key := range keys {
		// Values + Add, not Get + Set: whitelisted headers like anthropic-beta
		// are legal multi-valued, and collapsing them to the first value
		// silently drops beta-capability declarations.
		for _, value := range src.Values(key) {
			// A value with control bytes can never be transmitted — net/http
			// refuses the whole upstream request, turning one mangled client
			// header (a session id with embedded NUL, say) into a 502. Skip
			// the value instead: an invalid identity travels as absent, the
			// same fail-identifiable rule the routing sticky key applies.
			if value != "" && isTransmittableHeaderValue(value) {
				dst.Add(key, value)
			}
		}
	}
}

// isTransmittableHeaderValue reports whether net/http can send the value:
// no control characters (field-content per RFC 9110 §5.5 — bytes 0x21–0x7E
// plus SP and HTAB; HTAB is excluded here as header values carrying it have
// no legitimate consumer and trimming ambiguity is worse than dropping).
func isTransmittableHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if b := value[i]; b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

type streamEnd int

const (
	streamEOF streamEnd = iota
	streamClientGone
	streamUpstreamErr
	// streamWriteStalled: the client connection's write deadline fired — the
	// peer kept its connection but stopped reading, so a Write that began
	// before the deadline was still blocked when it expired. Like
	// streamClientGone this is not an upstream fault; it is distinct because
	// the connection owner did nothing (no close, no reset).
	streamWriteStalled
)

// streamWriteGrace caps how far CLIENT writes may run past the request's
// upstream-timeout deadline. That deadline is absolute and shared by the two
// sides of the exchange: the upstream Read fails at it, and whatever was in
// flight is handed to the client right after — a strict bound would cut that
// tail (and the fail-closed terminal the watcher synthesizes for it) off a
// perfectly healthy reader. The grace is the window that tail gets; a stalled
// peer is abandoned when it runs out. It never exceeds the budget itself, so
// a short upstream_timeout keeps the bound proportionally tight.
const streamWriteGrace = 5 * time.Second

// clientWriteDeadline is the deadline of a response's client writes: the
// request's upstream-timeout deadline plus the tail grace. budget is the
// span from the attempt's dispatch to that deadline (the configured timeout,
// less whatever a same-target retry spent).
func clientWriteDeadline(upstreamDeadline time.Time, budget time.Duration) time.Time {
	grace := min(streamWriteGrace, budget)
	if grace < 0 {
		grace = 0
	}
	return upstreamDeadline.Add(grace)
}

// isWriteStall reports whether a client write error is the connection's
// write deadline firing (net.OpError around os.ErrDeadlineExceeded, or the
// same error surfacing later from a sticky bufio failure).
func isWriteStall(err error) bool {
	return err != nil && errors.Is(err, os.ErrDeadlineExceeded)
}

// clientWriteBound applies one response's client-write deadline to the
// connection. The deadline itself comes from the executor's response handle
// (timingResponseWriter.writeDeadline, set by commit from the request's
// upstream-timeout context); http.ResponseController reaches the connection
// through the wrappers' Unwrap chain. A writer that cannot take a deadline
// (recorders, test fakes) leaves the controller nil: its writes stay
// unbounded exactly as before and only the error identity classifies a
// stall.
type clientWriteBound struct {
	controller *http.ResponseController
	deadline   time.Time
}

func resolveClientWriteBound(writer http.ResponseWriter) clientWriteBound {
	timed, ok := writer.(*timingResponseWriter)
	if !ok || timed.writeDeadline.IsZero() {
		return clientWriteBound{}
	}
	return clientWriteBound{controller: http.NewResponseController(timed), deadline: timed.writeDeadline}
}

// arm sets the deadline before a client write; flush pushes buffered bytes
// and returns the error http.Flusher.Flush swallows (a timed-out write
// surfaces here first); clear drops the deadline again when the copy ends.
// net/http also clears a connection's write deadline when the request ends,
// so a kept-alive connection is safe either way — clearing here keeps the
// bound scoped to the copy should anything else ever write to the client
// after it.
func (bound clientWriteBound) arm() {
	if bound.controller != nil {
		_ = bound.controller.SetWriteDeadline(bound.deadline)
	}
}

func (bound clientWriteBound) flush(flusher http.Flusher) error {
	if bound.controller == nil || flusher == nil {
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	return bound.controller.Flush()
}

func (bound clientWriteBound) clear() {
	if bound.controller != nil {
		_ = bound.controller.SetWriteDeadline(time.Time{})
	}
}

// clientEnd classifies a failed client write.
func (bound clientWriteBound) clientEnd(err error) streamEnd {
	if isWriteStall(err) {
		return streamWriteStalled
	}
	return streamClientGone
}

type countingReadCloser struct {
	io.ReadCloser
	Count int64
	// Reads counts distinct Read calls that returned bytes — the arrival
	// shape the committed generation-window classification keys on (a body
	// spread over many reads streamed; 1-3 reads arrived in bulk).
	Reads int
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	reader.Count += int64(count)
	if count > 0 {
		reader.Reads++
	}
	return count, err
}

// flushBufPool recycles the 32KB streaming copy buffer: one fresh allocation
// per forwarded response was the proxy's largest flat allocation source under
// the forward benchmarks.
var flushBufPool = sync.Pool{New: func() any {
	buf := make([]byte, 32<<10)
	return &buf
}}

// flushCopy streams body to the client. Client writes are bounded by the
// response's write deadline when the writer carries one (see clientWriteBound):
// a peer that keeps its connection but stops reading used to park this loop
// in Write forever, out of reach of the upstream-timeout context.
func flushCopy(writer http.ResponseWriter, body io.ReadCloser) streamEnd {
	flusher, _ := writer.(http.Flusher)
	bound := resolveClientWriteBound(writer)
	defer bound.clear()
	bufp := flushBufPool.Get().(*[]byte)
	defer flushBufPool.Put(bufp)
	buf := *bufp
	for {
		n, err := body.Read(buf)
		if n > 0 {
			bound.arm()
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return bound.clientEnd(writeErr)
			}
			if flushErr := bound.flush(flusher); isWriteStall(flushErr) {
				return streamWriteStalled
			}
		}
		if err != nil {
			if err == io.EOF {
				return streamEOF
			}
			return streamUpstreamErr
		}
	}
}

// heartbeatFrame is an SSE comment line: a protocol-level no-op that every
// compliant SSE parser (openai / anthropic / responses SDKs) skips. Written
// only into client-facing event streams to reset client/edge idle timers
// while a slow-TTFT upstream is still silent.
var heartbeatFrame = []byte(": ping\n\n")

// flushCopyHeartbeat is flushCopy plus a keepalive ticker: whenever the
// upstream body stays silent for interval, a heartbeat comment frame goes to
// the client. Body reads run on a helper goroutine so the ticker can fire
// during a blocked Read; only the calling goroutine writes to the client.
// Heartbeats bypass the recording chain (they are written to the client,
// never read from the body) and the ttft marker (via WriteHeartbeat).
//
// source is the RAW upstream body at the bottom of body's wrapper chain (the
// recording wrappers sit between the two). On a heartbeat write failure the
// parked helper Read is unplugged by closing source — NOT body: the close
// then reaches the parked Read from below and every wrapper's post-Read
// logic unwinds on the reader goroutine alone, while the caller's later
// full-chain body.Close runs strictly after this function returns (the
// reader is drained first). Closing the wrapper chain instead would run its
// Close-side effects (usage commit, capture callback) concurrently with the
// same wrappers unwinding on the reader goroutine.
func flushCopyHeartbeat(writer http.ResponseWriter, body, source io.ReadCloser, interval time.Duration) streamEnd {
	flusher, _ := writer.(http.Flusher)
	heartbeatWriter, hasHeartbeat := writer.(interface {
		WriteHeartbeat([]byte) (int, error)
	})
	bound := resolveClientWriteBound(writer)
	defer bound.clear()
	writeHeartbeat := func() error {
		bound.arm()
		if hasHeartbeat {
			_, err := heartbeatWriter.WriteHeartbeat(heartbeatFrame)
			return err
		}
		_, err := writer.Write(heartbeatFrame)
		return err
	}
	bufp := flushBufPool.Get().(*[]byte)
	buf := *bufp
	type readResult struct {
		n   int
		err error
	}
	readReq := make(chan struct{})
	readRes := make(chan readResult, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range readReq {
			n, err := body.Read(buf)
			readRes <- readResult{n, err}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	recycle := true
	defer func() {
		close(readReq)
		// ALWAYS drain the reader before returning: the caller's post-copy
		// tail (body.Close, counter/recorder reads, usage commit) must never
		// race a Read still unwinding through the recording wrappers. Every
		// return path guarantees the goroutine exits promptly — a parked Read
		// is unplugged by the source close in the heartbeat-failure branch
		// before this defer runs.
		<-readerDone
		if recycle {
			flushBufPool.Put(bufp)
		}
	}()
	// At most one read request is outstanding: the reader goroutine parks in
	// body.Read until the upstream produces bytes, and re-sending the request
	// before that read completes would deadlock the loop.
	readReq <- struct{}{}
	// A heartbeat is a comment LINE: an upstream read can split an SSE event
	// at any byte, and appending ": ping\n\n" behind a partial line corrupts
	// the client's reassembly ("data: {\"cho: ping"). Ticks that land
	// mid-line are skipped; the ticker keeps firing, so the heartbeat is
	// merely deferred until the frame completes (or the stream ends).
	lineAligned := true
	for {
		select {
		case result := <-readRes:
			if result.n > 0 {
				bound.arm()
				if _, err := writer.Write(buf[:result.n]); err != nil {
					return bound.clientEnd(err)
				}
				if flushErr := bound.flush(flusher); isWriteStall(flushErr) {
					return streamWriteStalled
				}
				lineAligned = buf[result.n-1] == '\n'
				ticker.Reset(interval)
			}
			if result.err != nil {
				if result.err == io.EOF {
					return streamEOF
				}
				return streamUpstreamErr
			}
			readReq <- struct{}{}
		case <-ticker.C:
			if !lineAligned {
				continue
			}
			writeErr := writeHeartbeat()
			// The heartbeat is a few bytes and lands in the response buffer;
			// when the peer stopped reading the deadline failure is the
			// Flush's — which http.Flusher.Flush cannot report but the bound's
			// flush can.
			flushErr := error(nil)
			if writeErr == nil {
				flushErr = bound.flush(flusher)
			}
			if writeErr != nil || isWriteStall(flushErr) {
				// The reader goroutine may still be parked in body.Read holding
				// buf; the buffer must NOT return to the pool — abandon it to
				// the GC. Close the RAW source (never the wrapper chain, see
				// the function comment) so the parked Read unwinds and the
				// drain in the deferred cleanup completes; the caller's own
				// body.Close afterwards is an idempotent second close.
				recycle = false
				_ = source.Close()
				if writeErr != nil {
					return bound.clientEnd(writeErr)
				}
				return streamWriteStalled
			}
		}
	}
}

type timingResponseWriter struct {
	http.ResponseWriter
	firstByte    time.Time
	hasFirstByte bool
	flusher      http.Flusher
	// writeDeadline, when set by commit, is the absolute deadline of this
	// response's client writes: the request's upstream-timeout deadline plus
	// streamWriteGrace. The copy loops reach it through clientWriteBound.
	writeDeadline time.Time
}

func newTimingResponseWriter(writer http.ResponseWriter) *timingResponseWriter {
	flusher, _ := writer.(http.Flusher)
	return &timingResponseWriter{ResponseWriter: writer, flusher: flusher}
}

// Unwrap lets http.ResponseController reach the connection through this
// wrapper — without it a client-write deadline could not be set at all.
func (writer *timingResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *timingResponseWriter) Write(body []byte) (int, error) {
	if !writer.hasFirstByte {
		writer.hasFirstByte = true
		writer.firstByte = time.Now()
	}
	return writer.ResponseWriter.Write(body)
}

// WriteHeartbeat writes a client-facing keepalive frame WITHOUT recording the
// first byte: TTFT measures the upstream's first real byte, not proxy padding.
func (writer *timingResponseWriter) WriteHeartbeat(frame []byte) (int, error) {
	return writer.ResponseWriter.Write(frame)
}

func (writer *timingResponseWriter) Flush() {
	if writer.flusher != nil {
		writer.flusher.Flush()
	}
}

// FlushError is Flush with the error http.Flusher.Flush cannot return; the
// controller prefers it, so a timed-out client write surfaces at the flush
// instead of on the next Write.
func (writer *timingResponseWriter) FlushError() error {
	return http.NewResponseController(writer.ResponseWriter).Flush()
}
