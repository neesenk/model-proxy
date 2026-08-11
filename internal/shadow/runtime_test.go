package shadow

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
	"model-proxy/provider"
)

type testProvider struct {
	auth    func(*http.Request) error
	extra   func(*http.Request, string)
	rewrite func(string, []byte, string) (string, []byte)
}

func (providerTest *testProvider) AuthHeaders(request *http.Request) error {
	request.Header.Set("Authorization", "Bearer test-token")
	if providerTest.auth != nil {
		return providerTest.auth(request)
	}
	return nil
}

func (*testProvider) Refresh() error                                   { return nil }
func (*testProvider) Logout() error                                    { return nil }
func (*testProvider) Usage() error                                     { return nil }
func (*testProvider) FetchModels() ([]string, error)                   { return nil, nil }
func (*testProvider) Quota() (*provider.QuotaSnapshot, error)          { return nil, nil }
func (*testProvider) ProbeRequest(string) provider.ProbeRequest        { return provider.ProbeRequest{} }
func (*testProvider) FilterModelIDs(ids []string) ([]string, []string) { return ids, nil }

func (providerTest *testProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	if providerTest.rewrite != nil {
		return providerTest.rewrite(targetURL, body, path)
	}
	return targetURL, body
}

func (providerTest *testProvider) ExtraHeaders(request *http.Request, path string) {
	if providerTest.extra != nil {
		providerTest.extra(request, path)
	}
}

func TestRuntimeDefaultsAndSamplingThresholds(t *testing.T) {
	defaultRuntime := NewRuntime(Options{})
	if cap(defaultRuntime.sem) != defaultMaxConcurrent {
		t.Fatalf("default semaphore capacity = %d, want %d", cap(defaultRuntime.sem), defaultMaxConcurrent)
	}
	if !defaultRuntime.ShouldSample() {
		t.Fatal("nil SampleRate should sample every request")
	}

	zero, one, half := 0.0, 1.0, 0.5
	if NewRuntime(Options{SampleRate: &zero}).ShouldSample() {
		t.Fatal("zero SampleRate sampled")
	}
	if !NewRuntime(Options{SampleRate: &one}).ShouldSample() {
		t.Fatal("one SampleRate did not sample")
	}
	if !newRuntime(Options{SampleRate: &half}, func() float64 { return 0.49 }).ShouldSample() {
		t.Fatal("random value below threshold did not sample")
	}
	if newRuntime(Options{SampleRate: &half}, func() float64 { return 0.5 }).ShouldSample() {
		t.Fatal("random value at threshold sampled")
	}
}

func TestRuntimeTryAcquireAndRelease(t *testing.T) {
	runtime := NewRuntime(Options{MaxConcurrent: 1, Timeout: 123 * time.Millisecond})
	if runtime.client.Timeout != 123*time.Millisecond {
		t.Fatalf("client timeout = %s", runtime.client.Timeout)
	}
	first := runtime.TryAcquire()
	if first == nil {
		t.Fatal("first acquire failed")
	}
	if runtime.TryAcquire() != nil {
		t.Fatal("second acquire exceeded concurrency cap")
	}
	first.Release()
	first.Release()
	second := runtime.TryAcquire()
	if second == nil {
		t.Fatal("acquire after release failed")
	}
	if runtime.TryAcquire() != nil {
		t.Fatal("duplicated release freed the active permit")
	}
	second.Release()
	if len(runtime.sem) != 0 {
		t.Fatalf("semaphore has %d permits after matched releases", len(runtime.sem))
	}
}

