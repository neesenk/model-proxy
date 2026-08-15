package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/targetexec"
)

// postStatus is post() with the status code returned (for asserting committed
// upstream errors).
func postStatus(t *testing.T, url, body string) int {
	t.Helper()
	resp, err := http.Post(url, "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func modelLockCfg(primary, fallback *httptest.Server) *Config {
	return &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
			"m2": {
				{Provider: "primary", Model: "m2", Priority: 1},
			},
		},
		Scheduling: Scheduling{
			CircuitThreshold: 3, CircuitCooldown: "50ms", RateLimitBackoff: "10s",
			UpstreamTimeout: "5s", StickyDwell: "0s", ModelLockout: "10m",
		},
	}
}

func newModelLockProxy(t *testing.T, cfg *Config) (*Proxy, *httptest.Server) {
	t.Helper()
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px
}

func (p *Proxy) modelLockState(provider, model string) (failures int, locked bool) {
	for _, entry := range p.runtimeState.Dashboard(time.Now()).ModelLocks[provider] {
		if entry.Model == model {
			return entry.Failures, true
		}
	}
	return 0, false
}

// TestModelLock_404FailsOverWithoutCircuit: a 404 on the primary's model fails
// over to the fallback (client still gets 200), locks ONLY (primary, m1), and
// never touches the primary's circuit — its other model m2 keeps serving.
func TestModelLock_404FailsOverWithoutCircuit(t *testing.T) {
	primary, pHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 404, `{"error":"model not found"}`, nil, 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("m1 request: status = %d, want 200 (failover to fallback)", st)
	}
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1", got)
	}
	if got := fHits.Load(); got != 1 {
		t.Errorf("fallback hits = %d, want 1", got)
	}
	failures, locked := p.modelLockState("primary", "m1")
	if !locked || failures != 1 {
		t.Errorf("model lock (primary,m1): failures=%d locked=%v, want 1,true", failures, locked)
	}
	// Circuit must NOT be poisoned.
	if h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]; ok &&
		h.ConsecutiveFailures != 0 {
		t.Errorf("primary circuit poisoned by model failure: %+v", h)
	}

	// Next m1 request skips the locked model entirely.
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits after lock = %d, want 1 (locked model skipped)", got)
	}
	if got := fHits.Load(); got != 2 {
		t.Errorf("fallback hits = %d, want 2", got)
	}
}

// TestModelLock_OtherModelUnaffected: with (primary, m1) locked, the primary's
// OTHER model m2 is still served by the primary (no account-level damage).
// Uses a primary that 404s only m1 and serves m2.
func TestModelLock_OtherModelUnaffected(t *testing.T) {
	primary, pHits := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c == 1 {
			return 404, `{"error":"model not found"}`, nil, 0 // the m1 request
		}
		return 200, `{"ok":true}`, nil, 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	_, px := newModelLockProxy(t, cfg)

	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // 404 → lock (primary,m1)
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m2","messages":[]}`); st != 200 {
		t.Fatalf("m2 request: status = %d, want 200 (primary still serves m2)", st)
	}
	if got := pHits.Load(); got != 2 {
		t.Errorf("primary hits = %d, want 2 (m2 must reach the primary despite the m1 lock)", got)
	}
}

// TestModelLock_LastTargetCommitsUpstreamError: on a single-target route the
// upstream 404 commits unchanged (no opaque 502), while the lock is recorded
// for subsequent requests.
func TestModelLock_LastTargetCommitsUpstreamError(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 404, `{"error":"model not found"}`, nil, 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m2","messages":[]}`); st != 404 {
		t.Errorf("single-target 404: status = %d, want 404 (upstream error committed)", st)
	}
	if got := fHits.Load(); got != 0 {
		t.Errorf("fallback hits = %d, want 0 (m2 routes only to primary)", got)
	}
	if _, locked := p.modelLockState("primary", "m2"); !locked {
		t.Error("(primary,m2) should be locked after the committed 404")
	}
}

// TestModelLock_ModelDenied400: a 400 "no access to model" locks the model and
// fails over; an unrelated 400 (bad request) commits without locking.
func TestModelLock_ModelDenied400(t *testing.T) {
	primary, _ := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c == 1 {
			return 400, `{"error":"You do not have access to model m1"}`, nil, 0
		}
		return 400, `{"error":"max_tokens is too large"}`, nil, 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("denied 400: status = %d, want 200 (failover)", st)
	}
	if _, locked := p.modelLockState("primary", "m1"); !locked {
		t.Error("(primary,m1) should be locked after model-denied 400")
	}

	// An ordinary 400 on m2 (single target) commits and does NOT lock.
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m2","messages":[]}`); st != 400 {
		t.Errorf("ordinary 400: status = %d, want 400 (committed unchanged)", st)
	}
	if _, locked := p.modelLockState("primary", "m2"); locked {
		t.Error("(primary,m2) must NOT be locked by an ordinary 400")
	}
}

