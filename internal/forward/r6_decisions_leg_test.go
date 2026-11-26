package forward

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/logx"
)

// r6DecisionsStatusResponder answers every leg call with status and body.
func r6DecisionsStatusResponder(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// r6FlakyTransport fails its first failCalls round trips with a fast-fail
// connection sentinel (io.ErrUnexpectedEOF — http.Client.Do wraps it in the
// url.Error a real transport surfaces), then delegates to the real transport.
type r6FlakyTransport struct {
	failCalls int32
	calls     atomic.Int32
	failedURL string
	next      http.RoundTripper
}

func (transport *r6FlakyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if transport.calls.Add(1) <= transport.failCalls {
		transport.failedURL = req.URL.String()
		return nil, io.ErrUnexpectedEOF
	}
	return transport.next.RoundTrip(req)
}

// r6CaptureLogs redirects the standard logger — logx's only output — into a
// buffer for the rest of the test, with the timestamp prefix dropped so a
// line can be compared whole. The logger is process-global: tests using this
// helper must not run in parallel.
func r6CaptureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	original, flags, level := log.Writer(), log.Flags(), logx.CurrentLevel()
	log.SetOutput(&logs)
	log.SetFlags(0)
	logx.SetLevel("info")
	t.Cleanup(func() {
		log.SetOutput(original)
		log.SetFlags(flags)
		logx.SetLevel(level.String())
	})
	return &logs
}

func r6RedispatchLines(logs *bytes.Buffer) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(line, "fast-fail connection error") {
			lines = append(lines, line)
		}
	}
	return lines
}

func r6AssertRedispatchLog(t *testing.T, logs *bytes.Buffer, kind, provider, failedURL string) {
	t.Helper()
	want := fmt.Sprintf("[%s provider=%s] fast-fail connection error (Post %q: unexpected EOF) — redispatching once on a fresh connection", kind, provider, failedURL)
	got := r6RedispatchLines(logs)
	if len(got) != 1 || got[0] != want {
		t.Errorf("redispatch log = %q, want exactly one line %q", got, want)
	}
}

func r6DecisionsHarness(t *testing.T, up *fakeUpstream) (*harness, pipeline, decisionsInput) {
	t.Helper()
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {DecisionsBaseURL: up.srv.URL + "/v1", Provider: "typesafe"},
		},
	}
	snap := h.snapshot(cfg)
	in := decisionsInput{
		Runtime:     snap,
		Target:      RouteTarget{Provider: "p", Model: "jev-1.13", Protocol: "decisions"},
		RequestID:   "req-1",
		Kind:        "route-select",
		Proto:       "openai",
		Exposed:     "m",
		CalledModel: "m",
		Timeout:     5 * time.Second,
		State:       map[string]any{"task": "hello"},
		Candidates: []selectorCandidate{
			{Target: RouteTarget{Provider: "a", Model: "ma"}, ID: "c0"},
			{Target: RouteTarget{Provider: "b", Model: "mb"}, ID: "c1"},
		},
		ChoiceInstructions: "pick one",
	}
	return h, pipeline{svc: h.svc, state: h.state}, in
}

// r6DecisionsMetrics reads the decisions leg's counters (provider "p", model "jev-1.13").
func r6DecisionsMetrics(h *harness) counters.ProviderMetricsSnapshot {
	return h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "p", Model: "jev-1.13"}]
}

// TestR6CallDecisionsContentRefusal5xx: the decisions leg applies the
// executor's content-refusal rule too — a 5xx whose body proves a CONTENT
// refusal is attributed to the request, not the provider, read from the same
// bounded 8 KiB window. The leg is abandoned without touching the circuit or
// the failure metric, and — like every other failure class of this leg —
// without a failover metric; an ordinary 5xx keeps its accounting exactly as
// before.
func TestR6CallDecisionsContentRefusal5xx(t *testing.T) {
	const refusal = `{"error":{"message":"Output blocked by content filtering policy","type":"server_error"}}`
	call := func(t *testing.T, status int, body string) (*harness, decisionsOutput, *fakeUpstream) {
		t.Helper()
		up := newFakeUpstream(t, r6DecisionsStatusResponder(status, body))
		h, p, in := r6DecisionsHarness(t, up)
		return h, p.callDecisions(context.Background(), in), up
	}

	t.Run("content refusal abandons the leg without any accounting", func(t *testing.T) {
		h, res, up := call(t, http.StatusBadGateway, refusal)
		if res.Err == nil {
			t.Fatal("selector result has no error, want the 502 leg failure")
		}
		if got, want := res.Err.Error(), "upstream status 502 proved a content refusal"; got != want {
			t.Errorf("res.Err = %q, want %q (the refusal must be told apart from an upstream fault)", got, want)
		}
		if up.hits() != 1 {
			t.Errorf("upstream hits = %d, want 1", up.hits())
		}
		if h.gate.failures["p"] != 0 || h.gate.successes["p"] != 0 || h.gate.modelFailures["p/jev-1.13"] != 0 {
			t.Errorf("gate = failures %d, successes %d, model failures %d; want a content refusal to record nothing",
				h.gate.failures["p"], h.gate.successes["p"], h.gate.modelFailures["p/jev-1.13"])
		}
		if metrics := r6DecisionsMetrics(h); metrics.Failures != 0 || metrics.Failovers != 0 || metrics.Requests != 0 {
			t.Errorf("metrics = %+v, want all zero (the decisions leg never counts a failover)", metrics)
		}
	})

	t.Run("ordinary 5xx keeps its accounting", func(t *testing.T) {
		h, res, _ := call(t, http.StatusInternalServerError, `{"error":"internal server error"}`)
		if res.Err == nil {
			t.Fatal("selector result has no error, want the 500 leg failure")
		}
		if got, want := res.Err.Error(), "upstream status 500"; got != want {
			t.Errorf("res.Err = %q, want %q", got, want)
		}
		if h.gate.failures["p"] != 1 || h.gate.successes["p"] != 0 {
			t.Errorf("gate = failures %d, successes %d; want 1/0", h.gate.failures["p"], h.gate.successes["p"])
		}
		if metrics := r6DecisionsMetrics(h); metrics.Failures != 1 || metrics.Failovers != 0 || metrics.Requests != 0 {
			t.Errorf("metrics = %+v, want Failures 1 / Failovers 0 / Requests 0", metrics)
		}
	})

	t.Run("refusal wording beyond the 8 KiB peek is not evidence", func(t *testing.T) {
		body := `{"error":{"message":"` + strings.Repeat("x", 8300) + ` Output blocked by content filtering policy","type":"server_error"}}`
		h, res, _ := call(t, http.StatusBadGateway, body)
		if res.Err == nil {
			t.Fatal("selector result has no error, want the 502 leg failure")
		}
		if got, want := res.Err.Error(), "upstream status 502"; got != want {
			t.Errorf("res.Err = %q, want %q (the leg must read the same 8 KiB window as the executor)", got, want)
		}
		if metrics := r6DecisionsMetrics(h); h.gate.failures["p"] != 1 || metrics.Failures != 1 || metrics.Failovers != 0 {
			t.Errorf("gate failures = %d, metrics = %+v; want an ordinary 5xx (1 / Failures 1 / Failovers 0)", h.gate.failures["p"], metrics)
		}
	})
}

