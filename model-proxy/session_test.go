package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestForwardThreadsSessionID verifies the session header becomes the live
// sticky key after a successful forward, rather than exposing a test-only
// scheduler callback from production code.
func TestForwardThreadsSessionID(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	keyFile := filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")
	if err := os.WriteFile(keyFile, []byte(`{"api_key":"KEY-A"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: srv.URL, Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"glm-5"}`)))
	req.Header.Set("x-claude-code-session-id", "sess-XYZ")
	w := httptest.NewRecorder()
	p.forward("openai", w, req, nextRequestID())
	if w.Code != http.StatusOK {
		t.Fatalf("forward status = %d, want %d", w.Code, http.StatusOK)
	}

	sticky, ok := p.runtimeState.Sticky("sess-XYZ")
	if !ok || sticky.Provider != "zhipu" {
		t.Fatalf("session sticky = %+v, present=%t; want provider zhipu for sess-XYZ", sticky, ok)
	}
}
