package targetexec

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"syscall"
	"testing"
)

// r6CancelDoer fails every call with err; the first call also runs onDo, so a
// test can cancel the context before the failure is returned.
type r6CancelDoer struct {
	err   error
	calls int
	onDo  func()
}

func (d *r6CancelDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	if d.calls == 1 && d.onDo != nil {
		d.onDo()
	}
	return nil, d.err
}

// r6BrokenBody answers every Read with a connection-class error: the response
// headers arrived, the body broke mid-read.
type r6BrokenBody struct {
	closed bool
}

func (body *r6BrokenBody) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func (body *r6BrokenBody) Close() error {
	body.closed = true
	return nil
}

// TestR6BufferedLegFastFailConnRedispatch: BufferedLeg is the headless
// counterpart of Executor, and a connection-class blip (EOF / reset / refused
// / HTTP/2 GOAWAY, returned in milliseconds) means the same thing on both —
// an external-path hiccup, not a dead provider, and no response bytes exist
// to duplicate. It takes one same-target redispatch on a fresh connection
// before the failure is handed to the caller, under the same limits as the
// executor: one redispatch per leg, its own retry slot (neither consumed by
// nor consuming the 401 refresh and the 400 param strip), never for a
// timeout, a canceled caller, or a response that already arrived.
func TestR6BufferedLegFastFailConnRedispatch(t *testing.T) {
	// One scripted dispatch: a transport error, or a response.
	type dispatch struct {
		err    error
		status int
		body   string
	}
	const ok = `{"ok":true}`
	const plain = `{"model":"model"}`
	cases := []struct {
		name           string
		request        string // "" = plain
		dispatches     []dispatch
		wantCalls      int
		wantStatus     int
		wantErr        error
		wantRedispatch int
		wantRefreshes  int
		wantLearned    []string
		check          func(t *testing.T, sent [][]byte)
	}{
		{
			name:           "unexpected EOF then 200",
			dispatches:     []dispatch{{err: fastFailConnError(io.ErrUnexpectedEOF)}, {status: http.StatusOK, body: ok}},
			wantCalls:      2,
			wantStatus:     http.StatusOK,
			wantRedispatch: 1,
		},
		{
			name:           "HTTP/2 GOAWAY then 200",
			dispatches:     []dispatch{{err: fastFailConnError(goAwayTestError())}, {status: http.StatusOK, body: ok}},
			wantCalls:      2,
			wantStatus:     http.StatusOK,
			wantRedispatch: 1,
		},
		{
			// Two fast-fails end the leg with the second error: ONE redispatch,
			// never a third dispatch, and no earlier status to report.
			name:           "fast-fail twice returns the second error",
			dispatches:     []dispatch{{err: fastFailConnError(io.ErrUnexpectedEOF)}, {err: fastFailConnError(syscall.ECONNRESET)}},
			wantCalls:      2,
			wantErr:        syscall.ECONNRESET,
			wantRedispatch: 1,
		},
		{
			// A timeout's wall-time budget is already spent.
			name:       "timeout is not redispatched",
			dispatches: []dispatch{{err: fastFailConnError(context.DeadlineExceeded)}},
			wantCalls:  1,
			wantErr:    context.DeadlineExceeded,
		},
		{
			// The slots are independent in this direction too: a 401 refresh
			// that already happened leaves the one redispatch intact, and the
			// redispatch does not buy a second refresh.
			name: "401 refresh then fast-fail",
			dispatches: []dispatch{
				{status: http.StatusUnauthorized, body: `{"error":"expired"}`},
				{err: fastFailConnError(io.ErrUnexpectedEOF)},
				{status: http.StatusOK, body: ok},
			},
			wantCalls:      3,
			wantStatus:     http.StatusOK,
			wantRedispatch: 1,
			wantRefreshes:  1,
			check: func(t *testing.T, sent [][]byte) {
				for i, body := range sent {
					if string(body) != plain {
						t.Errorf("send %d = %s, want the same request on every dispatch", i+1, body)
					}
				}
			},
		},
		{
			// Same for the 400 param strip: exactly one redispatch afterwards,
			// and it resends the stripped request.
			name:    "400 strip then fast-fail",
			request: `{"model":"model","temperature":0.5}`,
			dispatches: []dispatch{
				{status: http.StatusBadRequest, body: `{"error":{"message":"Unsupported parameter: temperature"}}`},
				{err: fastFailConnError(io.ErrUnexpectedEOF)},
				{status: http.StatusOK, body: ok},
			},
			wantCalls:      3,
			wantStatus:     http.StatusOK,
			wantRedispatch: 1,
			wantLearned:    []string{"temperature"},
			check: func(t *testing.T, sent [][]byte) {
				if string(sent[0]) != `{"model":"model","temperature":0.5}` {
					t.Errorf("first send = %s, want the request still carrying the rejected parameter", sent[0])
				}
				if string(sent[1]) != plain || string(sent[2]) != plain {
					t.Errorf("sends after the strip = %s then %s, want the stripped request both times", sent[1], sent[2])
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := tc.request
			if request == "" {
				request = plain
			}
			impl := &bufferedLegProvider{}
			// One slot per dispatch, built here: a response body is single-use,
			// so cases must not share one.
			failures := make([]error, len(tc.dispatches))
			responses := make([]*http.Response, len(tc.dispatches))
			for i, d := range tc.dispatches {
				failures[i] = d.err
				if d.err == nil {
					responses[i] = testResponse(d.status, d.body)
				}
			}
			redispatches := 0
			var learned []string
			doer := &sequenceDoer{errors: failures, responses: responses}
			status, body, err := (BufferedLeg{
				Client:          doer,
				Plan:            bufferedLegPlan(impl),
				LearnParamBlock: func(param string) { learned = append(learned, param) },
				OnRedispatch:    func(error) { redispatches++ },
			}).Do(context.Background(), []byte(request))
			if doer.calls != tc.wantCalls || redispatches != tc.wantRedispatch {
				t.Errorf("Doer calls / redispatches = %d / %d, want %d / %d", doer.calls, redispatches, tc.wantCalls, tc.wantRedispatch)
			}
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
			if tc.wantErr == nil {
				if err != nil || string(body) != ok {
					t.Errorf("err/body = %v / %q, want the redispatched 200", err, body)
				}
			} else if !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want the final transport error wrapping %v", err, tc.wantErr)
			}
			if impl.refreshes != tc.wantRefreshes || !slices.Equal(learned, tc.wantLearned) {
				t.Errorf("refreshes / learned = %d / %v, want %d / %v (the one-shot slots stay independent)", impl.refreshes, learned, tc.wantRefreshes, tc.wantLearned)
			}
			if tc.check != nil {
				tc.check(t, doer.bodies)
			}
		})
	}
}

