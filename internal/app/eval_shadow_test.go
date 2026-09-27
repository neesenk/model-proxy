package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
)

func boolPtr(v bool) *bool { return &v }

// TestEvalShadow_PairwiseJudgeLogsVerdict exercises the full L2 eval shadow
// path: a graded primary commits, the shadow request replays to the paired
// grade, a judge compares the two responses, and the shadow request-log record
// carries the eval_verdict diagnostic.
func TestEvalShadow_PairwiseJudgeLogsVerdict(t *testing.T) {
	var shadowHit atomic.Bool
	var shadowBody atomic.Value
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHit.Store(true)
		body, _ := io.ReadAll(r.Body)
		shadowBody.Store(string(body))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"shadow-1","choices":[{"message":{"role":"assistant","content":"shadow answer"},"finish_reason":"stop"}]}`))
	}))
	defer shadowUpstream.Close()

	var judgeHit atomic.Bool
	judgeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeHit.Store(true)
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"model": "typesafe/jev-1.13.0",
			"answers": {"model_choice": {"type": "choice", "choice": "shadow_better", "confidence": 0.8}},
			"usage": {"input_tokens": 100, "output_tokens": 10}
		}`))
	}))
	defer judgeUpstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary": {Provider: testProviderID, OpenAIBaseURL: ""},
			"shadow":  {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
			"judge":   {Provider: testProviderID, DecisionsBaseURL: judgeUpstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"alias": {
				{Provider: "primary", Model: "primary-model", Priority: 1},
				{Provider: "shadow", Model: "shadow-model", Priority: 2},
			},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"alias": {
				Grades: map[string][]configdomain.RouteTarget{
					"cheap":  {{Provider: "primary", Model: "primary-model"}},
					"strong": {{Provider: "shadow", Model: "shadow-model"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolPtr(false)}, Grade: "cheap"},
				},
				Eval: &configdomain.EvalConfig{
					SampleRate: 1.0,
					Pair:       "opposite",
					Judge:      configdomain.RouteTarget{Provider: "judge", Model: "jev-1.13.0", Protocol: "decisions"},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "primary"}
	p.providers["shadow"] = &testProv{key: "shadow"}
	p.providers["judge"] = &testProv{key: "judge"}
	// Deterministically sample every request for the test.
	p.evalRand = func() float64 { return 0 }

	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"primary-1","choices":[{"message":{"role":"assistant","content":"primary answer"},"finish_reason":"stop"}]}`))
	}))
	defer primaryUpstream.Close()
	// Point primary provider at its upstream after proxy construction so the
	// config load does not need a live URL.
	p.cfg.Providers["primary"] = configdomain.Provider{
		Provider:      testProviderID,
		OpenAIBaseURL: primaryUpstream.URL,
	}

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Wait for shadow + judge to finish and the log to drain.
	time.Sleep(300 * time.Millisecond)
	p.Close()

	if !shadowHit.Load() {
		t.Fatal("shadow upstream was not called")
	}
	if !judgeHit.Load() {
		t.Fatal("judge upstream was not called")
	}

	var verdict string
	var primaryRecord *requestlog.Record
	records := allRecords(t, logDir)
	for _, record := range records {
		if strings.HasPrefix(record.RequestID, "shadow-") {
			for _, d := range record.Diagnostics {
				if d.Code == "eval_verdict" {
					verdict = d.Detail
				}
			}
		} else if primaryRecord == nil {
			r := record
			primaryRecord = &r
		}
	}
	if verdict != "shadow_better" {
		t.Fatalf("eval_verdict = %q, want shadow_better; primary routing = %+v", verdict, primaryRecord.Routing)
	}

	// The judge request-log record must not persist the response bodies because
	// eval judge calls are marked sensitive.
	var judgeRecord *requestlog.Record
	for i := range records {
		if strings.HasPrefix(records[i].RequestID, "eval-judge-") {
			judgeRecord = &records[i]
			break
		}
	}
	if judgeRecord == nil {
		t.Fatal("judge request-log record missing")
	}
	if judgeRecord.RequestBody != "" {
		t.Errorf("judge request-log body = %q, want empty (sensitive)", judgeRecord.RequestBody)
	}

	// Shadow replay should use the paired grade's target.
	shadowReqBody, _ := shadowBody.Load().(string)
	var shadowReq map[string]any
	if err := json.Unmarshal([]byte(shadowReqBody), &shadowReq); err != nil {
		t.Fatalf("shadow request body: %v", err)
	}
	if shadowReq["model"] != "shadow-model" {
		t.Fatalf("shadow model = %v, want shadow-model", shadowReq["model"])
	}
}

// TestEvalShadow_SkipsWhenNotSampled ensures eval shadow dispatch respects the
// configured sample rate and does not fire when the random draw fails.
func TestEvalShadow_SkipsWhenNotSampled(t *testing.T) {
	var shadowHit atomic.Bool
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHit.Store(true)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUpstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary": {Provider: testProviderID, OpenAIBaseURL: ""},
			"shadow":  {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"alias": {
				{Provider: "primary", Model: "primary-model", Priority: 1},
				{Provider: "shadow", Model: "shadow-model", Priority: 2},
			},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"alias": {
				Grades: map[string][]configdomain.RouteTarget{
					"cheap":  {{Provider: "primary", Model: "primary-model"}},
					"strong": {{Provider: "shadow", Model: "shadow-model"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolPtr(false)}, Grade: "cheap"},
				},
				Eval: &configdomain.EvalConfig{
					SampleRate: 0.5,
					Pair:       "opposite",
					Judge:      configdomain.RouteTarget{Provider: "shadow", Model: "shadow-model", Protocol: "decisions"},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "primary"}
	p.providers["shadow"] = &testProv{key: "shadow"}
	// Force the sample draw to miss.
	p.evalRand = func() float64 { return 0.9 }

	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"primary-1","choices":[{"message":{"role":"assistant","content":"primary answer"},"finish_reason":"stop"}]}`))
	}))
	defer primaryUpstream.Close()
	p.cfg.Providers["primary"] = configdomain.Provider{
		Provider:      testProviderID,
		OpenAIBaseURL: primaryUpstream.URL,
	}

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)
	p.Close()

	if shadowHit.Load() {
		t.Fatal("shadow upstream was called despite failed sample draw")
	}
}

