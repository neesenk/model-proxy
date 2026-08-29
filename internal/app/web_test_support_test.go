package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newTestWeb(t *testing.T) (*WebServer, *Proxy) {
	t.Helper()
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := newTestProxy(t, cfg)
	return NewWebServer(p, "test-config.yaml"), p
}

// serveWeb dispatches one request through the mux the WebServer registers on —
// the same routing production uses — for tests that exercise endpoints without
// a listener.
func serveWeb(w *WebServer, recorder *httptest.ResponseRecorder, req *http.Request) {
	mux := http.NewServeMux()
	w.Register(mux)
	mux.ServeHTTP(recorder, req)
}
