package app

import (
	"bytes"
	"fmt"
	"io"
	"log"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerbuild"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/provider"
)

// Performance tests: isolate proxy forwarding overhead with a mock upstream, no
// dependency on the real AQP gateway. The AQP benchmarks use a mock mint server
// plus a temp SSO cookie file, so no real credential or network is touched.

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
	routes := map[string][]configdomain.RouteTarget{}
	for alias, real := range modelMap {
		if !seen[real] {
			seen[real] = true
			provModels = append(provModels, real)
		}
		routes[alias] = []configdomain.RouteTarget{{Provider: "t", Model: real}}
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"t": {OpenAIBaseURL: upstreamURL, Provider: testProviderID, Models: provModels},
		},
		Routes: routes,
		// The forward benchmarks measure rewrite/forward overhead; the secret
		// guard scans the full body on every request (its own cost belongs to
		// its own guard benchmarks). A zero-value Guard config defaults to
		// "log" (scan on), which would silently dominate the LargeBody case.
		Guard: configdomain.GuardConfig{Secrets: "off"},
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
// model is written as the FIRST key — the layout real LLM clients emit (same
// assumption as protocol.ExtractModel); a map[string]any marshal would sort it
// last and unrepresentatively force RewriteModel's fallback path.
func largeBody() []byte {
	pad := bytes.Repeat([]byte("x"), 60*1024)
	b := []byte(`{"model":"claude-opus-4-7","max_tokens":100,"messages":[{"role":"user","content":"`)
	b = append(b, pad...)
	b = append(b, `"}]}`...)
	return b
}

// --- benchmarks ---

