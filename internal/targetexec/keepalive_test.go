package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// frameWriter captures every client-bound write (in order) so heartbeat tests
// can wait on frames as events instead of sleeping.
type frameWriter struct {
	http.ResponseWriter
	frames chan string
}

func (writer *frameWriter) Write(p []byte) (int, error) {
	n, err := writer.ResponseWriter.Write(p)
	// Capture AFTER the underlying write so receiving the frame synchronizes
	// with anything the write stamped (e.g. the ttft first-byte marker).
	writer.frames <- string(p)
	return n, err
}

func (writer *frameWriter) WriteHeartbeat(p []byte) (int, error) {
	var n int
	var err error
	heartbeat, ok := writer.ResponseWriter.(interface {
		WriteHeartbeat([]byte) (int, error)
	})
	if ok {
		n, err = heartbeat.WriteHeartbeat(p)
	} else {
		n, err = writer.ResponseWriter.Write(p)
	}
	writer.frames <- string(p)
	return n, err
}

func waitFrame(t *testing.T, frames chan string) string {
	t.Helper()
	select {
	case frame := <-frames:
		return frame
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a client frame")
		return ""
	}
}

func TestFlushCopyHeartbeatEmitsHeartbeatDuringSilence(t *testing.T) {
	pipeReader, pipeWriter := io.Pipe()
	recorder := httptest.NewRecorder()
	timed := newTimingResponseWriter(recorder)
	writer := &frameWriter{ResponseWriter: timed, frames: make(chan string, 32)}
	end := make(chan streamEnd, 1)
	go func() { end <- flushCopyHeartbeat(writer, pipeReader, pipeReader, 10*time.Millisecond) }()

	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("silence produced %q, want a heartbeat frame", frame)
	}
	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("silence produced %q, want a second heartbeat frame", frame)
	}
	if timed.hasFirstByte {
		t.Fatal("heartbeat stamped the ttft first-byte marker")
	}
	wroteAt := time.Now()
	if _, err := pipeWriter.Write([]byte("data: x\n\n")); err != nil {
		t.Fatal(err)
	}
	if frame := waitFrame(t, writer.frames); frame != "data: x\n\n" {
		t.Fatalf("frame = %q, want the real data", frame)
	}
	if !timed.hasFirstByte || timed.firstByte.Before(wroteAt) {
		t.Fatalf("ttft marker = %v at %v, want stamped by the real first byte (>= %v)",
			timed.hasFirstByte, timed.firstByte, wroteAt)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if got := <-end; got != streamEOF {
		t.Fatalf("end = %v, want clean EOF", got)
	}
	body := recorder.Body.String()
	if !strings.HasPrefix(body, ": ping\n\n") || !strings.HasSuffix(body, "data: x\n\n") {
		t.Fatalf("client body = %q, want heartbeats then data", body)
	}
}

func TestFlushCopyHeartbeatCleanEOFWithoutData(t *testing.T) {
	recorder := httptest.NewRecorder()
	body := io.NopCloser(strings.NewReader(""))
	if got := flushCopyHeartbeat(recorder, body, body, time.Hour); got != streamEOF {
		t.Fatalf("end = %v, want clean EOF", got)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}

// Data resets the silence clock: heartbeat -> data -> silence must produce
// another heartbeat AFTER the data (the ticker.Reset path), not only before.
func TestFlushCopyHeartbeatResetsTickerOnData(t *testing.T) {
	pipeReader, pipeWriter := io.Pipe()
	recorder := httptest.NewRecorder()
	writer := &frameWriter{ResponseWriter: recorder, frames: make(chan string, 32)}
	end := make(chan streamEnd, 1)
	go func() { end <- flushCopyHeartbeat(writer, pipeReader, pipeReader, 10*time.Millisecond) }()

	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("initial silence produced %q, want a heartbeat", frame)
	}
	if _, err := pipeWriter.Write([]byte("data: a\n\n")); err != nil {
		t.Fatal(err)
	}
	// Extra heartbeats may already be queued ahead of the data frame on a
	// slow scheduler; drain until the data arrives.
	for frame := waitFrame(t, writer.frames); frame != "data: a\n\n"; frame = waitFrame(t, writer.frames) {
		if frame != ": ping\n\n" {
			t.Fatalf("unexpected frame %q before the data", frame)
		}
	}
	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("post-data silence produced %q, want a heartbeat after the data", frame)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if got := <-end; got != streamEOF {
		t.Fatalf("end = %v, want clean EOF", got)
	}
	body := recorder.Body.String()
	dataAt := strings.Index(body, "data: a\n\n")
	if dataAt <= 0 || !strings.Contains(body[dataAt:], ": ping\n\n") {
		t.Fatalf("client body = %q, want heartbeats both before and after the data", body)
	}
}

