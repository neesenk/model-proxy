package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
)

// TestForward_RequestLog_AnthropicTerminalUsageDialect: a passthrough
// anthropic stream whose message_delta replaces the figures message_start
// reported (step-plan's placeholder split, recorded from the 2026-11-24
// serve logs) reads as one ledger everywhere the request is accounted — the
// live token counter and the request-log record — and, now that the usage
// projection agrees with the stream fold, is stored as its aggregated JSON.
// The usage-equality gate used to keep every such stream raw and index the
// placeholder cache_read instead (94,668 cache-read tokens that day where the
// terminal frames reported 60,160).
func TestForward_RequestLog_AnthropicTerminalUsageDialect(t *testing.T) {
	const model = "deepseek-v4.1-flash" // the fixture's own model name: nothing to normalize
	stream, err := os.ReadFile(filepath.Join("..", "..", "testdata", "wire", "anthropic_step-plan_tool.sse"))
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write(stream)
	}))
	defer upstream.Close()
	proxy := newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"backend": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			model: {{Provider: "backend", Model: model, Protocol: "anthropic"}},
		},
	})
	proxy.providers["backend"] = &testProv{key: "test-token"}
	dir := t.TempDir()
	proxy.initRequestLog(configdomain.RequestLogConfig{Enabled: true, Dir: dir, AggregateStreams: true})
	proxy.reqLogStarted = proxy.lifecycle.Run(func(<-chan struct{}) { proxy.reqLog.Run() })
	proxy.reqLogIndexStarted = proxy.lifecycle.Run(func(<-chan struct{}) { proxy.reqLogIndex.Run() })
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	defer server.Close()

	code, clientBody := post(t, server.URL+"/v1/messages",
		`{"model":"`+model+`","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK || clientBody != string(stream) {
		t.Fatalf("client = (%d, %q), want the upstream stream byte-for-byte", code, clientBody)
	}
	awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "backend", Model: model})
	proxy.Close() // drains the request logger

	// The live counter: message_delta's split, not message_start's placeholder.
	key := counters.TokenKey{Provider: "backend", Model: model}
	if got, want := proxy.tokens.Snapshot()[key], (counters.TokenUsage{Input: 5670, Output: 672, CacheRead: 4864, Requests: 1}); got != want {
		t.Errorf("committed tokens = %+v, want %+v", got, want)
	}
	records := allRecords(t, dir)
	if len(records) != 1 {
		t.Fatalf("request log record count = %d, want 1", len(records))
	}
	record := records[0]
	if !record.ResponseAggregated || record.ResponseSize != int64(len(stream)) {
		t.Fatalf("record aggregated=%v size=%d, want aggregated with wire size %d", record.ResponseAggregated, record.ResponseSize, len(stream))
	}
	if got, want := requestlog.ExtractUsage(record.ResponseBody), (requestlog.Usage{Input: 5670, Output: 672, CacheRead: 4864}); got != want {
		t.Errorf("usage projected from the logged record = %+v, want %+v", got, want)
	}
}
