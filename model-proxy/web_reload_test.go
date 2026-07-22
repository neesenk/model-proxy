package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandleAccountAdd_ReloadFailureWarning (bug 8): a credential mutation is
// persisted to the pool file BEFORE reload, so a reload failure (config.yaml
// unreadable/invalid — the mutation itself never touches config.yaml) must NOT
// be swallowed as a false success. The handler keeps 2xx (the account IS saved)
// but surfaces a `warning` so the UI can tell the user the runtime is stale until
// config.yaml is fixed + reloaded. The error is also logged for troubleshooting.
func TestHandleAccountAdd_ReloadFailureWarning(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// openai_base_url with no usage_url now validates via the apiKeyValidationURL
	// fallback (GET openai_base_url/models); point it at a reachable mock so the
	// add succeeds and the reload-failure warning path is what's under test.
	valSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer valSrv.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: "+valSrv.URL+"}\n"))
	p := newTestProxy(t, cfg)
	// configFile points at a non-existent path → reload's LoadConfig read fails.
	w := newWebServer(p, "/no/such/config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-test-warning"}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 (account persists even when reload fails): %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID, Status, Warning string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "added" {
		t.Errorf("status=%q want added", out.Status)
	}
	if out.Warning == "" {
		t.Errorf("reload failed but response carries no warning; UI would show false success: %s", rec.Body.String())
	}
}