// TestEvalShadow_PinForceSkipsEval confirms that pin/force requests bypass
// route policy and therefore do not trigger eval shadow dispatch.
func TestEvalShadow_PinForceSkipsEval(t *testing.T) {
	var shadowHit atomic.Bool
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHit.Store(true)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUpstream.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary": {Provider: testProviderID, OpenAIBaseURL: ""},
			"shadow":  {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"alias": {
				{Provider: "primary", Model: "primary-model", Priority: 1},
				{Provider: "shadow", Model: "shadow-model", Priority: 2},
			},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"alias": {
				Grades: map[string][]configdomain.RouteTarget{
					"cheap":  {{Provider: "primary", Model: "primary-model"}},
					"strong": {{Provider: "shadow", Model: "shadow-model"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolPtr(false)}, Grade: "cheap"},
				},
				Eval: &configdomain.EvalConfig{
					SampleRate: 1.0,
					Pair:       "opposite",
					Judge:      configdomain.RouteTarget{Provider: "shadow", Model: "shadow-model", Protocol: "decisions"},
				},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "primary"}
	p.providers["shadow"] = &testProv{key: "shadow"}
	p.evalRand = func() float64 { return 0 }

	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"primary-1","choices":[{"message":{"role":"assistant","content":"primary answer"},"finish_reason":"stop"}]}`))
	}))
	defer primaryUpstream.Close()
	p.cfg.Providers["primary"] = configdomain.Provider{
		Provider:      testProviderID,
		OpenAIBaseURL: primaryUpstream.URL,
	}

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-mp-force-provider", "primary")
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	time.Sleep(200 * time.Millisecond)
	p.Close()

	if shadowHit.Load() {
		t.Fatal("shadow upstream was called for a force-provider request")
	}
}
