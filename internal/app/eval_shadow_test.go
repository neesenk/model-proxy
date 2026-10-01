package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

	post(t, px.URL+"/v1/chat/completions",
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)

	// The detached shadow/judge tasks are admitted asynchronously after the
	// client response returns; wait for their observable effects BEFORE Close
	// (Close only waits for already-admitted tasks), then Close drains the
	// request log so the on-disk assertions below are stable.
	waitUntil(t, "shadow upstream hit", shadowHit.Load)
	waitUntil(t, "judge upstream hit", judgeHit.Load)
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

	post(t, px.URL+"/v1/chat/completions",
		`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)

	// The shadow dispatch decision (sample draw / force-provider policy) is
	// synchronous on the forward goroutine, and Close waits for admitted
	// detached tasks — no fixed sleep needed to prove the negative.
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

	// The shadow dispatch decision (sample draw / force-provider policy) is
	// synchronous on the forward goroutine, and Close waits for admitted
	// detached tasks — no fixed sleep needed to prove the negative.
	p.Close()

	if shadowHit.Load() {
		t.Fatal("shadow upstream was called for a force-provider request")
	}
}

// TestResolveEvalPairGradeUsesRouteTargetOrder pins "opposite" pairing to the
// route's expanded target order (via forward.GradeOrder), not the grades map
// iteration order: with >=3 grades a map-derived order is nondeterministic and
// would pair different grades on different runs.
func TestResolveEvalPairGradeUsesRouteTargetOrder(t *testing.T) {
	grades := map[string][]configdomain.RouteTarget{
		"cheap":  {{Provider: "a", Model: "cheap"}},
		"mid":    {{Provider: "b", Model: "mid"}},
		"strong": {{Provider: "c", Model: "strong"}},
	}
	ordered := []configdomain.RouteTarget{
		{Provider: "a", Model: "cheap"},
		{Provider: "b", Model: "mid"},
		{Provider: "c", Model: "strong"},
	}
	for _, tc := range []struct{ primary, want string }{
		{"cheap", "mid"},
		{"mid", "strong"},
		{"strong", "mid"}, // last grade wraps to the previous one
	} {
		got, ok := resolveEvalPairGrade(tc.primary, "opposite", ordered, grades, nil)
		if !ok || got != tc.want {
			t.Fatalf("opposite(%s) = %q,%v want %q,true", tc.primary, got, ok, tc.want)
		}
	}

	// Reversing the route target order must reverse the pairing: this is only
	// possible if the order comes from `ordered`, not the map.
	reversed := []configdomain.RouteTarget{
		{Provider: "c", Model: "strong"},
		{Provider: "b", Model: "mid"},
		{Provider: "a", Model: "cheap"},
	}
	if got, ok := resolveEvalPairGrade("mid", "opposite", reversed, grades, nil); !ok || got != "cheap" {
		t.Fatalf("opposite(mid) over reversed order = %q,%v want cheap,true", got, ok)
	}

	// An explicit grade: pair ignores the order entirely.
	if got, ok := resolveEvalPairGrade("cheap", "grade:strong", ordered, grades, nil); !ok || got != "strong" {
		t.Fatalf("grade:strong = %q,%v want strong,true", got, ok)
	}
}

// TestEvalShadow_OmittedSampleRateDefaultsTo005 is the regression for the
// dead SampleRateValue default: config.yaml documents "0 or omitted falls
// back to the default 0.05", but dispatch read the raw field and treated 0 as
// "eval off" — an eval block without sample_rate silently disabled the L2
// loop. The draw boundary must be 0.05: 0.049 samples, 0.05 does not.
func TestEvalShadow_OmittedSampleRateDefaultsTo005(t *testing.T) {
	for _, tc := range []struct {
		name       string
		draw       float64
		wantShadow bool
	}{
		{"draw below default samples", 0.049, true},
		{"draw at default skips", 0.05, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shadowHit := make(chan struct{}, 1)
			shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case shadowHit <- struct{}{}:
				default:
				}
				w.Write([]byte(`{"id":"shadow-1","choices":[{"message":{"role":"assistant","content":"shadow answer"},"finish_reason":"stop"}]}`))
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
						// sample_rate deliberately omitted: the documented
						// default 0.05 must apply.
						Eval: &configdomain.EvalConfig{
							Pair:  "opposite",
							Judge: configdomain.RouteTarget{Provider: "shadow", Model: "shadow-model", Protocol: "decisions"},
						},
					},
				},
			}
			p := newTestProxy(t, cfg)
			p.providers["primary"] = &testProv{key: "primary"}
			p.providers["shadow"] = &testProv{key: "shadow"}
			p.evalRand = func() float64 { return tc.draw }

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

			post(t, px.URL+"/v1/chat/completions",
				`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)

			if tc.wantShadow {
				select {
				case <-shadowHit:
				case <-time.After(5 * time.Second):
					t.Fatal("shadow upstream was not called though draw 0.049 < default rate 0.05")
				}
			}
			// Close drains admitted shadow work; a dispatched shadow would have
			// hit the upstream by the time Close returns.
			p.Close()
			select {
			case <-shadowHit:
				if !tc.wantShadow {
					t.Fatal("shadow upstream was called though draw 0.05 >= default rate 0.05")
				}
			default:
			}
		})
	}
}