func TestExecuteRewritesAndCapturesDetachedRequest(t *testing.T) {
	var gotPath string
	var gotQuery url.Values
	var gotBody []byte
	var gotHeader http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotPath = request.URL.Path
		gotQuery = request.URL.Query()
		gotBody, _ = io.ReadAll(request.Body)
		gotHeader = request.Header.Clone()
		writer.Header().Set("X-Upstream", "yes")
		_, _ = writer.Write([]byte("response"))
	}))
	defer upstream.Close()

	impl := &testProvider{
		rewrite: func(targetURL string, body []byte, path string) (string, []byte) {
			return targetURL + "?provider-rewrite=1", body
		},
		extra: func(request *http.Request, path string) {
			if path != "/v1/chat/completions" {
				t.Errorf("ExtraHeaders path = %q", path)
			}
			request.Header.Set("X-Provider-Extra", "provider")
			request.Header.Set("X-Override", "provider")
		},
	}
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target: configdomain.RouteTarget{Provider: "candidate", Model: "replacement"},
		ProviderConfig: configdomain.Provider{
			Provider:      "test",
			OpenAIBaseURL: upstream.URL + "/base",
			Headers: map[string]string{
				"X-Configured": "configured",
				"X-Override":   "configured",
			},
		},
		Provider:        impl,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	original := []byte(`{"model":"called","messages":[]}`)
	result := NewRuntime(Options{}).Execute(t.Context(), Job{
		Plan: plan, Body: original, CalledModel: "called", MaxBodyBytes: 64,
	})
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if result.Request == nil || result.Response == nil || result.Started.IsZero() {
		t.Fatalf("result missing request/response/start: %+v", result)
	}
	if gotPath != "/base/v1/chat/completions" || gotQuery.Get("provider-rewrite") != "1" {
		t.Fatalf("upstream URL = %q?%s", gotPath, gotQuery.Encode())
	}
	if got := gotHeader.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("Authorization = %q", got)
	}
	if got := gotHeader.Get("X-Configured"); got != "configured" {
		t.Errorf("configured header = %q", got)
	}
	if got := gotHeader.Get("X-Provider-Extra"); got != "provider" {
		t.Errorf("provider header = %q", got)
	}
	if got := gotHeader.Get("X-Override"); got != "provider" {
		t.Errorf("provider-owned header override = %q, want provider", got)
	}
	if got := gotHeader.Get("Content-Type"); got != "application/json" {
		t.Errorf("content type = %q", got)
	}
	if want := `{"messages":[],"model":"replacement"}`; string(gotBody) != want {
		t.Errorf("upstream body = %s, want %s", gotBody, want)
	}
	if string(original) != `{"model":"called","messages":[]}` {
		t.Errorf("original body mutated to %s", original)
	}
	if got := string(result.RequestBody); got != `{"messages":[],"model":"replacement"}` {
		t.Errorf("result request body = %s", got)
	}
	if !bytes.Equal(result.Capture.Body, []byte("response")) || result.Capture.Total != 8 || result.Capture.Truncated {
		t.Errorf("capture = %+v", result.Capture)
	}
}

func TestExecuteConversionFailureMakesNoHTTPCall(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer upstream.Close()
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          configdomain.RouteTarget{Provider: "candidate", Model: "replacement"},
		ProviderConfig:  configdomain.Provider{Provider: "test", AnthropicBaseURL: upstream.URL},
		Provider:        &testProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.Anthropic,
		ClientPath:      "/v1/chat/completions",
	})
	result := NewRuntime(Options{}).Execute(t.Context(), Job{
		Plan: plan, Body: []byte(`{"model":"called","messages":[BAD`), CalledModel: "called",
	})
	if result.Err == nil {
		t.Fatal("conversion failure returned nil error")
	}
	if result.Request != nil || result.Response != nil {
		t.Fatalf("conversion failure built exchange: %+v", result)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("conversion failure made %d HTTP calls", got)
	}
}

func TestExecuteBoundedCapture(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("0123456789"))
	}))
	defer upstream.Close()
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          configdomain.RouteTarget{Provider: "candidate"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: upstream.URL},
		Provider:        &testProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	result := NewRuntime(Options{}).Execute(t.Context(), Job{Plan: plan, Body: []byte(`{}`), MaxBodyBytes: 4})
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if got := string(result.Capture.Body); got != "0123" {
		t.Errorf("captured prefix = %q", got)
	}
	if result.Capture.Total != 10 || !result.Capture.Truncated {
		t.Errorf("capture metadata = %+v", result.Capture)
	}
}

func TestExecutePreservesPartialCaptureOnResponseReadError(t *testing.T) {
	readErr := errors.New("response read failed")
	body := &errorReadCloser{body: []byte("partial"), err: readErr}
	runtime := NewRuntime(Options{})
	runtime.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
			Request:    request,
		}, nil
	})
	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          configdomain.RouteTarget{Provider: "candidate"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://example.test"},
		Provider:        &testProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})

	result := runtime.Execute(t.Context(), Job{
		Plan:         plan,
		Body:         []byte(`{}`),
		MaxBodyBytes: 64,
	})

	if !errors.Is(result.Err, readErr) {
		t.Fatalf("Execute error = %v, want %v", result.Err, readErr)
	}
	if result.Request == nil || result.Response == nil {
		t.Fatalf("partial response exchange missing: %+v", result)
	}
	if got := string(result.Capture.Body); got != "partial" {
		t.Errorf("captured prefix = %q, want partial", got)
	}
	if result.Capture.Total != 7 || result.Capture.Truncated {
		t.Errorf("capture metadata = %+v, want total 7 and not truncated", result.Capture)
	}
	if !body.closed {
		t.Fatal("response body was not closed after read error")
	}
}

func TestRuntimeNilSafety(t *testing.T) {
	var nilRuntime *Runtime
	if nilRuntime.ShouldSample() {
		t.Fatal("nil runtime sampled")
	}
	if permit := nilRuntime.TryAcquire(); permit != nil {
		t.Fatalf("nil runtime acquired permit %+v", permit)
	}
	var nilPermit *Permit
	nilPermit.Release()

	emptyRuntime := &Runtime{}
	if emptyRuntime.ShouldSample() {
		t.Fatal("runtime without semaphore sampled")
	}
	if permit := emptyRuntime.TryAcquire(); permit != nil {
		t.Fatalf("runtime without semaphore acquired permit %+v", permit)
	}
	(&Permit{runtime: emptyRuntime}).Release()
}