// BenchmarkProxy_Forward_NoMap: small body, forward with NO model rewrite
// (identity model_map: target model == called model, so RewriteModel is a
// passthrough). A nil model_map would configure NO route at all and measure
// the no-route error path instead of a real forward.
func BenchmarkProxy_Forward_NoMap(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	px := newProxyServer(b, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
	px := newProxyServer(b, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
	pxp, px := newProxyServerP(b, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
	pxp, px := newProxyServerP(b, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
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
	pxp, px := newProxyServerP(b, up.URL, "static", map[string]string{"claude-opus-4-7": "claude-opus-4-7"})
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
		// A non-200 means the benchmark is measuring an error path (e.g. no
		// matching route), not a real forward — fail loudly instead of
		// reporting meaningless numbers.
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// BenchmarkProxy_Forward_Guard_LargeBody: clean ~64KB body with the guard
// fully on (secrets=log + known secrets + decode + paths) — every request
// pays the literal prefilter, so this sizes its cost on a large clean body.
// Compare against BenchmarkProxy_Forward_WithMap_LargeBody (guard off).
func BenchmarkProxy_Forward_Guard_LargeBody(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"t": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"claude-opus-4-7"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"claude-opus-4-7": {{Provider: "t", Model: "claude-opus-4-7"}}},
		Guard:  configdomain.GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true, Paths: "log"},
	}
	p := newTestProxy(b, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			b.Fatalf("forward returned %d", resp.StatusCode)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// TestProxy_Guard_CleanBodyScanBudget guards the guard: clean-body scanning
// runs on EVERY request, so the literal prefilter must keep it cheap. The
// guard package measures ~0.3ms per 64KB body; the budget (race-aware, see
// race_budget_*_test.go) keeps >10x headroom so a loaded CI machine cannot
// flake.
func TestProxy_Guard_CleanBodyScanBudget(t *testing.T) {
	sc, err := buildGuardScanner(&configdomain.Config{Guard: configdomain.GuardConfig{KnownSecrets: true, Decode: true}},
		[]providerbuild.Secret{{Value: "poolkey-" + strings.Repeat("wX9q", 8), Label: "pool:test#1/api_key"}})
	if err != nil {
		t.Fatal(err)
	}
	body := largeBody()
	const scans = 20
	start := time.Now()
	for i := 0; i < scans; i++ {
		if names := sc.Scan(body); len(names) != 0 {
			t.Fatalf("clean body secret hit: %v", names)
		}
		if cats := sc.ScanPaths(body); len(cats) != 0 {
			t.Fatalf("clean body path hit: %v", cats)
		}
		// The context-aware scan must short-circuit on the same literal gate:
		// a clean body never pays the JSON structure walk.
		if strong, weak := sc.ScanPathsContext(body); len(strong) != 0 || len(weak) != 0 {
			t.Fatalf("clean body path-context hit: %v/%v", strong, weak)
		}
	}
	if elapsed := time.Since(start); elapsed > guardScanBudget {
		t.Errorf("guard clean-body scan: %d x 64KB took %s, budget %s", scans, elapsed, guardScanBudget)
	}
}

// BenchmarkAuthInject_AQP_ColdMint: AQP provider cache-miss path — every
// iteration re-mints through the mock mint server (Refresh clears the cache),
// complementing the Cached benchmark's per-request hot path.
func BenchmarkAuthInject_AQP_ColdMint(b *testing.B) {
	silenceLog(b)
	mint := newUpstream(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"retcode":0,"data":{"api_key":"cold-key-0123456789abcdef","project_id":"p"}}`)
	})
	defer mint.Close()
	dir := b.TempDir()
	cookiePath := dir + "/cookie.json"
	cookieFile := `{"sso_session_cookie":"SSO_C=fake"}`
	if err := os.WriteFile(cookiePath, []byte(cookieFile), 0o600); err != nil {
		b.Fatal(err)
	}
	p := provider.NewAqpKeyProvider(mint.URL, cookiePath)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.Refresh(); err != nil {
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
	if err := os.WriteFile(cookiePath, []byte(cookieFile), 0o600); err != nil {
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

// claudeCodeBody builds a Claude-Code-shaped Anthropic Messages request: a
// system prompt, a tool table, then `turns` assistant tool_use / user
// tool_result round-trips carrying source code. This is the shape whose
// body-wide passes (guard scan, request profile, role counts) a uniform
// padding body hides — structure, quotes and identifiers drive their cost.
// model is the first key, as real clients write it.
func claudeCodeBody(turns int) []byte {
	const code = "func (p pipeline) serveOnce(req serveRequest, st *serveState) serveResult {\n" +
		"\tordered, decision := p.svc.Schedule(req.runtime.Cfg, req.exposed, req.sessionKey)\n" +
		"\tfor index, target := range ordered {\n\t\tplan, err := planTarget(req.runtime, target, req.proto)\n" +
		"\t\tif err != nil {\n\t\t\treturn serveResult{planningErr: true}\n\t\t}\n\t\t_, _, _ = index, plan, decision\n\t}\n" +
		"\treturn serveResult{} // 中文注释：转发管线\n}\n"
	var b bytes.Buffer
	b.WriteString(`{"model":"claude-opus-4-7","max_tokens":32000,"stream":false,"system":[{"type":"text","text":`)
	fmt.Fprintf(&b, "%q", strings.Repeat("Follow the repository rules and cite file_path:line_number. ", 200))
	b.WriteString(`}],"tools":[`)
	for i := 0; i < 24; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"name":"Tool%d","description":%q,"input_schema":{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"}},"required":["file_path"]}}`,
			i, strings.Repeat("Perform one workspace operation with the documented parameters. ", 8))
	}
	b.WriteString(`],"messages":[{"role":"user","content":[{"type":"text","text":"analyze the forward pipeline"}]}`)
	for t := 0; t < turns; t++ {
		fmt.Fprintf(&b, `,{"role":"assistant","content":[{"type":"text","text":"Reading step %d."},{"type":"tool_use","id":"toolu_%024d","name":"Read","input":{"file_path":"/work/repo/internal/forward/f%d.go","offset":%d}}]}`, t, t, t, t*10)
		fmt.Fprintf(&b, `,{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_%024d","content":%q}]}`, t, strings.Repeat(code, 2+t%3))
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// BenchmarkProxy_Forward_ClaudeCodeBody_1MB measures the request side on a
// realistic ~1MB coding-agent request through a same-protocol (anthropic)
// target: the default guard scan (secrets + paths), the request-log profile
// and role projection, and the capture. The upstream answers a tiny JSON, so
// the body-wide passes dominate.
func BenchmarkProxy_Forward_ClaudeCodeBody_1MB(b *testing.B) {
	silenceLog(b)
	up := newUpstream(jsonOK)
	defer up.Close()
	p := newTestProxy(b, &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"t": {AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"claude-opus-4-7"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"claude-opus-4-7": {{Provider: "t", Model: "claude-opus-4-7"}}},
		Guard:  configdomain.GuardConfig{Secrets: "log", KnownSecrets: true, Decode: true, Paths: "log"},
	})
	logger := requestlog.New(requestlog.Options{Directory: b.TempDir(), MaxFileSize: 1 << 30, MaxBodyBytes: 5 << 20})
	go logger.Run()
	defer logger.Shutdown()
	p.reqLog = logger
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	body := claudeCodeBody(260)
	cli := &http.Client{Timeout: 10 * time.Second}

	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Contains(answer, []byte(`"pong"`)) {
			b.Fatalf("forward = (%d, %q), want the upstream's 200 answer", resp.StatusCode, answer)
		}
	}
}

// BenchmarkProxy_Forward_SSE_AliasedLongStream measures the dominant
// production response path: a long openai chat stream (3000 chunks) whose
// route target model differs from the exposed name, so every chunk passes
// the model normalizer, the terminal watcher, the usage scanner and the
// request-log capture (aggregated on write, request_log.aggregate_streams).
func BenchmarkProxy_Forward_SSE_AliasedLongStream(b *testing.B) {
	silenceLog(b)
	var stream bytes.Buffer
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&stream, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-1017\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok%d \"},\"finish_reason\":null}]}\n\n", i)
	}
	stream.WriteString("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"up-1017\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":3000}}\n\ndata: [DONE]\n\n")
	up := newUpstream(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write(stream.Bytes())
	})
	defer up.Close()
	p := newTestProxy(b, &configdomain.Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]configdomain.Provider{
			"t": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"up-1017"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"alias-model": {{Provider: "t", Model: "up-1017"}}},
	})
	logger := requestlog.New(requestlog.Options{
		Directory: b.TempDir(), MaxFileSize: 1 << 30, MaxBodyBytes: 5 << 20,
		AggregateStream: aggregateLoggedStream(true),
	})
	go logger.Run()
	defer logger.Shutdown()
	p.reqLog = logger
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	body := []byte(`{"model":"alias-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	cli := &http.Client{Timeout: 30 * time.Second}

	b.SetBytes(int64(stream.Len()))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		resp, err := cli.Post(px.URL+"/v1/chat/completions", "application/json", bytes.NewReader(body))
		if err != nil {
			b.Fatal(err)
		}
		answer, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b.Fatalf("forward returned %d", resp.StatusCode)
		}
		if i == 0 && (bytes.Count(answer, []byte(`"model":"alias-model"`)) != 3001 || bytes.Contains(answer, []byte("up-1017"))) {
			b.Fatalf("client stream not normalized to the exposed model: %.300q", answer)
		}
	}
}
