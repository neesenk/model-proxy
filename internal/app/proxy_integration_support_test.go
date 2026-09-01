package app

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/counters"
	"model-proxy/internal/provider"
)

// usecase_test.go organizes tests by user-facing use case (end-to-end through
// the proxy), rather than by function. Each test drives a real HTTP request
// through p.Handler against httptest upstreams and asserts the observable
// outcome a user cares about. Shared helpers (testProv, post, stringReader,
// newCaptureUpstream) live in proxy_routing_test.go / routes_test.go.

// --- mock helpers specific to use cases ---

// scriptedUpstream responds with a sequence of (status, body) per request,
// letting a single server simulate "first 401 then 200 after refresh".
func scriptedUpstream(scripts ...respScript) *httptest.Server {
	var idx int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(atomic.AddInt32(&idx, 1)) - 1
		if i >= len(scripts) {
			i = len(scripts) - 1
		}
		s := scripts[i]
		if s.headers != nil {
			for k, v := range s.headers {
				w.Header().Set(k, v)
			}
		}
		if s.status != 200 {
			w.WriteHeader(s.status)
		}
		w.Write([]byte(s.body))
	}))
}

type respScript struct {
	status  int
	body    string
	headers map[string]string
}

// recordingProv is a testProv that records calls to AuthHeaders/Refresh, so a
// test can assert the 401-refresh-retry path actually invoked Refresh.
type recordingProv struct {
	testProv
	authCalls    int32
	refreshCalls int32
}

func (p *recordingProv) AuthHeaders(req *http.Request) error {
	atomic.AddInt32(&p.authCalls, 1)
	return p.testProv.AuthHeaders(req)
}
func (p *recordingProv) Refresh() error {
	atomic.AddInt32(&p.refreshCalls, 1)
	return nil
}

// newProxyWithStatic builds a Proxy whose providers are all testProv with the
// given keys, so tests don't hit real auth files.
func newProxyWithStatic(t testing.TB, cfg *Config, keys map[string]string) *Proxy {
	p := newTestProxy(t, cfg)
	for name, key := range keys {
		p.providers[name] = &testProv{key: key}
	}
	return p
}

// fakeAuth is a provider.Authenticator that injects a Bearer key, for building
// real provider implementations (AqpProvider/CodexProvider) in tests
// without reading auth files.
type fakeAuth struct{ key string }

func (a fakeAuth) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+a.key)
	req.Header.Del("x-api-key")
	return nil
}
func (a fakeAuth) Refresh() error { return nil }

// mustRealProvider builds a real provider implementation by provider_id (so its
// RewriteRequest/AuthHeaders logic is exercised), wired with the given cfg.
func mustRealProvider(t *testing.T, providerID string, cfg *provider.Config) provider.Provider {
	t.Helper()
	p, err := provider.New(cfg, providerID+"_test")
	if err != nil {
		t.Fatalf("provider.New(%s): %v", providerID, err)
	}
	return p
}

// waitUntil polls a condition until it holds or the deadline lapses. It
// replaces fixed sleeps that "prove" timing (AGENTS.md testing rules): the
// wait ends as soon as the observable state flips (fast machines finish
// early) and never flakes on slow ones, and a genuine failure still fails
// with a named condition instead of a downstream assertion mystery.
func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !condition() {
		t.Fatalf("timed out waiting for %s", what)
	}
}

// awaitCommitMetrics waits until the server-side Committed effect for key
// (target Requests, attempts/ok, latency/TTFT) has been recorded, then
// returns the metrics snapshot. A client can finish reading a Content-Length
// (non-streaming) body before the handler goroutine runs the post-copy
// Committed effect, so snapshotting right after ReadAll races on loaded
// machines. Streaming responses don't need this: their terminating chunk
// only reaches the client after the handler returns.
func awaitCommitMetrics(t *testing.T, p *Proxy, key counters.PMKey) map[counters.PMKey]counters.ProviderMetricsSnapshot {
	t.Helper()
	waitUntil(t, "commit metrics for "+key.Provider+"/"+key.Model, func() bool {
		return p.metrics.Snapshot()[key].Requests >= 1
	})
	return p.metrics.Snapshot()
}
