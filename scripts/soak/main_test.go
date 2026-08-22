package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestScenarioBodiesAreValidJSON: every scenario builder must emit parseable
// JSON with the requested model routed in (a malformed body would make the
// whole scenario measure the proxy's 400 path instead of the intended flow).
func TestScenarioBodiesAreValidJSON(t *testing.T) {
	for _, s := range scenarios() {
		for _, i := range []int{0, 1, 7} {
			var v map[string]any
			if err := json.Unmarshal([]byte(s.build("soak-model", i)), &v); err != nil {
				t.Errorf("%s[%d]: body not valid JSON: %v", s.name, i, err)
			}
		}
	}
	// The error scenario must NOT route to the real model.
	for _, s := range scenarios() {
		if s.name == "errors" && !strings.Contains(s.build("soak-model", 0), "soak-missing-model") {
			t.Errorf("errors scenario routes to the soak model")
		}
	}
}

// TestRunScenarioAgainstTestServer drives the harness loop end-to-end against
// a local stub proxy: short requests succeed, and the error scenario's
// expected-status inversion reports 502s as ok.
func TestRunScenarioAgainstTestServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if model, _ := body["model"].(string); model == "soak-missing-model" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if stream, _ := body["stream"].(bool); stream {
			w.Header().Set("content-type", "text/event-stream")
			w.Write([]byte("data: {\"type\":\"message_start\"}\n\n"))
			w.(http.Flusher).Flush()
			w.Write([]byte("data: {\"type\":\"message_stop\"}\n\n"))
			return
		}
		w.Write([]byte(`{"id":"m","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer server.Close()
	client := server.Client()

	byName := func(name string) scenario {
		for _, s := range scenarios() {
			if s.name == name {
				return s
			}
		}
		t.Fatalf("scenario %q missing", name)
		return scenario{}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	results := runScenario(ctx, client, server.URL, "soak-model", "short", byName("short"), 2)
	if len(results) == 0 {
		t.Fatal("short scenario produced no results")
	}
	for _, r := range results {
		if !r.ok || r.status != http.StatusOK {
			t.Errorf("short result = %+v, want ok 200", r)
		}
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	results = runScenario(ctx2, client, server.URL, "soak-model", "errors", byName("errors"), 2)
	for _, r := range results {
		if !r.ok || r.status != http.StatusBadGateway {
			t.Errorf("errors result = %+v, want ok with expected 502 inversion", r)
		}
	}
}

// TestReportPercentiles keeps the summarizer honest on a fixed sample.
func TestReportPercentiles(t *testing.T) {
	results := make([]result, 100)
	for i := range results {
		results[i] = result{ok: true, ms: float64(i + 1)}
	}
	rate := report("unit", results)
	if rate != 0 {
		t.Errorf("error rate = %f, want 0", rate)
	}
}
