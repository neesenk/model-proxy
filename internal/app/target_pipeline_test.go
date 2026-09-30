package app

import (
	"bytes"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

// TestCaptureResponseProgressWhenSubscribed verifies that CaptureResponse wraps
// the body with a progress tap when the event hub has subscribers, and that
// reading the body emits progress events for the right request id.
func TestCaptureResponseProgressWhenSubscribed(t *testing.T) {
	p := &Proxy{processServices: processServices{events: observeevents.NewHub(), evalRand: rand.Float64}}
	effects := targetExecutionEffects{proxy: p, generation: 1}

	ch, _, cancel := p.events.Subscribe()
	defer cancel()

	bodyText := bytes.Repeat([]byte("a"), 100)
	src := io.NopCloser(bytes.NewReader(bodyText))
	wrapped := effects.CaptureResponse(src, sampleAttempt("req-progress"))

	// Drain the wrapped body.
	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read wrapped body: %v", err)
	}
	if !bytes.Equal(got, bodyText) {
		t.Fatalf("wrapped body changed: got %d bytes, want %d", len(got), len(bodyText))
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var progress []observeevents.Event
	collectDone := time.After(50 * time.Millisecond)
loop:
	for {
		select {
		case e := <-ch:
			if e.Type == "progress" {
				progress = append(progress, e)
			}
		case <-collectDone:
			break loop
		}
	}

	if len(progress) == 0 {
		t.Fatal("expected progress events, got none")
	}
	first := progress[0]
	if first.RequestID != "req-progress" {
		t.Fatalf("progress request_id = %q, want req-progress", first.RequestID)
	}
	last := progress[len(progress)-1]
	if last.ReceivedBytes != int64(len(bodyText)) {
		t.Fatalf("final received_bytes = %d, want %d", last.ReceivedBytes, len(bodyText))
	}
}

// TestCaptureResponseProgressNoSubscriberFastPath verifies that CaptureResponse
// returns the original body unchanged when no one is subscribed to the events hub.
func TestCaptureResponseProgressNoSubscriberFastPath(t *testing.T) {
	p := &Proxy{processServices: processServices{events: observeevents.NewHub(), evalRand: rand.Float64}}
	effects := targetExecutionEffects{proxy: p, generation: 1}

	src := io.NopCloser(bytes.NewReader([]byte("hello")))
	wrapped := effects.CaptureResponse(src, sampleAttempt("req-no-sub"))
	if wrapped != src {
		t.Fatal("CaptureResponse wrapped body when no subscribers exist")
	}
}

func sampleAttempt(requestID string) targetexec.AttemptDTO {
	return targetexec.AttemptDTO{
		Target:   configdomain.RouteTarget{Provider: "teststatic", Model: "test-model"},
		Protocol: protocol.OpenAI,
		Request:  httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		Response: &http.Response{StatusCode: 200, Header: make(http.Header)},
		Scope: targetexec.Scope{
			Agent: "curl",
			Log: targetexec.LogContext{
				RequestID: requestID,
				Exposed:   "test-model",
			},
		},
	}
}

// TestCaptureResponseEvalBodyStoreUsesRequestSnapshot is the regression for
// the eval body store re-reading reload-owned config in the post-commit
// bodycapture callback: the store decision must come from the REQUEST's
// snapshot config (effects.cfg), the same one dispatchEvalShadow samples
// from. A reload that removes eval between commit and callback must not
// silently drop a sampled body — and a reload that ADDS eval must not retain
// bodies for a route the request's own generation never evaluated.
func TestCaptureResponseEvalBodyStoreUsesRequestSnapshot(t *testing.T) {
	evalCfg := func() *configdomain.Config {
		return &configdomain.Config{
			RoutePolicies: map[string]configdomain.RoutePolicy{
				"test-model": {Eval: &configdomain.EvalConfig{
					SampleRate: 1.0,
					Judge:      configdomain.RouteTarget{Provider: "judge", Model: "jev", Protocol: "decisions"},
				}},
			},
		}
	}
	for _, tc := range []struct {
		name        string
		snapshotCfg *configdomain.Config // the request's own generation
		liveCfg     *configdomain.Config // the reload-owned current generation
		wantStored  bool
	}{
		{"snapshot has eval, reload removed it", evalCfg(), &configdomain.Config{}, true},
		{"snapshot has no eval, reload added it", &configdomain.Config{}, evalCfg(), false},
		{"both generations have eval", evalCfg(), evalCfg(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &Proxy{
				generationState: generationState{cfg: tc.liveCfg},
				processServices: processServices{
					evalPrimaryBodies: newEvalBodyCache(evalBodyCacheMaxEntries, evalBodyCacheMaxBytes),
					evalRand:          rand.Float64,
				},
			}
			p.reqLog = requestlog.New(requestlog.Options{
				Directory: t.TempDir(), MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
			})
			effects := targetExecutionEffects{proxy: p, cfg: tc.snapshotCfg, generation: 1}

			src := io.NopCloser(bytes.NewReader([]byte("primary answer")))
			wrapped := effects.CaptureResponse(src, sampleAttempt("req-eval-gen"))
			if _, err := io.ReadAll(wrapped); err != nil {
				t.Fatalf("read wrapped body: %v", err)
			}
			if err := wrapped.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			stored := p.evalPrimaryBodies.Retrieve("req-eval-gen") != nil
			if stored != tc.wantStored {
				t.Fatalf("stored = %v, want %v (decision must follow the request snapshot, not the live config)", stored, tc.wantStored)
			}
		})
	}
}