// TestR6BufferedLegFastFailConnDoesNotConsume401Slot: the redispatch is an
// independent retry slot — a connection blip followed by a stale-key 401 still
// gets its refresh retry, and the leg returns the refreshed 200.
func TestR6BufferedLegFastFailConnDoesNotConsume401Slot(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{
		errors: []error{fastFailConnError(io.ErrUnexpectedEOF), nil, nil},
		responses: []*http.Response{
			nil,
			testResponse(http.StatusUnauthorized, `{"error":"expired"}`),
			testResponse(http.StatusOK, `{"ok":true}`),
		},
	}
	status, body, err := BufferedLeg{Client: doer, Plan: bufferedLegPlan(impl)}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil || status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("status/body/err = %d %q %v, want the refreshed 200", status, body, err)
	}
	if doer.calls != 3 || impl.refreshes != 1 {
		t.Errorf("calls/refreshes = %d/%d, want 3/1 (redispatch, 401, refreshed retry)", doer.calls, impl.refreshes)
	}
}

// TestR6BufferedLegFastFailConnRedispatchResendsSameBody: the redispatch is
// the same request again, built by the same per-send pipeline as the first
// one. The provider rewrite and the param block are both non-identity yet
// idempotent — transforms compound across iterations, so identity ones would
// prove nothing and non-idempotent ones could not resend the same bytes. Each
// runs on both sends (counted), and both sends carry their effect: with the
// pipeline skipped on the redispatch the bytes would still match, because the
// first send's body already went through it.
func TestR6BufferedLegFastFailConnRedispatchResendsSameBody(t *testing.T) {
	impl := &bufferedLegProvider{
		rewriteBody: func(body []byte) []byte {
			// Idempotent: stamps the request once.
			if bytes.Contains(body, []byte(`"rewritten":true`)) {
				return body
			}
			return bytes.Replace(body, []byte(`{`), []byte(`{"rewritten":true,`), 1)
		},
	}
	paramBlocks := 0
	doer := &sequenceDoer{
		errors:    []error{fastFailConnError(io.ErrUnexpectedEOF), nil},
		responses: []*http.Response{nil, testResponse(http.StatusOK, `{}`)},
	}
	_, _, err := (BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
		ApplyParamBlock: func(body []byte) []byte {
			paramBlocks++
			// Idempotent: drops the blocked parameter while it is present.
			return bytes.ReplaceAll(body, []byte(`,"temperature":0.5`), []byte(``))
		},
	}).Do(context.Background(), []byte(`{"model":"model","temperature":0.5}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if doer.calls != 2 || impl.rewrites != 2 || paramBlocks != 2 {
		t.Fatalf("dispatches/rewrites/param blocks = %d/%d/%d, want 2/2/2 (every send goes through the whole pipeline)",
			doer.calls, impl.rewrites, paramBlocks)
	}
	const want = `{"rewritten":true,"model":"model"}`
	if got := string(doer.bodies[0]); got != want {
		t.Errorf("first send = %s, want %s (rewritten, blocked parameter dropped)", got, want)
	}
	if got := string(doer.bodies[1]); got != want {
		t.Errorf("redispatch = %s, want %s (the same rewrite and param block as the first send)", got, want)
	}
}

// TestR6BufferedLegFastFailConnNotRedispatchedWhenAbandoned: two failures
// that look like a dead connection are not one — a caller that canceled the
// leg abandoned it (no redispatch on a dead context), and a body that broke
// after the response headers arrived belongs to a request the upstream already
// took (no second dispatch, the received status is still reported).
func TestR6BufferedLegFastFailConnNotRedispatchedWhenAbandoned(t *testing.T) {
	t.Run("canceled caller", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		doer := &r6CancelDoer{err: fastFailConnError(io.ErrUnexpectedEOF), onDo: cancel}
		status, _, err := BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{})}.Do(ctx, []byte(`{"model":"model"}`))
		if doer.calls != 1 {
			t.Errorf("Doer calls = %d, want 1 (a canceled leg is abandoned, not retried)", doer.calls)
		}
		if status != 0 || !errors.Is(err, io.ErrUnexpectedEOF) || ctx.Err() != context.Canceled {
			t.Errorf("status/err/ctx = %d/%v/%v, want 0 / the transport error / context.Canceled", status, err, ctx.Err())
		}
	})

	t.Run("body broke after the headers", func(t *testing.T) {
		body := &r6BrokenBody{}
		doer := &sequenceDoer{responses: []*http.Response{{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}}}
		status, _, err := BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{})}.Do(context.Background(), []byte(`{"model":"model"}`))
		if doer.calls != 1 {
			t.Errorf("Doer calls = %d, want 1 (the upstream already took this request)", doer.calls)
		}
		if status != http.StatusOK || !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("status/err = %d/%v, want the received 200 and the read error", status, err)
		}
		if !body.closed {
			t.Error("response body was not closed after the read error")
		}
	})
}

// TestR6BufferedLegFastFailRedispatchNotifiesHookOnce: BufferedLeg is
// headless (no logger), so a redispatch is reported through OnRedispatch —
// exactly once per leg, with the connection error that triggered it. When
// the redispatch fails too, the second error is the caller's return value,
// not a second notification. A nil hook is the zero-cost default.
func TestR6BufferedLegFastFailRedispatchNotifiesHookOnce(t *testing.T) {
	cases := []struct {
		name   string
		errors []error
	}{
		{"unexpected EOF then 200", []error{fastFailConnError(io.ErrUnexpectedEOF), nil}},
		{"HTTP/2 GOAWAY then 200", []error{fastFailConnError(goAwayTestError()), nil}},
		{"fast-fail twice", []error{fastFailConnError(io.ErrUnexpectedEOF), fastFailConnError(syscall.ECONNRESET)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var notified []error
			responses := make([]*http.Response, len(tc.errors))
			responses[len(responses)-1] = testResponse(http.StatusOK, `{"ok":true}`)
			doer := &sequenceDoer{errors: tc.errors, responses: responses}
			leg := BufferedLeg{
				Client:       doer,
				Plan:         bufferedLegPlan(&bufferedLegProvider{}),
				OnRedispatch: func(err error) { notified = append(notified, err) },
			}
			leg.Do(context.Background(), []byte(`{"model":"model"}`))
			if doer.calls != 2 {
				t.Fatalf("Doer calls = %d, want 2", doer.calls)
			}
			if len(notified) != 1 {
				t.Fatalf("OnRedispatch calls = %d (%v), want exactly 1", len(notified), notified)
			}
			if notified[0] != tc.errors[0] {
				t.Errorf("OnRedispatch got %v, want the connection error of the dispatch it redispatched (%v)", notified[0], tc.errors[0])
			}
		})
	}

	t.Run("nil hook", func(t *testing.T) {
		doer := &sequenceDoer{
			errors:    []error{fastFailConnError(io.ErrUnexpectedEOF), nil},
			responses: []*http.Response{nil, testResponse(http.StatusOK, `{"ok":true}`)},
		}
		status, body, err := (BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{})}).Do(context.Background(), []byte(`{"model":"model"}`))
		if err != nil || status != http.StatusOK || string(body) != `{"ok":true}` || doer.calls != 2 {
			t.Fatalf("status/body/err/calls = %d %q %v %d, want the nil-hook redispatch to behave like a hooked one", status, body, err, doer.calls)
		}
	})
}

// TestR6BufferedLegFastFailRedispatchHookSilentWithoutRedispatch: no
// redispatch, no notification. A timeout, an error that is not
// connection-class, a canceled caller and a body that broke after the
// headers all fail the leg without a second dispatch, and a clean exchange
// dispatches once — the hook stays untouched in every one of them.
func TestR6BufferedLegFastFailRedispatchHookSilentWithoutRedispatch(t *testing.T) {
	hooked := func(notified *[]error) func(error) {
		return func(err error) { *notified = append(*notified, err) }
	}

	t.Run("timeout", func(t *testing.T) {
		var notified []error
		doer := &sequenceDoer{errors: []error{fastFailConnError(context.DeadlineExceeded)}, responses: []*http.Response{nil}}
		(BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{}), OnRedispatch: hooked(&notified)}).Do(context.Background(), []byte(`{}`))
		if doer.calls != 1 || len(notified) != 0 {
			t.Errorf("calls/notifications = %d/%d, want 1/0", doer.calls, len(notified))
		}
	})

	t.Run("error that is not connection-class", func(t *testing.T) {
		var notified []error
		doer := &sequenceDoer{errors: []error{errors.New("connection reset")}, responses: []*http.Response{nil}}
		(BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{}), OnRedispatch: hooked(&notified)}).Do(context.Background(), []byte(`{}`))
		if doer.calls != 1 || len(notified) != 0 {
			t.Errorf("calls/notifications = %d/%d, want 1/0", doer.calls, len(notified))
		}
	})

	t.Run("canceled caller", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var notified []error
		doer := &r6CancelDoer{err: fastFailConnError(io.ErrUnexpectedEOF), onDo: cancel}
		(BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{}), OnRedispatch: hooked(&notified)}).Do(ctx, []byte(`{}`))
		if doer.calls != 1 || len(notified) != 0 {
			t.Errorf("calls/notifications = %d/%d, want 1/0", doer.calls, len(notified))
		}
	})

	t.Run("body broke after the headers", func(t *testing.T) {
		var notified []error
		doer := &sequenceDoer{responses: []*http.Response{{StatusCode: http.StatusOK, Header: make(http.Header), Body: &r6BrokenBody{}}}}
		(BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{}), OnRedispatch: hooked(&notified)}).Do(context.Background(), []byte(`{}`))
		if doer.calls != 1 || len(notified) != 0 {
			t.Errorf("calls/notifications = %d/%d, want 1/0", doer.calls, len(notified))
		}
	})

	t.Run("clean exchange", func(t *testing.T) {
		var notified []error
		doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{"ok":true}`)}}
		(BufferedLeg{Client: doer, Plan: bufferedLegPlan(&bufferedLegProvider{}), OnRedispatch: hooked(&notified)}).Do(context.Background(), []byte(`{}`))
		if doer.calls != 1 || len(notified) != 0 {
			t.Errorf("calls/notifications = %d/%d, want 1/0", doer.calls, len(notified))
		}
	})
}
