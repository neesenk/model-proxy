package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
)

// newTerminalLogHarness wires a real request logger into the forward harness.
// Committed-row logging is app-owned (internal/app target pipeline), so every
// row read back here was written by forward itself — legs and the
// all-targets-failed terminal row, nothing else. Call rows() once, after the
// served request has returned: it drains the logger before reading.
func newTerminalLogHarness(t *testing.T) (*harness, func() []requestlog.Record) {
	t.Helper()
	h := newHarness()
	dir := t.TempDir()
	logger := requestlog.New(requestlog.Options{Directory: dir, MaxBodyBytes: 1 << 20})
	go logger.Run()
	t.Cleanup(logger.Shutdown)
	h.svc.ReqLog = logger
	return h, func() []requestlog.Record {
		logger.Shutdown()
		rows, err := requestlog.QueryRecords(dir, requestlog.Filter{Limit: 1000})
		if err != nil {
			t.Fatalf("query request log: %v", err)
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Attempt < rows[j].Attempt })
		return rows
	}
}

func terminalRowConfig(providers map[string]Provider, targets ...RouteTarget) *Config {
	return &Config{
		Providers: providers,
		Routes:    map[string][]RouteTarget{"m": targets},
	}
}

// splitTerminalRows separates the failover legs from the all-targets-failed
// terminal row (the request's only non-attempt row in this harness).
func splitTerminalRows(t *testing.T, rows []requestlog.Record) ([]requestlog.Record, *requestlog.Record) {
	t.Helper()
	var legs []requestlog.Record
	var terminal *requestlog.Record
	for i := range rows {
		if rows[i].AttemptOnly {
			legs = append(legs, rows[i])
			continue
		}
		if terminal != nil {
			t.Fatalf("more than one non-attempt row for one request: %+v", rows)
		}
		terminal = &rows[i]
	}
	return legs, terminal
}

// TestServeAllTargetsFailedLogsTerminalRow: a request the proxy answers with
// its own 502 must leave a request-log row for that answer. Before the fix the
// terminal went unlogged (only the live end event existed), so a failed
// request was invisible wherever attempt legs are excluded — key usage,
// session aggregates, replay.
func TestServeAllTargetsFailedLogsTerminalRow(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	h, rows := newTerminalLogHarness(t)
	cfg := terminalRowConfig(
		map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		RouteTarget{Provider: "up", Model: "real-model"},
	)
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (%s), want 502", w.Code, w.Body.String())
	}

	legs, terminal := splitTerminalRows(t, rows())
	if len(legs) != 1 || legs[0].Provider != "up" || legs[0].Status != http.StatusInternalServerError {
		t.Fatalf("legs = %+v, want the single 500 leg of the only target", legs)
	}
	if terminal == nil {
		t.Fatalf("no terminal row: the client's 502 left no request-log trace")
	}
	if terminal.Provider != terminalLogProvider || terminal.UpstreamModel != "" {
		t.Errorf("terminal provider/upstream_model = %q/%q, want %q/\"\" (the proxy's own row)", terminal.Provider, terminal.UpstreamModel, terminalLogProvider)
	}
	if terminal.Status != http.StatusBadGateway {
		t.Errorf("terminal status = %d, want the 502 the client received", terminal.Status)
	}
	if terminal.ResponseBody != w.Body.String() || terminal.ResponseSize != int64(len(w.Body.String())) {
		t.Errorf("terminal response = %q (%d bytes), want exactly the client bytes %q", terminal.ResponseBody, terminal.ResponseSize, w.Body.String())
	}
	if terminal.RequestBody != openaiChatBody() {
		t.Errorf("terminal request_body = %q, want the original client body (so the failed request stays replayable)", terminal.RequestBody)
	}
	if terminal.Exposed != "m" || terminal.CalledModel != "m" {
		t.Errorf("terminal exposed/called_model = %q/%q, want m/m", terminal.Exposed, terminal.CalledModel)
	}
	if terminal.Protocol != "openai" || terminal.Method != http.MethodPost || terminal.Path != "/v1/chat/completions" {
		t.Errorf("terminal protocol/method/path = %q/%q/%q", terminal.Protocol, terminal.Method, terminal.Path)
	}
	if terminal.RequestID != legs[0].RequestID {
		t.Errorf("terminal request_id = %q, want the legs' %q", terminal.RequestID, legs[0].RequestID)
	}
	if terminal.Attempt != legs[len(legs)-1].Attempt+1 {
		t.Errorf("terminal attempt = %d, want one past the last leg (%d)", terminal.Attempt, legs[len(legs)-1].Attempt)
	}
}

// TestServeAllTargetsFailedLogsEveryLeg: the last hop used to write nothing —
// legs were limited to hops that still had a next target — so the proximate
// cause of the terminal failure (what the last upstream answered) was the one
// piece of evidence the request log never carried.
func TestServeAllTargetsFailedLogsEveryLeg(t *testing.T) {
	first := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"first exploded"}`))
	})
	last := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"last unavailable"}`))
	})
	h, rows := newTerminalLogHarness(t)
	cfg := terminalRowConfig(
		map[string]Provider{
			"first": {OpenAIBaseURL: first.srv.URL, Provider: "test-static"},
			"last":  {OpenAIBaseURL: last.srv.URL, Provider: "test-static"},
		},
		RouteTarget{Provider: "first", Model: "x"},
		RouteTarget{Provider: "last", Model: "y"},
	)
	if w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (%s), want 502", w.Code, w.Body.String())
	}

	legs, terminal := splitTerminalRows(t, rows())
	if len(legs) != 2 {
		t.Fatalf("legs = %+v, want both hops (the last hop included)", legs)
	}
	want := []struct {
		provider string
		status   int
		evidence string
	}{
		{"first", http.StatusInternalServerError, "first exploded"},
		{"last", http.StatusServiceUnavailable, "last unavailable"},
	}
	for i, w := range want {
		if legs[i].Provider != w.provider || legs[i].Status != w.status || legs[i].Attempt != i {
			t.Errorf("leg %d = provider %q status %d attempt %d, want %q/%d/%d", i, legs[i].Provider, legs[i].Status, legs[i].Attempt, w.provider, w.status, i)
		}
		if !strings.Contains(legs[i].ResponseBody, w.evidence) {
			t.Errorf("leg %d response_body = %q, want the upstream answer %q as evidence", i, legs[i].ResponseBody, w.evidence)
		}
	}
	if terminal == nil {
		t.Fatalf("no terminal row for the all-targets-failed request")
	}
	if terminal.Status != http.StatusBadGateway || terminal.Attempt != 2 {
		t.Errorf("terminal status/attempt = %d/%d, want 502/2 (after both legs)", terminal.Status, terminal.Attempt)
	}
}