// TestJudgeEvalPairAbortsOnStop is the regression for the judge context not
// being bound to lifecycle stop: WaitBeforeLogDrain could be dragged for the
// full 30s judge budget by one in-flight judge call. Closing stop must cancel
// the judge promptly (channel-asserted, no timing sleeps).
func TestJudgeEvalPairAbortsOnStop(t *testing.T) {
	judgeStarted := make(chan struct{})
	releaseJudge := make(chan struct{})
	var judgeOnce sync.Once
	judgeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		judgeOnce.Do(func() { close(judgeStarted) })
		<-releaseJudge // hang until the test releases us
	}))
	// Release BEFORE server Close (LIFO runs Close first): a canceled
	// request-with-body keeps its upstream connection open (Go transport
	// semantics), so a handler waiting on r.Context() would outlive the
	// assertion and hang Close.
	defer judgeUpstream.Close()
	defer close(releaseJudge)

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"primary": {Provider: testProviderID, OpenAIBaseURL: ""},
			"judge":   {Provider: testProviderID, DecisionsBaseURL: judgeUpstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"alias": {{Provider: "primary", Model: "primary-model", Priority: 1}},
		},
		RoutePolicies: map[string]configdomain.RoutePolicy{
			"alias": {
				Grades: map[string][]configdomain.RouteTarget{
					"cheap": {{Provider: "primary", Model: "primary-model"}},
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
	p.providers["judge"] = &testProv{key: "judge"}

	runtime := p.SnapshotRuntime()
	stop := make(chan struct{})
	type judgeOut struct {
		verdict string
		diags   []requestlog.ConversionDiagnostic
	}
	done := make(chan judgeOut, 1)
	go func() {
		verdict, diags := p.judgeEvalPair(
			runtime, stop, "openai", "alias", "alias",
			configdomain.RouteTarget{Provider: "primary", Model: "primary-model"},
			configdomain.RouteTarget{Provider: "shadow", Model: "shadow-model"},
			[]byte("primary"), []byte("shadow"),
			"req-stop", "agent", "session",
		)
		done <- judgeOut{verdict: verdict, diags: diags}
	}()

	select {
	case <-judgeStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("judge upstream was not called")
	}
	close(stop)
	select {
	case out := <-done:
		if out.verdict != "" {
			t.Fatalf("verdict = %q, want empty (judge canceled by stop)", out.verdict)
		}
		found := false
		for _, d := range out.diags {
			if d.Code == "eval_judge_error" {
				found = true
			}
		}
		if !found {
			t.Fatalf("diags = %+v, want an eval_judge_error diagnostic", out.diags)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("judge did not return promptly after stop — it must not hold the drain window for the full judge budget")
	}
}
