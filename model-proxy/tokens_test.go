package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUsageScannerAnthropic(t *testing.T) {
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":50,\"cache_read_input_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":200}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	key := tokenKey{Provider: "zhipu", Model: "glm-5"}
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), key, tc)
	io.Copy(io.Discard, sc)

	got := tc.snapshot()[key]
	if got.Input != 100 || got.CacheCreation != 50 || got.CacheRead != 10 || got.Output != 200 {
		t.Errorf("usage = %+v, want in=100 cc=50 cr=10 out=200", got)
	}
}

func TestUsageScannerOpenAI(t *testing.T) {
	stream := []byte("data: {\"id\":\"x\",\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":13}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "deepseek", Model: "d"}, tc)
	io.Copy(io.Discard, sc)
	got := tc.snapshot()[tokenKey{Provider: "deepseek", Model: "d"}]
	if got.Input != 7 || got.Output != 13 {
		t.Errorf("usage = %+v, want in=7 out=13", got)
	}
}

// Split every usage payload byte-by-byte to prove the scanner reassembles across
// arbitrarily small reads.
func TestUsageScannerSplitBoundaries(t *testing.T) {
	payload := []byte("data: {\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":99}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(&oneByteReader{b: payload}), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(io.Discard, sc)
	got := tc.snapshot()[tokenKey{Provider: "p", Model: "m"}]
	if got.Input != 42 || got.Output != 99 {
		t.Errorf("split-boundary usage = %+v, want in=42 out=99", got)
	}
}

// Pass-through must be byte-identical.
func TestUsageScannerPassthrough(t *testing.T) {
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\ndata: garbage\n\n")
	var sink bytes.Buffer
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(&sink, sc)
	if !bytes.Equal(sink.Bytes(), stream) {
		t.Errorf("passthrough not byte-identical:\nwant %q\ngot  %q", stream, sink.Bytes())
	}
}

// An oversized line is skipped for scanning but still passed through.
func TestUsageScannerOversizedLine(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), 80_000)
	stream := append([]byte("data: "), huge...)
	stream = append(stream, []byte("\n\ndata: {\"usage\":{\"prompt_tokens\":3}}\n\n")...)
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	var sink bytes.Buffer
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(&sink, sc)
	if !bytes.Equal(sink.Bytes(), stream) {
		t.Error("oversized passthrough mismatch")
	}
	if got := tc.snapshot()[tokenKey{Provider: "p", Model: "m"}].Input; got != 3 {
		t.Errorf("usage after oversized line = %d, want 3", got)
	}
}

func TestTokenCounterPersist(t *testing.T) {
	path := t.TempDir() + "/tokens.json"
	tc := newTokenCounter(path)
	tc.commit(tokenKey{Provider: "z", Model: "m"}, tokenUsage{Input: 10, Output: 20, Requests: 1})
	if err := tc.save(); err != nil {
		t.Fatal(err)
	}
	tc2 := newTokenCounter(path)
	if err := tc2.load(); err != nil {
		t.Fatal(err)
	}
	got := tc2.snapshot()[tokenKey{Provider: "z", Model: "m"}]
	if got.Input != 10 || got.Output != 20 || got.Requests != 1 {
		t.Errorf("persist round-trip failed: %+v", got)
	}
}

// oneByteReader yields one byte per Read to force split-boundary scanning.
type oneByteReader struct {
	b   []byte
	off int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.off]
	r.off++
	return 1, nil
}

