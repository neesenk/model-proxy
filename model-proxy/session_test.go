package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestForwardThreadsSessionID verifies Task 2 plumbing: the value of the
// x-claude-code-session-id request header reaches the scheduler via a
// test-only hook (Proxy.scheduleHook). Behavior is unchanged for now
// (decideOrder ignores sessionKey); Task 6 switches the sticky key to it.
//
// Single account (no pool): a singular zhipu_apikey.json is written directly,
// matching the deepseek_forward_test.go pattern — writePoolFile is Task 4.
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
	p := NewProxy(cfg)

	var captured string
	p.scheduleHook = func(sessionKey string) { captured = sessionKey }

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(`{"model":"glm-5"}`)))
	req.Header.Set("x-claude-code-session-id", "sess-XYZ")
	p.forward("openai", httptest.NewRecorder(), req, nextRequestID())

	if captured != "sess-XYZ" {
		t.Fatalf("sessionKey threaded = %q, want sess-XYZ", captured)
	}
}