// An upstream read can split an SSE event at any byte. A heartbeat appended
// behind a partial line would corrupt the client's reassembly, so ticks that
// land mid-line must be deferred until the frame completes.
func TestFlushCopyHeartbeatDefersToLineBoundary(t *testing.T) {
	pipeReader, pipeWriter := io.Pipe()
	recorder := httptest.NewRecorder()
	writer := &frameWriter{ResponseWriter: recorder, frames: make(chan string, 32)}
	end := make(chan streamEnd, 1)
	go func() { end <- flushCopyHeartbeat(writer, pipeReader, pipeReader, 10*time.Millisecond) }()

	partial := "data: {\"cho"
	if _, err := pipeWriter.Write([]byte(partial)); err != nil {
		t.Fatal(err)
	}
	if frame := waitFrame(t, writer.frames); frame != partial {
		t.Fatalf("frame = %q, want the partial data chunk", frame)
	}
	// Stay silent well past several intervals: no heartbeat may be appended
	// to the half-written line.
	select {
	case frame := <-writer.frames:
		t.Fatalf("mid-frame silence produced %q, want the heartbeat deferred to a line boundary", frame)
	case <-time.After(200 * time.Millisecond):
	}
	rest := "\"}\n\n"
	if _, err := pipeWriter.Write([]byte(rest)); err != nil {
		t.Fatal(err)
	}
	if frame := waitFrame(t, writer.frames); frame != rest {
		t.Fatalf("frame = %q, want the rest of the split frame", frame)
	}
	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("aligned silence produced %q, want a heartbeat", frame)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if got := <-end; got != streamEOF {
		t.Fatalf("end = %v, want clean EOF", got)
	}
	body := recorder.Body.String()
	if stripped := strings.ReplaceAll(body, ": ping\n\n", ""); stripped != partial+rest {
		t.Fatalf("client body minus heartbeats = %q, want the original stream %q", stripped, partial+rest)
	}
	for offset := 0; ; {
		at := strings.Index(body[offset:], ": ping")
		if at < 0 {
			break
		}
		at += offset
		if at > 0 && body[at-1] != '\n' {
			t.Fatalf("heartbeat at offset %d is not at a line boundary: %q", at, body)
		}
		offset = at + len(": ping")
	}
}

// A heartbeat write failure (client gone) must unplug and drain the parked
// reader goroutine BEFORE flushCopyHeartbeat returns: the caller's post-copy
// tail then runs strictly after the recording chain went idle.
func TestFlushCopyHeartbeatWriteFailureReleasesReader(t *testing.T) {
	pipeReader, pipeWriter := io.Pipe()
	defer pipeWriter.Close()
	body := &readWatchBody{ReadCloser: pipeReader, returned: make(chan struct{}, 1)}
	end := make(chan streamEnd, 1)
	go func() { end <- flushCopyHeartbeat(failingResponseWriter{}, body, pipeReader, 10*time.Millisecond) }()
	if got := <-end; got != streamClientGone {
		t.Fatalf("end = %v, want client gone", got)
	}
	// Regression: the caller's post-copy tail (body.Close, counter reads,
	// usage commit) must never race a Read unwinding through the recording
	// wrappers. flushCopyHeartbeat unplugs the parked Read itself (raw
	// source close) and drains the reader BEFORE returning — the unwind
	// signal must therefore already be observable here, without the caller
	// having to close the body first.
	select {
	case <-body.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("reader goroutine still parked after flushCopyHeartbeat returned")
	}
}

type readWatchBody struct {
	io.ReadCloser
	returned chan struct{}
}

func (body *readWatchBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if err != nil {
		select {
		case body.returned <- struct{}{}:
		default:
		}
	}
	return n, err
}

// readTapBody tees the recording chain's view of the response body so a test
// can assert heartbeat frames never enter it (request log / usage / cache all
// wrap this read side).
type readTapBody struct {
	io.ReadCloser
	sink *bytes.Buffer
}

func (body *readTapBody) Read(p []byte) (int, error) {
	n, err := body.ReadCloser.Read(p)
	if n > 0 {
		body.sink.Write(p[:n])
	}
	return n, err
}

type tapEffects struct {
	executorEffects
	captured bytes.Buffer
}

func (effects *tapEffects) CaptureResponse(body io.ReadCloser, _ AttemptDTO) io.ReadCloser {
	return &readTapBody{ReadCloser: body, sink: &effects.captured}
}