// TestTokenCounterConcurrent would race under -race before the fix (the old
// commit mutated *tokenUsage fields after releasing tc.mu inside entry()). It
// spawns 50 concurrent committers to the SAME key plus a concurrent snapshot
// reader; after the fix every increment lands (no lost updates) and -race is
// clean. Final Input/Output must equal exactly the number of committers.
func TestTokenCounterConcurrent(t *testing.T) {
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	key := tokenKey{Provider: "p", Model: "m"}
	const committers = 50

	var snapDone sync.WaitGroup
	snapDone.Add(1)
	stopSnap := make(chan struct{})
	// Concurrent reader: hammers snapshot during commits. Before the fix this
	// read *tokenUsage fields while commit mutated them unlocked -> -race.
	go func() {
		defer snapDone.Done()
		for {
			select {
			case <-stopSnap:
				return
			default:
				_ = tc.snapshot()
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(committers)
	start := make(chan struct{})
	for i := 0; i < committers; i++ {
		go func() {
			defer wg.Done()
			<-start
			tc.commit(key, tokenUsage{Input: 1, Output: 1})
		}()
	}
	close(start) // release all committers together to maximize contention
	wg.Wait()
	close(stopSnap)
	snapDone.Wait()

	got := tc.snapshot()[key]
	if got.Input != committers {
		t.Errorf("Input = %d, want %d (lost increments)", got.Input, committers)
	}
	if got.Output != committers {
		t.Errorf("Output = %d, want %d (lost increments)", got.Output, committers)
	}
	if got.Requests != committers {
		t.Errorf("Requests = %d, want %d", got.Requests, committers)
	}
}

// TestForwardCountsTokens verifies the proxy forward hot path wraps SSE response
// bodies in a usageScanner keyed by the chosen (provider, model), committing
// observed anthropic usage (input_tokens from message_start, output_tokens from
// message_delta) to the shared tokenCounter. Non-SSE responses are not scanned.
// HOME is pinned to a temp dir (with a dummy zhipu apikey) so NewProxy's
// baseline load can't pick up the developer's real ~/.model-proxy/token_usage.json
// and the provider can authenticate against the mock upstream.
func TestForwardCountsTokens(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		w.Write(stream)
	}))
	defer up.Close()
	cfg, err := LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: glm-5}]\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	p := NewProxy(cfg)
	rec := httptest.NewRecorder()
	p.handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`)))
	io.Copy(io.Discard, rec.Result().Body)
	rec.Result().Body.Close()
	got := p.tokens.snapshot()[tokenKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 42 || got.Output != 8 {
		t.Errorf("tokens = %+v, want in=42 out=8", got)
	}
	if got.Requests != 1 {
		t.Errorf("requests = %d, want 1", got.Requests)
	}
}

// TestForwardDoesNotScanNonSSE verifies a non-SSE 2xx response is NOT wrapped:
// the counter must stay zero (no scanner overhead, no commit) for plain JSON.
func TestForwardDoesNotScanNonSSE(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: glm-5}]\n"))
	p := NewProxy(cfg)
	rec := httptest.NewRecorder()
	p.handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`)))
	io.Copy(io.Discard, rec.Result().Body)
	rec.Result().Body.Close()
	got := p.tokens.snapshot()[tokenKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 0 || got.Output != 0 || got.Requests != 0 {
		t.Errorf("non-SSE tokens = %+v, want zero (non-SSE must not be scanned)", got)
	}
}

// disconnectWriter wraps an httptest.ResponseRecorder and fails every Write,
// simulating a client that has already gone away (broken pipe). flushCopy must
// stop reading the upstream stream on the first failed write.
type disconnectWriter struct {
	*httptest.ResponseRecorder
}

func (disconnectWriter) Write([]byte) (int, error) {
	return 0, errors.New("client disconnected: broken pipe")
}

// TestForwardCommitsOnDisconnect verifies that when a client disconnects
// mid-stream (flushCopy's w.Write returns an error after the upstream has
// already delivered usage events), usage observed BEFORE the disconnect —
// notably input_tokens from message_start, which arrives at the START of the
// stream before any cancel — is still committed.
//
// Previously the inline usageScanner passed to flushCopy was never assigned,
// so resp.Body.Close() closed the underlying body and bypassed the scanner's
// Close → commit path, silently dropping observed usage. With the fix the
// wrapped body is bound to a variable and closed explicitly, firing the
// commit. On a normal (EOF) stream the scanner's Read already committed, so
// the explicit Close is a harmless no-op (no double-count) — covered by
// TestForwardCountsTokens above.
func TestForwardCommitsOnDisconnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".model-proxy", "zhipu_apikey.json"), []byte(`{"api_key":"sk-test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Upstream delivers both usage events, then HOLDS the stream open (err==nil
	// on the proxy's first Read) to simulate ongoing generation the client
	// cancels. This is the critical precondition: the scanner's Read-err commit
	// path must NOT fire (no error yet), so the only commit path is the
	// explicit body.Close() the fix adds.
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":42}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":8}}\n\n")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		w.Write(stream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold open until the proxy closes the body (disconnect)
	}))
	defer up.Close()
	cfg, err := LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: "+up.URL+"\nroutes:\n  m: [{provider: zhipu, model: glm-5}]\n"))
	if err != nil {
		t.Fatalf("LoadConfigFromBytes: %v", err)
	}
	p := NewProxy(cfg)
	rec := &disconnectWriter{ResponseRecorder: httptest.NewRecorder()}
	p.handler(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`)))
	got := p.tokens.snapshot()[tokenKey{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 42 {
		t.Errorf("input tokens after disconnect = %d, want 42 (observed usage must commit on client-cancel, not be silently dropped)", got.Input)
	}
	if got.Output != 8 {
		t.Errorf("output tokens after disconnect = %d, want 8", got.Output)
	}
	if got.Requests != 1 {
		t.Errorf("requests after disconnect = %d, want 1", got.Requests)
	}
}
