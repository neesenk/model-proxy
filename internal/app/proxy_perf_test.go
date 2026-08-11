package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
	"model-proxy/provider"
)

// Performance tests: isolate proxy forwarding overhead with a mock upstream, no
// dependency on the real AQP gateway. auth uses a stubbed AqpKeyProvider (empty
// mint URL -> no network call), testing proxy logic purely.

// silenceLog mutes forward's per-request log so the bench isn't slowed/spammed
// by log IO, then restores the process-global logger after the test/benchmark.
func silenceLog(t testing.TB) {
	t.Helper()
	previous := log.Writer()
	log.SetOutput(io.Discard)
	t.Cleanup(func() { log.SetOutput(previous) })
}

// newUpstream returns a mock upstream; h decides response behavior.
func newUpstream(h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(h)
}

// newProxyServer wraps a Proxy with the given auth + model_map and returns a hitable httptest.Server.
func newProxyServer(t testing.TB, upstreamURL, auth string, modelMap map[string]string) *httptest.Server {
	_, px := newProxyServerP(t, upstreamURL, auth, modelMap)
	return px
}

// newProxyServerP is newProxyServer but also returns the *Proxy, so callers can
// wire extra state (e.g. a request logger) onto it for benchmarking.
func newProxyServerP(t testing.TB, upstreamURL, auth string, modelMap map[string]string) (*Proxy, *httptest.Server) {
	seen := map[string]bool{}
	var provModels []string
	routes := map[string][]RouteTarget{}
	for alias, real := range modelMap {
		if !seen[real] {
			seen[real] = true
			provModels = append(provModels, real)
		}
		routes[alias] = []RouteTarget{{Provider: "t", Model: real}}
	}
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"t": {OpenAIBaseURL: upstreamURL, Provider: testProviderID, Models: provModels},
		},
		Routes: routes,
	}
	p := newTestProxy(t, cfg)
	return p, httptest.NewServer(http.HandlerFunc(p.Handler))
}

// --- mock upstream handlers ---

// jsonOK returns a fixed JSON response (non-streaming).
func jsonOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}]}`))
}

// sseOK returns 3 SSE event chunks (streaming, flush after each).
func sseOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	fl, _ := w.(http.Flusher)
	for i := 0; i < 3; i++ {
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"chunk\"}}\n\n")
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

// smallBody / largeBody build request bodies of different sizes.
func smallBody() []byte {
	return []byte(`{"model":"claude-opus-4-7","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}`)
}

// largeBody builds a ~64KB request body (large messages content), amplifying rewriteModel's json cost.
func largeBody() []byte {
	pad := bytes.Repeat([]byte("x"), 60*1024)
	v := map[string]any{
		"model":      "claude-opus-4-7",
		"max_tokens": 100,
		"messages": []map[string]any{
			{"role": "user", "content": string(pad)},
		},
	}
	b, _ := json.Marshal(v)
	return b
}

// --- benchmarks ---

