package targetexec

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

// bufferedLegProvider is a configurable provider.Provider fake for BufferedLeg
// tests: it records RewriteRequest applications and can fail AuthHeaders or
// Refresh on demand.
type bufferedLegProvider struct {
	authErr    error
	refreshErr error
	refreshes  int
	rewrites   int
	// rewriteBody, when set, transforms the body on every RewriteRequest call.
	rewriteBody func([]byte) []byte
}

func (p *bufferedLegProvider) AuthHeaders(*http.Request) error { return p.authErr }
func (p *bufferedLegProvider) Refresh() error {
	p.refreshes++
	return p.refreshErr
}
func (p *bufferedLegProvider) RewriteRequest(targetURL string, body []byte, _ string) (string, []byte) {
	p.rewrites++
	if p.rewriteBody != nil {
		body = p.rewriteBody(body)
	}
	return targetURL, body
}
func (*bufferedLegProvider) Logout() error                           { return nil }
func (*bufferedLegProvider) Usage() error                            { return nil }
func (*bufferedLegProvider) FetchModels() ([]string, error)          { return nil, nil }
func (*bufferedLegProvider) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (*bufferedLegProvider) ProbeRequest(string) provider.ProbeRequest {
	return provider.ProbeRequest{}
}
func (*bufferedLegProvider) ExtraHeaders(*http.Request, string)               {}
func (*bufferedLegProvider) FilterModelIDs(ids []string) ([]string, []string) { return ids, nil }

func bufferedLegPlan(provider provider.Provider) Plan {
	return NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test/"},
		Provider:        provider,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
}

func TestBufferedLegSuccessPassthrough(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusOK, `{"ok":true}`),
	}}
	capture := &BufferedLegExchange{}
	status, body, err := BufferedLeg{
		Client:  doer,
		Plan:    bufferedLegPlan(impl),
		Capture: capture,
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Errorf("status/body = %d %q, want 200 {\"ok\":true}", status, body)
	}
	if doer.calls != 1 {
		t.Fatalf("Doer calls = %d, want 1", doer.calls)
	}
	if capture.Request == nil || capture.Response == nil {
		t.Fatal("Capture not populated")
	}
	if capture.Request.Method != http.MethodPost {
		t.Errorf("captured method = %s, want POST", capture.Request.Method)
	}
	// BaseURL trailing slash is trimmed before the upstream path joins.
	if capture.Request.URL.String() != "https://upstream.test/v1/chat/completions" {
		t.Errorf("captured URL = %s", capture.Request.URL.String())
	}
	if got := capture.Request.Header.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q, want application/json", got)
	}
	if string(capture.SentBody) != `{"model":"model"}` {
		t.Errorf("SentBody = %q", capture.SentBody)
	}
}

func TestBufferedLegAuthRefreshRetry(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusUnauthorized, `{"error":"expired"}`),
		testResponse(http.StatusOK, `{"ok":true}`),
	}}
	status, body, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK || string(body) != `{"ok":true}` {
		t.Errorf("status/body = %d %q, want 200 {\"ok\":true}", status, body)
	}
	if impl.refreshes != 1 || doer.calls != 2 {
		t.Errorf("refreshes/calls = %d/%d, want 1/2", impl.refreshes, doer.calls)
	}
}

func TestBufferedLegAuthRefreshFailureReturns401(t *testing.T) {
	impl := &bufferedLegProvider{refreshErr: errors.New("refresh denied")}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusUnauthorized, `{"error":"expired"}`),
	}}
	status, body, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusUnauthorized || string(body) != `{"error":"expired"}` {
		t.Errorf("status/body = %d %q, want the 401 response", status, body)
	}
	if impl.refreshes != 1 || doer.calls != 1 {
		t.Errorf("refreshes/calls = %d/%d, want 1/1 (no retry after failed refresh)", impl.refreshes, doer.calls)
	}
}

