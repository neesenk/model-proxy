package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandler_PprofEndpointIsOptIn: /debug/pprof/ must be unreachable by
// default (falls through to the unknown-path 502) and served when the proxy
// was constructed with MP_PPROF=1.
func TestHandler_PprofEndpointIsOptIn(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	get := func(p *Proxy, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		p.Handler(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}

	p := newTestProxy(t, cfg)
	if rec := get(p, "/debug/pprof/"); rec.Code == http.StatusOK {
		t.Errorf("pprof index served without MP_PPROF: status=%d", rec.Code)
	}

	t.Setenv("MP_PPROF", "1")
	pOn := newTestProxy(t, cfg)
	rec := get(pOn, "/debug/pprof/")
	if rec.Code != http.StatusOK {
		t.Errorf("pprof index: status=%d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "profiles") {
		t.Errorf("pprof index body does not look like the profile list: %q", body)
	}
}