// TestModelLock_SuccessClears: after the lockout expires, a served response
// from the same (provider, model) clears the lock.
func TestModelLock_SuccessClears(t *testing.T) {
	primary, _ := newHitServer(func(c int) (int, string, http.Header, time.Duration) {
		if c == 1 {
			return 404, `{"error":"model not found"}`, nil, 0
		}
		return 200, `{"ok":true}`, nil, 0 // recovered
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	cfg.Scheduling.ModelLockout = "50ms"
	p, px := newModelLockProxy(t, cfg)

	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // 404 → lock
	if _, locked := p.modelLockState("primary", "m1"); !locked {
		t.Fatal("expected (primary,m1) locked")
	}
	// Lockout expiry is observable — poll instead of sleeping a fixed 80ms.
	waitUntil(t, "model lockout expiry", func() bool {
		_, locked := p.modelLockState("primary", "m1")
		return !locked
	})
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // primary serves → clear
	if _, locked := p.modelLockState("primary", "m1"); locked {
		t.Error("model lock should be cleared after a served response")
	}
}

// TestEmpty200_PreflightFailsOver: a 200 with Content-Length: 0 is caught
// BEFORE commit — the client gets the fallback's real answer, the model is
// locked, and the account's circuit stays clean.
func TestEmpty200_PreflightFailsOver(t *testing.T) {
	primary, pHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, ``, nil, 0 // Go serves this as Content-Length: 0
	})
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("status = %d, want 200 (fallback served)", st)
	}
	if got := fHits.Load(); got != 1 {
		t.Errorf("fallback hits = %d, want 1 (empty 200 failed over)", got)
	}
	if _, locked := p.modelLockState("primary", "m1"); !locked {
		t.Error("(primary,m1) should be locked after the empty 200")
	}
	if h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]; ok &&
		h.ConsecutiveFailures != 0 {
		t.Errorf("circuit poisoned by empty 200: %+v", h)
	}

	// Next request skips the locked model.
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1 (locked after empty 200)", got)
	}
}

// TestEmpty200_PostCommitLearned: a chunked (no Content-Length) 200 that
// streams zero bytes commits to the client — it can't be recalled — but the
// failure is learned post-commit and the NEXT request fails over.
func TestEmpty200_PostCommitLearned(t *testing.T) {
	var pHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		pHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // commits chunked encoding with zero data bytes
		}
	}))
	defer primary.Close()
	fallback, fHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`); st != 200 {
		t.Fatalf("status = %d, want 200 (empty stream committed)", st)
	}
	if got := fHits.Load(); got != 0 {
		t.Errorf("fallback hits = %d, want 0 (first empty stream commits)", got)
	}
	if _, locked := p.modelLockState("primary", "m1"); !locked {
		t.Error("(primary,m1) should be locked by the post-commit zero-byte 200")
	}

	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`) // fails over now
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1 (locked after post-commit learning)", got)
	}
	if got := fHits.Load(); got != 1 {
		t.Errorf("fallback hits = %d, want 1", got)
	}
}

// TestParamStrip_LearnAndRetry: a 400 "Unsupported parameter" teaches the
// blocklist; the current request is retried immediately with the param
// stripped, and FUTURE requests strip it preemptively (no more 400s).
func TestParamStrip_LearnAndRetry(t *testing.T) {
	var sawMaxTokens atomic.Int32
	var hits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		if bytes.Contains(b, []byte(`"max_tokens"`)) {
			sawMaxTokens.Add(1)
			w.WriteHeader(400)
			w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model."}}`))
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	// First request: 400 → learn → strip → retry → 200.
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[],"max_tokens":100}`); st != 200 {
		t.Fatalf("status = %d, want 200 (strip-retry served)", st)
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("primary hits = %d, want 2 (400 + stripped retry)", got)
	}
	if got := sawMaxTokens.Load(); got != 1 {
		t.Errorf("bodies with max_tokens = %d, want 1 (only the first attempt)", got)
	}
	blocked := p.runtimeState.ParamBlocked("primary", "m1", "max_tokens")
	if !blocked {
		t.Error("max_tokens should be in primary's learned blocklist")
	}

	// Second request: stripped preemptively — no 400 round-trip at all.
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[],"max_tokens":100}`); st != 200 {
		t.Fatalf("status = %d, want 200", st)
	}
	if got := hits.Load(); got != 3 {
		t.Errorf("primary hits = %d, want 3 (preemptive strip, no retry)", got)
	}
	if got := sawMaxTokens.Load(); got != 1 {
		t.Errorf("bodies with max_tokens = %d, want still 1", got)
	}
}

// TestStripTopLevelParam: unit semantics of the best-effort stripper.
func TestStripTopLevelParam(t *testing.T) {
	out, did := targetexec.StripTopLevelParam([]byte(`{"model":"m","max_tokens":5,"messages":[]}`), "max_tokens")
	if !did {
		t.Fatal("expected did=true")
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	if _, ok := obj["max_tokens"]; ok {
		t.Error("max_tokens not stripped")
	}
	if _, ok := obj["model"]; !ok {
		t.Error("model must be preserved")
	}
	if _, did := targetexec.StripTopLevelParam([]byte(`{"model":"m"}`), "max_tokens"); did {
		t.Error("absent key: did should be false")
	}
	if _, did := targetexec.StripTopLevelParam([]byte(`not-json`), "max_tokens"); did {
		t.Error("non-JSON: did should be false")
	}
}

// TestEmpty200_ClientCancelNoLock (P1-1c): a client that disconnects before
// the first byte must NOT produce a model lock — zero bytes streamed in that
// case means "client gone", not "empty upstream body".
func TestEmpty200_ClientCancelNoLock(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers committed; body streams zero bytes while open
		}
		time.Sleep(500 * time.Millisecond) // outlast the client's 150ms timeout
	}))
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := modelLockCfg(primary, fallback)
	p, px := newModelLockProxy(t, cfg)

	client := &http.Client{Timeout: 150 * time.Millisecond}
	resp, err := client.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
	if err == nil {
		resp.Body.Close()
	}
	time.Sleep(300 * time.Millisecond) // let the proxy observe the cancellation
	n := 0
	for _, locks := range p.runtimeState.Dashboard(time.Now()).ModelLocks {
		n += len(locks)
	}
	if n != 0 {
		t.Errorf("client disconnect must NOT lock the model, got %d lock(s)", n)
	}
}