func TestBufferedLegUnsupportedParamLearnStripRetry(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusBadRequest, `{"error":{"message":"Unsupported parameter: temperature"}}`),
		testResponse(http.StatusOK, `{"ok":true}`),
	}}
	var learned, stripped []string
	status, _, err := BufferedLeg{
		Client:          doer,
		Plan:            bufferedLegPlan(impl),
		LearnParamBlock: func(param string) { learned = append(learned, param) },
		OnStripParam:    func(param string) { stripped = append(stripped, param) },
	}.Do(context.Background(), []byte(`{"model":"model","temperature":0.5}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200 after strip retry", status)
	}
	if len(learned) != 1 || learned[0] != "temperature" {
		t.Errorf("learned = %v, want [temperature]", learned)
	}
	if len(stripped) != 1 || stripped[0] != "temperature" {
		t.Errorf("OnStripParam = %v, want [temperature]", stripped)
	}
	if doer.calls != 2 {
		t.Fatalf("Doer calls = %d, want 2", doer.calls)
	}
	if strings.Contains(string(doer.bodies[1]), "temperature") {
		t.Errorf("retry body still carries stripped param: %s", doer.bodies[1])
	}
}

func TestBufferedLegUnsupportedParamStripNoChangeNoRetry(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusBadRequest, `{"error":{"message":"Unsupported parameter: top_k"}}`),
	}}
	var learned []string
	stripHooks := 0
	status, body, err := BufferedLeg{
		Client:          doer,
		Plan:            bufferedLegPlan(impl),
		LearnParamBlock: func(param string) { learned = append(learned, param) },
		OnStripParam:    func(string) { stripHooks++ },
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusBadRequest || !strings.Contains(string(body), "top_k") {
		t.Errorf("status/body = %d %q, want the 400 response returned", status, body)
	}
	if len(learned) != 1 || learned[0] != "top_k" {
		t.Errorf("learned = %v, want [top_k] (learning happens even when strip is a no-op)", learned)
	}
	if stripHooks != 0 {
		t.Errorf("OnStripParam calls = %d, want 0 (no strip happened)", stripHooks)
	}
	if doer.calls != 1 {
		t.Errorf("Doer calls = %d, want 1 (unchanged strip must not retry)", doer.calls)
	}
}

func TestBufferedLegTransportError(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &errorDoer{err: errors.New("connection reset")}
	status, _, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err == nil || err.Error() != "connection reset" {
		t.Fatalf("err = %v, want the transport error", err)
	}
	var buildErr *BufferedLegBuildError
	if errors.As(err, &buildErr) {
		t.Fatal("transport error must not classify as a pre-wire build error")
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 (no upstream response)", status)
	}
}

func TestBufferedLegCancellationDistinguishable(t *testing.T) {
	impl := &bufferedLegProvider{}
	ctx, cancel := context.WithCancel(context.Background())
	doer := &errorDoer{err: context.Canceled, onDo: cancel}
	status, _, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(ctx, []byte(`{"model":"model"}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if status != 0 || ctx.Err() != context.Canceled {
		t.Errorf("status/ctx.Err = %d/%v, want 0/context.Canceled so the caller classifies the cancel", status, ctx.Err())
	}
}

func TestBufferedLegRetryTransportErrorKeepsLastStatus(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &flakyDoer{responses: []*http.Response{
		testResponse(http.StatusUnauthorized, `{"error":"expired"}`),
	}, err: errors.New("connection reset")}
	status, _, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err == nil {
		t.Fatal("want the second attempt's transport error")
	}
	if status != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (the response that triggered the retry)", status)
	}
}

func TestBufferedLegBodyCapHonored(t *testing.T) {
	impl := &bufferedLegProvider{}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusOK, `{"ok":true,"padding":"xxxxx"}`),
	}}
	status, body, err := BufferedLeg{
		Client:  doer,
		Plan:    bufferedLegPlan(impl),
		MaxBody: 8,
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if len(body) != 8 {
		t.Errorf("body length = %d, want the 8-byte MaxBody cap", len(body))
	}
}

