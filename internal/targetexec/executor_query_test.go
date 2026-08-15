package targetexec

import (
	"net/http"
	"net/http/httptest"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

func TestStripInternalQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"force_provider=zhipu", ""},
		{"force_provider=zhipu&beta=1", "beta=1"},
		{"beta=1&force_provider=zhipu", "beta=1"},
		{"a=1&b=2", "a=1&b=2"}, // untouched: byte transparency
		{"", ""},
		{"FORCE_PROVIDER=zhipu&b=2", "b=2"}, // case-insensitive like Query().Get
	}
	for _, c := range cases {
		if got := stripInternalQuery(c.in); got != c.want {
			t.Errorf("stripInternalQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

type recordingDoer struct{ urls []string }

func (d *recordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.urls = append(d.urls, request.URL.String())
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: http.NoBody}, nil
}

// Regression: ?force_provider=x is a PROXY control parameter (replay's
// --to). It used to be forwarded verbatim to the upstream API, leaking an
// internal knob (and tripping strict-argument upstreams). The executor must
// drop it while preserving every other query byte.
func TestExecutorDoesNotForwardInternalQuery(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions?force_provider=zhipu&beta=anthropic-beta-1", nil)
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	attempt := NewAttempt(Runtime{}, plan,
		Exchange{Request: request, Writer: httptest.NewRecorder(), Body: []byte(`{"model":"model"}`)},
		Scope{}, Policy{LastTarget: true})
	doer := &recordingDoer{}

	result := (Executor{Client: doer}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result not committed: %+v", result)
	}
	if len(doer.urls) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(doer.urls))
	}
	got := doer.urls[0]
	if want := "https://upstream.test/v1/chat/completions?beta=anthropic-beta-1"; got != want {
		t.Fatalf("upstream URL = %q, want %q (force_provider must not leak)", got, want)
	}
}

// Regression (RFC 9110 §7.6.1): hop-by-hop headers (Connection + everything
// it names, Trailer, …) belong to the proxy↔upstream connection and must
// never reach the client, while end-to-end headers pass through.
func TestExecutorStripsHopByHopResponseHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	attempt := NewAttempt(Runtime{}, plan,
		Exchange{Request: request, Writer: httptest.NewRecorder(), Body: []byte(`{"model":"model"}`)},
		Scope{}, Policy{LastTarget: true})
	response := &http.Response{StatusCode: 200, Body: http.NoBody, Header: http.Header{}}
	response.Header.Set("content-type", "application/json")
	response.Header.Set("connection", "keep-alive")
	response.Header.Add("connection", "x-upstream-private")
	response.Header.Set("x-upstream-private", "secret-token")
	response.Header.Set("trailer", "x-debug")
	response.Header.Set("x-request-id", "end-to-end")
	doer := &staticResponseDoer{response: response}

	result := (Executor{Client: doer}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result not committed: %+v", result)
	}
	writer := attempt.Exchange().Writer.(*httptest.ResponseRecorder)
	headers := writer.Header()
	for _, forbidden := range []string{"Connection", "Keep-Alive", "X-Upstream-Private", "Trailer"} {
		if got := headers.Get(forbidden); got != "" {
			t.Errorf("hop-by-hop header %q forwarded to client: %q", forbidden, got)
		}
	}
	if got := headers.Get("x-request-id"); got != "end-to-end" {
		t.Errorf("end-to-end header dropped: %q", got)
	}
	if got := headers.Get("content-type"); got != "application/json" {
		t.Errorf("content-type = %q", got)
	}
}

type staticResponseDoer struct{ response *http.Response }

func (d *staticResponseDoer) Do(*http.Request) (*http.Response, error) {
	return d.response, nil
}

// Regression: whitelisted request headers are legal multi-valued — collapsing
// them to the first value silently dropped beta-capability declarations.
func TestCopyHeaderWhitelistKeepsAllValues(t *testing.T) {
	src := http.Header{}
	src.Add("anthropic-beta", "context-1m-2025-08-07")
	src.Add("anthropic-beta", "interleaved-thinking-2025-05-14")
	dst := http.Header{}
	copyHeaderWhitelist(dst, src, "content-type", "anthropic-beta")
	if got := dst.Values("anthropic-beta"); len(got) != 2 {
		t.Fatalf("anthropic-beta values = %v, want both", got)
	}
	if _, ok := dst["Content-Type"]; ok {
		t.Error("absent header should not be created")
	}
}