// BenchmarkProxy_Forward_NoMap: small body, no model_map hit (rewriteModel not triggered).
func BenchmarkProxy_Forward_NoMap(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	px := newProxyServer(b, up.URL, "static", nil)
	defer px.Close()
	body := smallBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_WithMap: small body, model_map hit (triggers rewriteModel: unmarshal+marshal the whole body).
func BenchmarkProxy_Forward_WithMap(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(b, up.URL, "static", mm)
	defer px.Close()
	body := smallBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_WithMap_LargeBody: large body, model_map hit, amplifying rewriteModel's json cost.
func BenchmarkProxy_Forward_WithMap_LargeBody(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(b, up.URL, "static", mm)
	defer px.Close()
	body := largeBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_SSE: streaming (SSE) forwarding, 3 event chunks.
func BenchmarkProxy_Forward_SSE(b *testing.B) {
	silenceLog(b)
	up := newUpstream(sseOK)
	defer up.Close()
	px := newProxyServer(b, up.URL, "static", nil)
	defer px.Close()
	body := smallBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_RequestLog_SmallBody measures the hot-path overhead of
// per-request access logging on a small non-streaming response: bodycapture
// tee + record construction + non-blocking enqueue. The logger writes to a temp dir
// with a running loop, so the full pipeline (capture -> channel -> file write)
// is exercised. Compare against BenchmarkProxy_Forward_NoMap (logging disabled)
// to size the cost.
func BenchmarkProxy_Forward_RequestLog_SmallBody(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	pxp, px := newProxyServerP(b, up.URL, "static", nil)
	defer px.Close()
	// Wire a file-based request logger (running loop) onto the proxy.
	dir := b.TempDir()
	l := requestlog.New(requestlog.Options{
		Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 1 << 20,
	})
	go l.Run()
	defer l.Shutdown()
	pxp.reqLog = l
	body := smallBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_RequestLog_LargeBody measures logging overhead with a
// large response body, where the bodycapture tee cost (per-byte memory copy
// into the bounded buffer) dominates. The upstream returns a ~64KB body.
func BenchmarkProxy_Forward_RequestLog_LargeBody(b *testing.B) {
	silenceLog(b)
	up := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write(bytes.Repeat([]byte("y"), 64*1024))
	})
	defer up.Close()
	pxp, px := newProxyServerP(b, up.URL, "static", nil)
	defer px.Close()
	dir := b.TempDir()
	l := requestlog.New(requestlog.Options{
		Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 1 << 20,
	})
	go l.Run()
	defer l.Shutdown()
	pxp.reqLog = l
	body := smallBody()
	cli := &http.Client{Timeout: 10 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_RequestLog_1MB measures logging overhead at the
// max_body_bytes scale (1MB response): the bodycapture reader copies 1MB into
// the bounded buffer per request. Sizes the per-byte copy cost that dominates
// for large streaming responses.
func BenchmarkProxy_Forward_RequestLog_1MB(b *testing.B) {
	silenceLog(b)
	up := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write(bytes.Repeat([]byte("y"), 1024*1024))
	})
	defer up.Close()
	pxp, px := newProxyServerP(b, up.URL, "static", nil)
	defer px.Close()
	dir := b.TempDir()
	l := requestlog.New(requestlog.Options{
		Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 1 << 20,
	})
	go l.Run()
	defer l.Shutdown()
	pxp.reqLog = l
	body := smallBody()
	cli := &http.Client{Timeout: 30 * time.Second}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkAuthInject_AQP_Static: AQP provider static-key injection (the proxy's per-request hot path).
func BenchmarkAuthInject_AQP_Static(b *testing.B) {
	silenceLog(b)
	p := provider.NewAqpKeyProvider("", "")
	req, _ := http.NewRequest(http.MethodPost, "http://up/v1/messages", bytes.NewReader(smallBody()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Header.Del("Authorization")
		if err := p.Inject(req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuthInject_AQP_Cached: AQP provider cache hit (goes through the mutex).
// Uses a mock mint server + a temp cookie file; Refresh once, then bench Inject (cache hit).
func BenchmarkAuthInject_AQP_Cached(b *testing.B) {
	silenceLog(b)
	mint := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"cached-key-0123456789abcdef","project_id":"p"}}`)
	})
	defer mint.Close()
	// Temp cookie file.
	dir := b.TempDir()
	cookiePath := dir + "/cookie.json"
	cookieFile := `{"sso_session_cookie":"SSO_C=fake"}`
	if err := cliframework.WriteFile(cookiePath, []byte(cookieFile), 0o600); err != nil {
		b.Fatal(err)
	}
	p := provider.NewAqpKeyProvider(mint.URL, cookiePath)
	// Warm up: mint once to fill the cache.
	if err := p.Refresh(); err != nil {
		b.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, "http://up/v1/messages", bytes.NewReader(smallBody()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req.Header.Del("Authorization")
		if err := p.Inject(req); err != nil {
			b.Fatal(err)
		}
	}
}

// --- concurrent load test (with latency distribution and a direct-connection baseline) ---

// TestProxy_Load_Concurrent hammers the proxy concurrently, reporting QPS, latency
// distribution (p50/p95/p99), and error rate; compared against a direct hit to the
// mock upstream to isolate the proxy's extra overhead.
func TestProxy_Load_Concurrent(t *testing.T) {
	silenceLog(t)
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(t, up.URL, "static", mm)
	defer px.Close()
	body := smallBody()

	const conc = 50
	const total = 500
	perWorker := total / conc

	run := func(target string) (latencies []time.Duration, errCount int64, elapsed time.Duration) {
		cli := &http.Client{Timeout: 15 * time.Second}
		results := make([][]time.Duration, conc)
		var wg sync.WaitGroup
		wg.Add(conc)
		start := time.Now()
		for w := 0; w < conc; w++ {
			go func(w int) {
				defer wg.Done()
				local := make([]time.Duration, 0, perWorker)
				for i := 0; i < perWorker; i++ {
					s := time.Now()
					resp, err := cli.Post(target+"/v1/messages", "application/json", bytes.NewReader(body))
					if err != nil {
						atomic.AddInt64(&errCount, 1)
						continue
					}
					_, copyErr := io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
					if copyErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
						atomic.AddInt64(&errCount, 1)
						continue
					}
					local = append(local, time.Since(s))
				}
				results[w] = local
			}(w)
		}
		wg.Wait()
		elapsed = time.Since(start)
		for _, l := range results {
			latencies = append(latencies, l...)
		}
		return
	}

	percentile := func(lats []time.Duration, p float64) time.Duration {
		if len(lats) == 0 {
			return 0
		}
		sort.Slice(lats, func(i, j int) bool { return lats[i] < lats[j] })
		idx := int(float64(len(lats)) * p)
		if idx >= len(lats) {
			idx = len(lats) - 1
		}
		return lats[idx]
	}

	// Direct hit to the mock upstream (baseline).
	dLat, dErr, dElapsed := run(up.URL)
	// Via the proxy.
	pLat, pErr, pElapsed := run(px.URL)

	report := func(label string, lats []time.Duration, errCount int64, elapsed time.Duration) {
		ok := int64(len(lats))
		qps := float64(ok) / elapsed.Seconds()
		t.Logf("[%s] ok=%d err=%d  elapsed=%s  QPS=%.0f  lat p50=%s p95=%s p99=%s max=%s",
			label, ok, errCount, elapsed, qps,
			percentile(lats, 0.50), percentile(lats, 0.95), percentile(lats, 0.99),
			percentile(lats, 1.0))
	}

	report("direct(upstream)", dLat, dErr, dElapsed)
	report("via proxy     ", pLat, pErr, pElapsed)

	if dErr != 0 || int64(len(dLat)) != total {
		t.Errorf("direct baseline: successes=%d errors=%d, want %d/0", len(dLat), dErr, total)
	}
	if pErr != 0 || int64(len(pLat)) != total {
		t.Errorf("proxy: successes=%d errors=%d, want %d/0", len(pLat), pErr, total)
	}
	// Proxy extra overhead: p99 should be within a reasonable order of magnitude (not orders worse).
	overhead := percentile(pLat, 0.99) - percentile(dLat, 0.99)
	t.Logf("proxy p99 overhead vs direct: %s", overhead)
}

// TestProxy_SSE_FirstByte: streaming first-byte latency — upstream returns chunks;
// verifies the proxy's flushCopy forwards the first chunk promptly.
func TestProxy_SSE_FirstByte(t *testing.T) {
	silenceLog(t)
	release := make(chan struct{})
	firstFlushed := make(chan struct{})
	up := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Send the first chunk, then block until the client proves it received it.
		fmt.Fprintf(w, "data: {\"i\":0}\n\n")
		if fl != nil {
			fl.Flush()
		}
		close(firstFlushed)
		<-release
		for i := 1; i <= 3; i++ {
			fmt.Fprintf(w, "data: {\"i\":%d}\n\n", i)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		if fl != nil {
			fl.Flush()
		}
	})
	defer up.Close()
	px := newProxyServer(t, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
	defer px.Close()

	cli := &http.Client{Timeout: 2 * time.Second}
	resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(smallBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("SSE status=%d want 200; body=%q", resp.StatusCode, body)
	}

	<-firstFlushed
	// The upstream is blocked after its first flush. A successful read therefore
	// proves the proxy forwarded the partial stream instead of buffering to EOF.
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if err != nil {
		close(release)
		t.Fatalf("read first SSE chunk: %v", err)
	}
	if n == 0 || !bytes.Contains(buf[:n], []byte(`"i":0`)) {
		close(release)
		t.Fatalf("first SSE read = %q, want first event", buf[:n])
	}
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(rest, []byte("data: [DONE]")) {
		t.Errorf("remaining SSE stream missing DONE: %q", rest)
	}
}