// TestServeRateLimitedTerminalLogsRetryAfter: the rate-limit terminal answers
// 429 with Retry-After; its row must carry the same status and header.
func TestServeRateLimitedTerminalLogsRetryAfter(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h, rows := newTerminalLogHarness(t)
	h.state.allRateLimited = true
	h.state.earliestIn = 2 * time.Minute
	cfg := terminalRowConfig(
		map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		RouteTarget{Provider: "up", Model: "real-model"},
	)
	cfg.Scheduling = Scheduling{RetryWait: "1ms"}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status = %d retry-after = %q, want 429 with Retry-After", w.Code, w.Header().Get("Retry-After"))
	}

	_, terminal := splitTerminalRows(t, rows())
	if terminal == nil {
		t.Fatalf("no terminal row for the 429 terminal")
	}
	if terminal.Status != http.StatusTooManyRequests {
		t.Errorf("terminal status = %d, want the 429 the client received", terminal.Status)
	}
	if want := `"retry-after":"` + w.Header().Get("Retry-After") + `"`; !strings.Contains(terminal.ResponseHeaders, want) {
		t.Errorf("terminal response_headers = %q, want %s", terminal.ResponseHeaders, want)
	}
}

// TestServeAllTargetsFailedTerminalCarriesScheduleAndProfile: the terminal row
// is the request's own record, so it carries the routing metadata a committed
// row carries — the scheduling engine that produced the try order, and the
// request profile. The harness's pass-through scheduler never names a source,
// which is exactly why this test installs one that does.
func TestServeAllTargetsFailedTerminalCarriesScheduleAndProfile(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	h, rows := newTerminalLogHarness(t)
	h.svc.Schedule = func(_ *Config, _ map[string]string, _, _ string, _ bool, targets []RouteTarget, _ map[string]bool, _ uint64) ([]RouteTarget, string) {
		return targets, "load_balance"
	}
	cfg := terminalRowConfig(
		map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		RouteTarget{Provider: "up", Model: "real-model"},
	)
	if w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil); w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d (%s), want 502", w.Code, w.Body.String())
	}

	_, terminal := splitTerminalRows(t, rows())
	if terminal == nil {
		t.Fatalf("no terminal row")
	}
	if terminal.Schedule == nil || terminal.Schedule.Source != "load_balance" || terminal.Schedule.Note != "" {
		t.Errorf("terminal schedule = %+v, want source load_balance and no reroute note", terminal.Schedule)
	}
	if terminal.Profile == nil {
		t.Error("terminal row carries no request profile")
	}
}

// TestServeClientGoneLogsNoTerminalRow: a client that walks away mid-failover
// gets no answer from the proxy, so there is no terminal to log — the 499 live
// event is the only trace, and the legs already written stay the request's
// newest standalone rows.
func TestServeClientGoneLogsNoTerminalRow(t *testing.T) {
	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce atomic.Bool
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- struct{}{}
		// Go's http1 transport does not promptly close an outbound connection
		// that carried a request BODY when the round trip is canceled, so the
		// handler's r.Context() never fires: release explicitly.
		<-release
	}))
	defer blocked.Close()
	defer func() {
		if releaseOnce.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	fallback := newFakeUpstream(t, openaiOKResponder("never reached"))
	h, rows := newTerminalLogHarness(t)
	cfg := terminalRowConfig(
		map[string]Provider{
			"blocked":  {OpenAIBaseURL: blocked.URL, Provider: "test-static"},
			"fallback": {OpenAIBaseURL: fallback.srv.URL, Provider: "test-static"},
		},
		RouteTarget{Provider: "blocked", Model: "x"},
		RouteTarget{Provider: "fallback", Model: "y"},
	)
	snap := h.snapshot(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(openaiChatBody())).WithContext(ctx)
	w := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		defer close(served)
		Serve(h.svc, h.state, snap, "openai", w, r, "req-test")
	}()

	// Cancel once the first hop is in flight: the executor reports the hop as
	// client-gone and the failover chain stops without reaching the terminal.
	<-hit
	cancel()
	<-served
	if releaseOnce.CompareAndSwap(false, true) {
		close(release)
	}

	if fallback.hits() != 0 {
		t.Errorf("fallback hits = %d, want 0 (the chain stops at the client-gone hop)", fallback.hits())
	}
	legs, terminal := splitTerminalRows(t, rows())
	if terminal != nil {
		t.Errorf("client-gone request logged a terminal row %+v — it never answered the client", terminal)
	}
	if len(legs) != 0 {
		t.Errorf("client-gone request logged legs %+v, want none (the hop carried no upstream verdict)", legs)
	}
}
