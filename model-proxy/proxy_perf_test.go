package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Performance tests: isolate proxy forwarding overhead with a mock upstream, no
// dependency on the real Compass gateway. auth uses static_key (CQPProvider
// static path makes no network call), testing proxy logic purely.

// silenceLog mutes forward's per-request log so the bench isn't slowed/spammed by log IO.
func silenceLog() { log.SetOutput(io.Discard) }

// newUpstream returns a mock upstream; h decides response behavior.
func newUpstream(h http.HandlerFunc) *httptest.Server {
	return httptest.NewServer(h)
}

// newProxyServer wraps a Proxy with the given auth + model_map and returns a hitable httptest server.
func newProxyServer(upstreamURL, auth string, modelMap map[string]string) *httptest.Server {
	// Build provider models from the model_map (alias→real).
	provModels := map[string]ProviderModel{}
	routeModels := map[string]string{}
	for alias, real := range modelMap {
		provModels[real] = ProviderModel{Context: 200000, Output: 32768}
		routeModels[alias] = "t/" + real
	}
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"t": {BaseURL: upstreamURL, Provider: "static", Models: provModels},
		},
		Routes: map[string]ProtocolRoute{
			"anthropic": {Models: routeModels},
		},
	}
	p := NewProxy(cfg)
	return httptest.NewServer(http.HandlerFunc(p.handler))
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
	silenceLog()
	up := newUpstream(jsonOK)
	defer up.Close()
	px := newProxyServer(up.URL, "static", nil)
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
	silenceLog()
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(up.URL, "static", mm)
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
	silenceLog()
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(up.URL, "static", mm)
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
	silenceLog()
	up := newUpstream(sseOK)
	defer up.Close()
	px := newProxyServer(up.URL, "static", nil)
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

// BenchmarkAuthInject_CQP_Static: CQP provider static-key injection (the proxy's per-request hot path).
func BenchmarkAuthInject_CQP_Static(b *testing.B) {
	silenceLog()
	p := newCQPProvider("", "")
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

// BenchmarkAuthInject_CQP_Cached: CQP provider cache hit (goes through the mutex).
// Uses a mock mint server + a temp cookie file; Refresh once, then bench Inject (cache hit).
func BenchmarkAuthInject_CQP_Cached(b *testing.B) {
	silenceLog()
	mint := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"cached-key-0123456789abcdef","project_id":"p"}}`)
	})
	defer mint.Close()
	// Temp cookie file.
	dir := b.TempDir()
	cookiePath := dir + "/cookie.json"
	cookieFile := `{"sso_session_cookie":"SSO_C=fake"}`
	if err := writeFile(cookiePath, []byte(cookieFile), 0o600); err != nil {
		b.Fatal(err)
	}
	p := newCQPProvider(mint.URL, cookiePath)
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
	silenceLog()
	up := newUpstream(jsonOK)
	defer up.Close()
	mm := map[string]string{"claude-opus-4-7": "glm-5.2"}
	px := newProxyServer(up.URL, "static", mm)
	defer px.Close()
	body := smallBody()

	const conc = 50
	const total = 2000
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
					io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
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

	if pErr != 0 {
		t.Errorf("proxy had %d errors", pErr)
	}
	// Proxy extra overhead: p99 should be within a reasonable order of magnitude (not orders worse).
	overhead := percentile(pLat, 0.99) - percentile(dLat, 0.99)
	t.Logf("proxy p99 overhead vs direct: %s", overhead)
}

// TestProxy_SSE_FirstByte: streaming first-byte latency — upstream returns chunks;
// verifies the proxy's flushCopy forwards the first chunk promptly.
func TestProxy_SSE_FirstByte(t *testing.T) {
	silenceLog()
	up := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		// Send the first chunk immediately, then 5ms between subsequent chunks, to verify the first byte isn't buffered.
		fmt.Fprintf(w, "data: {\"i\":0}\n\n")
		if fl != nil {
			fl.Flush()
		}
		for i := 1; i <= 3; i++ {
			time.Sleep(5 * time.Millisecond)
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
	px := newProxyServer(up.URL, "static", nil)
	defer px.Close()

	cli := &http.Client{Timeout: 10 * time.Second}
	s := time.Now()
	resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(smallBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// Time to read the first byte = first-byte latency (should be far below the total stream time of ~15ms).
	buf := make([]byte, 64)
	n, _ := resp.Body.Read(buf)
	firstByte := time.Since(s)
	t.Logf("first-byte latency: %s (got %d bytes: %q)", firstByte, n, string(buf[:n]))
	// Read the whole stream.
	io.Copy(io.Discard, resp.Body)
	total := time.Since(s)
	t.Logf("total stream latency: %s", total)

	if firstByte > 50*time.Millisecond {
		t.Errorf("first-byte latency too high: %s (stream buffering?)", firstByte)
	}
	// The first byte should clearly precede the total time (proving chunked passthrough, not buffering the whole stream).
	if firstByte >= total {
		t.Errorf("first-byte (%s) >= total (%s): proxy buffered whole stream", firstByte, total)
	}
}