func TestBufferedLegAuthErrorWrapped(t *testing.T) {
	authErr := errors.New("no credential")
	impl := &bufferedLegProvider{authErr: authErr}
	doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{}`)}}
	status, _, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	if err == nil || err.Error() != "auth: no credential" {
		t.Fatalf("err = %v, want \"auth: no credential\"", err)
	}
	var buildErr *BufferedLegBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("err type = %T, want *BufferedLegBuildError (pre-wire failures record no provider failure)", err)
	}
	if !errors.Is(err, authErr) {
		t.Error("errors.Is must unwrap through BufferedLegBuildError to the auth cause")
	}
	if status != 0 {
		t.Errorf("status = %d, want 0 (pre-upstream failure)", status)
	}
	if doer.calls != 0 {
		t.Errorf("Doer calls = %d, want 0", doer.calls)
	}
}

func TestBufferedLegPerIterationRewriteAndParamBlock(t *testing.T) {
	impl := &bufferedLegProvider{
		rewriteBody: func(body []byte) []byte { return append(body, 'R') },
	}
	doer := &sequenceDoer{responses: []*http.Response{
		testResponse(http.StatusUnauthorized, `{"error":"expired"}`),
		testResponse(http.StatusOK, `{"ok":true}`),
	}}
	paramBlocks := 0
	_, _, err := BufferedLeg{
		Client: doer,
		Plan:   bufferedLegPlan(impl),
		ApplyParamBlock: func(body []byte) []byte {
			paramBlocks++
			return append(body, 'P')
		},
	}.Do(context.Background(), []byte(`{}`))
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if impl.rewrites != 2 || paramBlocks != 2 {
		t.Errorf("rewrites/paramBlocks = %d/%d, want 2/2 (re-applied every iteration)", impl.rewrites, paramBlocks)
	}
	// Iteration 2 rewrites the CURRENT body (iteration 1's rewritten +
	// param-blocked bytes), so the effects compound.
	if got, want := string(doer.bodies[0]), "{}RP"; got != want {
		t.Errorf("first send body = %q, want %q", got, want)
	}
	if got, want := string(doer.bodies[1]), "{}RPRP"; got != want {
		t.Errorf("retry send body = %q, want %q", got, want)
	}
}

func TestBufferedLegFailClosed(t *testing.T) {
	if _, _, err := (BufferedLeg{Plan: bufferedLegPlan(&bufferedLegProvider{})}).Do(context.Background(), nil); err == nil {
		t.Error("nil Client must fail closed")
	}
	plan := bufferedLegPlan(nil)
	if _, _, err := (BufferedLeg{Client: &sequenceDoer{}, Plan: plan}).Do(context.Background(), nil); err == nil {
		t.Error("nil provider implementation must fail closed")
	}
}

func TestBufferedLegRequestBuildError(t *testing.T) {
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "http://[::1]:badport"},
		Provider:        &bufferedLegProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	doer := &sequenceDoer{responses: []*http.Response{testResponse(http.StatusOK, `{}`)}}
	status, _, err := BufferedLeg{
		Client: doer,
		Plan:   plan,
	}.Do(context.Background(), []byte(`{"model":"model"}`))
	var buildErr *BufferedLegBuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("err = %v (%T), want *BufferedLegBuildError", err, err)
	}
	if status != 0 || doer.calls != 0 {
		t.Errorf("status/calls = %d/%d, want 0/0 (nothing reached the wire)", status, doer.calls)
	}
}

// errorDoer always fails; onDo runs inside Do so a test can cancel the context
// before the error is returned (caller-side cancellation classification).
type errorDoer struct {
	err  error
	onDo func()
}

func (d *errorDoer) Do(*http.Request) (*http.Response, error) {
	if d.onDo != nil {
		d.onDo()
	}
	return nil, d.err
}

// flakyDoer serves the queued responses, then fails every further call.
type flakyDoer struct {
	responses []*http.Response
	err       error
	calls     int
}

func (d *flakyDoer) Do(request *http.Request) (*http.Response, error) {
	if d.calls < len(d.responses) {
		response := d.responses[d.calls]
		d.calls++
		return response, nil
	}
	d.calls++
	return nil, d.err
}