func TestExecutePreparationAndTransportErrors(t *testing.T) {
	t.Run("runtime unavailable", func(t *testing.T) {
		var runtime *Runtime
		result := runtime.Execute(t.Context(), Job{})
		if !errors.Is(result.Err, errNoRuntime) || result.Request != nil || result.Response != nil {
			t.Fatalf("nil runtime result = %+v, want errNoRuntime without exchange", result)
		}
		result = (&Runtime{}).Execute(t.Context(), Job{})
		if !errors.Is(result.Err, errNoRuntime) {
			t.Fatalf("runtime without client error = %v, want errNoRuntime", result.Err)
		}
	})

	t.Run("provider unavailable", func(t *testing.T) {
		result := NewRuntime(Options{}).Execute(t.Context(), Job{})
		if !errors.Is(result.Err, errNoProvider) || result.Request != nil || result.Response != nil {
			t.Fatalf("missing provider result = %+v, want errNoProvider without exchange", result)
		}
	})

	t.Run("request construction", func(t *testing.T) {
		plan := targetexec.NewPlan(targetexec.PlanInput{
			Target:          configdomain.RouteTarget{Provider: "candidate"},
			ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://example.test"},
			Provider:        &testProvider{rewrite: func(string, []byte, string) (string, []byte) { return ":", nil }},
			ClientProtocol:  protocol.OpenAI,
			BackendProtocol: protocol.OpenAI,
			ClientPath:      "/v1/chat/completions",
		})
		result := NewRuntime(Options{}).Execute(t.Context(), Job{Plan: plan, Body: []byte(`{}`)})
		if result.Err == nil || result.Request != nil || result.Response != nil {
			t.Fatalf("malformed URL result = %+v, want construction error without exchange", result)
		}
	})

	t.Run("auth", func(t *testing.T) {
		authErr := errors.New("auth failed")
		plan := targetexec.NewPlan(targetexec.PlanInput{
			Target:          configdomain.RouteTarget{Provider: "candidate"},
			ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://example.test"},
			Provider:        &testProvider{auth: func(*http.Request) error { return authErr }},
			ClientProtocol:  protocol.OpenAI,
			BackendProtocol: protocol.OpenAI,
			ClientPath:      "/v1/chat/completions",
		})
		result := NewRuntime(Options{}).Execute(t.Context(), Job{Plan: plan, Body: []byte(`{}`)})
		if !errors.Is(result.Err, authErr) || result.Request == nil || result.Response != nil || !result.Started.IsZero() {
			t.Fatalf("auth result = %+v, want built request and no transport", result)
		}
	})

	t.Run("transport", func(t *testing.T) {
		transportErr := errors.New("transport failed")
		runtime := NewRuntime(Options{})
		runtime.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, transportErr
		})
		plan := targetexec.NewPlan(targetexec.PlanInput{
			Target:          configdomain.RouteTarget{Provider: "candidate"},
			ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://example.test"},
			Provider:        &testProvider{},
			ClientProtocol:  protocol.OpenAI,
			BackendProtocol: protocol.OpenAI,
			ClientPath:      "/v1/chat/completions",
		})
		result := runtime.Execute(nil, Job{Plan: plan, Body: []byte(`{}`)})
		if !errors.Is(result.Err, transportErr) || result.Request == nil || result.Response != nil || result.Started.IsZero() {
			t.Fatalf("transport result = %+v, want prepared exchange with exact error", result)
		}
	})

	t.Run("response close", func(t *testing.T) {
		closeErr := errors.New("response close failed")
		body := &closeErrorReader{Reader: bytes.NewReader([]byte("complete")), err: closeErr}
		runtime := NewRuntime(Options{})
		runtime.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       body,
				Request:    request,
			}, nil
		})
		plan := targetexec.NewPlan(targetexec.PlanInput{
			Target:          configdomain.RouteTarget{Provider: "candidate"},
			ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://example.test"},
			Provider:        &testProvider{},
			ClientProtocol:  protocol.OpenAI,
			BackendProtocol: protocol.OpenAI,
			ClientPath:      "/v1/chat/completions",
		})
		result := runtime.Execute(t.Context(), Job{Plan: plan, Body: []byte(`{}`), MaxBodyBytes: 64})
		if !errors.Is(result.Err, closeErr) || result.Response == nil {
			t.Fatalf("close result = %+v, want response and exact close error", result)
		}
		if got := string(result.Capture.Body); got != "complete" || result.Capture.Total != 8 {
			t.Errorf("capture before close error = %+v", result.Capture)
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

type errorReadCloser struct {
	body   []byte
	err    error
	read   bool
	closed bool
}

func (reader *errorReadCloser) Read(buffer []byte) (int, error) {
	if reader.read {
		return 0, reader.err
	}
	reader.read = true
	return copy(buffer, reader.body), nil
}

func (reader *errorReadCloser) Close() error {
	reader.closed = true
	return nil
}

type closeErrorReader struct {
	*bytes.Reader
	err error
}

func (reader *closeErrorReader) Close() error {
	return reader.err
}
