package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestForward_RequestBodyLimit: the inbound body is fully buffered for routing
// and conversion, so anything over max_request_body_bytes must be rejected with
// 413 BEFORE any routing work. The default cap (64 MiB) must keep ordinary
// requests flowing; an explicit small cap must be honored.
func TestForward_RequestBodyLimit(t *testing.T) {
	base := `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
`
	newProxy := func(extra string) *Proxy {
		cfg, err := LoadConfigFromBytes("test", []byte(base+extra))
		if err != nil {
			t.Fatal(err)
		}
		return newTestProxy(t, cfg)
	}
	post := func(p *Proxy, pad int) int {
		body := `{"model":"glm","stream":false,"pad":"` + strings.Repeat("x", pad) + `"}`
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		p.Handler(rec, req)
		return rec.Code
	}

	// Default cap: an ordinary request passes the gate, then fails on the
	// unroutable test upstream with a deterministic 502 — exactly that status
	// proves the gate opened (413 = gate rejected; anything else = new bug).
	p := newProxy("")
	if code := post(p, 1024); code != http.StatusBadGateway {
		t.Errorf("default cap: status=%d, want 502 (gate opened, unroutable upstream)", code)
	}

	// Explicit cap: a body larger than the cap is rejected with 413 up front.
	p = newProxy("max_request_body_bytes: 512\n")
	if code := post(p, 2048); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: status=%d, want 413", code)
	}
	// A body under the explicit cap passes the gate (deterministic 502 again).
	if code := post(p, 16); code != http.StatusBadGateway {
		t.Errorf("small body: status=%d, want 502 (gate opened, unroutable upstream)", code)
	}
}