// End-to-end through Execute: a slow-TTFT upstream stream emits SSE comment
// heartbeats to the client, while ttft still measures the real first byte and
// neither the recording chain nor the cache ever sees a heartbeat frame.
func TestExecutorStreamKeepaliveHeartbeat(t *testing.T) {
	provider := &executorTestProvider{}
	pipeReader, pipeWriter := io.Pipe()
	recorder := httptest.NewRecorder()
	writer := &frameWriter{ResponseWriter: recorder, frames: make(chan string, 64)}
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        provider,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	cache := responsecache.New(responsecache.Options{
		TTL: time.Minute, MaxEntries: 4, MaxBodyBytes: 1024,
	})
	runtime := Runtime{
		Scheduling: configdomain.Scheduling{StreamKeepalive: "10ms"},
		Cache:      cache,
	}
	attempt := NewAttempt(
		runtime,
		plan,
		Exchange{
			Request: request,
			Writer:  writer,
			Body:    []byte(`{"model":"model","stream":true,"messages":[]}`),
		},
		Scope{CacheKey: "cache-key", CalledModel: "model"},
		Policy{LastTarget: true},
	)
	response := testResponse(http.StatusOK, "")
	response.Header.Set("Content-Type", "text/event-stream")
	response.Body = pipeReader
	effects := &tapEffects{}
	result := make(chan Result, 1)
	go func() {
		result <- (Executor{
			Client:  &sequenceDoer{responses: []*http.Response{response}},
			State:   &executorState{},
			Effects: effects,
		}).Execute(attempt)
	}()

	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("first client frame = %q, want a heartbeat", frame)
	}
	if frame := waitFrame(t, writer.frames); frame != ": ping\n\n" {
		t.Fatalf("second client frame = %q, want a heartbeat", frame)
	}
	stream := "data: {\"id\":\"c1\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	if _, err := pipeWriter.Write([]byte(stream)); err != nil {
		t.Fatal(err)
	}
	if err := pipeWriter.Close(); err != nil {
		t.Fatal(err)
	}
	res := <-result
	if !res.Committed {
		t.Fatalf("result = %+v", res)
	}
	if effects.commits != 1 {
		t.Fatalf("commits = %d, want 1", effects.commits)
	}
	// Two observed heartbeats at a 10ms interval mean the real first byte came
	// no earlier than ~20ms in; a heartbeat-stamped ttft would read ~10ms.
	if got := effects.lastCommit.TTFTMilliseconds; got < 15 {
		t.Fatalf("ttft = %dms, want the real first byte (>= 15ms)", got)
	}
	if !bytes.Contains(effects.captured.Bytes(), []byte("data: [DONE]")) ||
		bytes.Contains(effects.captured.Bytes(), []byte(": ping")) {
		t.Fatalf("recording chain saw %q, want the stream without heartbeats", effects.captured.String())
	}
	entry, ok := cache.Lookup("cache-key", "model", time.Now())
	if !ok {
		t.Fatal("complete stream was not cached")
	}
	if bytes.Contains(entry.Body(), []byte(": ping")) {
		t.Fatalf("cached body contains a heartbeat frame: %q", entry.Body())
	}
	clientBody := recorder.Body.String()
	if !strings.HasPrefix(clientBody, ": ping\n\n") || !strings.HasSuffix(clientBody, stream) {
		t.Fatalf("client body = %q, want heartbeats then the passthrough stream", clientBody)
	}
}

// A non-stream response must never see a comment frame, even with the
// keepalive configured.
func TestExecutorKeepaliveSkipsNonStream(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	runtime := attempt.Runtime()
	runtime.Scheduling = configdomain.Scheduling{StreamKeepalive: "1ms"}
	attempt = NewAttempt(runtime, attempt.Plan(), attempt.Exchange(), attempt.Scope(), attempt.Policy())
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{testResponse(200, `{"ok":true}`)}},
		State:  &executorState{},
	}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if got := writer.Body.String(); got != `{"ok":true}` {
		t.Fatalf("non-stream body = %q, want untouched JSON", got)
	}
}

// Heartbeat frames add bytes beyond the upstream body, so a keepalive stream
// must not forward the upstream Content-Length; without keepalive the
// zero-copy passthrough keeps it.
func TestExecutorKeepaliveStripsUpstreamContentLength(t *testing.T) {
	stream := "data: {\"id\":\"c1\",\"model\":\"model\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	run := func(t *testing.T, keepalive string) *httptest.ResponseRecorder {
		provider := &executorTestProvider{}
		attempt, writer := testAttempt(provider, `{"model":"model","stream":true,"messages":[]}`, Policy{LastTarget: true})
		runtime := attempt.Runtime()
		runtime.Scheduling = configdomain.Scheduling{StreamKeepalive: keepalive}
		attempt = NewAttempt(runtime, attempt.Plan(), attempt.Exchange(), attempt.Scope(), attempt.Policy())
		response := testResponse(http.StatusOK, stream)
		response.Header.Set("Content-Type", "text/event-stream")
		response.Header.Set("Content-Length", "999")
		result := (Executor{
			Client: &sequenceDoer{responses: []*http.Response{response}},
			State:  &executorState{},
		}).Execute(attempt)
		if !result.Committed {
			t.Fatalf("result = %+v", result)
		}
		if got := writer.Body.String(); got != stream {
			t.Fatalf("client body = %q, want the passthrough stream", got)
		}
		return writer
	}
	if got := run(t, "10ms").Header().Get("Content-Length"); got != "" {
		t.Fatalf("keepalive stream forwarded Content-Length %q, want it stripped", got)
	}
	if got := run(t, "0").Header().Get("Content-Length"); got != "999" {
		t.Fatalf("passthrough without keepalive lost Content-Length: got %q, want the upstream value", got)
	}
}