// TestR6CallDecisionsFastFailConnRedispatch: the decisions call site gets
// BufferedLeg's one-shot fast-fail redispatch, and reports it in serve.log in
// the same line shape as the executor — the leg's Kind and the provider in the
// prefix. A redispatch that succeeds never reaches the breaker or the failure
// metric; when the redispatch fails too, the leg counts exactly ONE failure
// and stops — there is no third dispatch.
func TestR6CallDecisionsFastFailConnRedispatch(t *testing.T) {
	install := func(t *testing.T, failCalls int32) (*harness, pipeline, decisionsInput, *r6FlakyTransport, *fakeUpstream) {
		t.Helper()
		up := newFakeUpstream(t, selectorDecisionResponder("c1", 0.87))
		h, _, in := r6DecisionsHarness(t, up)
		transport := &r6FlakyTransport{failCalls: failCalls, next: http.DefaultTransport}
		h.svc.Client = &http.Client{Timeout: 30 * time.Second, Transport: transport}
		return h, pipeline{svc: h.svc, state: h.state}, in, transport, up
	}

	t.Run("blip then 200 succeeds without any failure accounting", func(t *testing.T) {
		logs := r6CaptureLogs(t)
		h, p, in, transport, up := install(t, 1)
		res := p.callDecisions(context.Background(), in)
		if res.Err != nil || res.ChoiceID != "c1" || res.Model != "jev-1.13.0" {
			t.Fatalf("selector result = %+v, want the redispatched decision", res)
		}
		if got := transport.calls.Load(); got != 2 || up.hits() != 1 {
			t.Errorf("dispatches/upstream hits = %d/%d, want 2/1 (blip + redispatch)", got, up.hits())
		}
		if h.gate.failures["p"] != 0 || h.gate.successes["p"] != 1 {
			t.Errorf("gate = failures %d, successes %d; want 0/1", h.gate.failures["p"], h.gate.successes["p"])
		}
		if metrics := r6DecisionsMetrics(h); metrics.Requests != 1 || metrics.Failures != 0 || metrics.Failovers != 0 {
			t.Errorf("metrics = %+v, want Requests 1 / Failures 0 / Failovers 0", metrics)
		}
		r6AssertRedispatchLog(t, logs, "route-select", "p", transport.failedURL)
	})

	t.Run("blip twice counts one failure and stops", func(t *testing.T) {
		logs := r6CaptureLogs(t)
		h, p, in, transport, up := install(t, 1<<30)
		res := p.callDecisions(context.Background(), in)
		if res.Err == nil {
			t.Fatalf("selector result = %+v, want the transport failure", res)
		}
		if got := transport.calls.Load(); got != 2 || up.hits() != 0 {
			t.Fatalf("dispatches/upstream hits = %d/%d, want exactly 2/0 (one redispatch, never a third)", got, up.hits())
		}
		if h.gate.failures["p"] != 1 || h.gate.successes["p"] != 0 {
			t.Errorf("gate = failures %d, successes %d; want 1/0 (the confirmed failure, counted once)", h.gate.failures["p"], h.gate.successes["p"])
		}
		if metrics := r6DecisionsMetrics(h); metrics.Requests != 0 || metrics.Failures != 1 || metrics.Failovers != 0 {
			t.Errorf("metrics = %+v, want Requests 0 / Failures 1 / Failovers 0", metrics)
		}
		r6AssertRedispatchLog(t, logs, "route-select", "p", transport.failedURL)
	})
}
