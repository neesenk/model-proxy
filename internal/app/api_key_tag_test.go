package app

// api_key_tag_test.go — composition coverage for the forward-key dimension:
// a request presented with a configured key is attributed to the key's
// sha256 id in the request log (never the token), an unauthenticated
// loopback request carries no id, and a bad key never reaches the pipeline.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/testsyn"
	"model-proxy/internal/webauth"
)

func apiKeyFixture(t *testing.T) (*Proxy, string, string) {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)

	keyFile := filepath.Join(t.TempDir(), "api_keys")
	if err := os.WriteFile(keyFile, []byte("# @label rig\nrig-key-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &configdomain.Config{
		Listen: "100.81.1.4:15722",
		Providers: map[string]configdomain.Provider{
			"a": {Provider: testProviderID, OpenAIBaseURL: upstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m": {{Provider: "a", Model: "m"}},
		},
		Web: configdomain.WebConfig{Auth: configdomain.WebAuthConfig{
			AdminTokenFile: filepath.Join(t.TempDir(), "admin"),
			APIKeysFile:    keyFile,
		}},
	}
	os.WriteFile(cfg.Web.Auth.AdminTokenFile, []byte("admin-tok\n"), 0o600)

	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "ka"}

	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	t.Cleanup(p.Close)

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)
	return p, px.URL, logDir
}

func readAPIKeyRecords(t *testing.T, logDir string) []requestlog.Record {
	t.Helper()
	return allRecords(t, logDir)
}

// TestAPIKeyTaggedInRequestLog: the authenticated request's record carries
// api_key = sha256(rig-key-1)[:8]; the token itself never appears.
func TestAPIKeyTaggedInRequestLog(t *testing.T) {
	_, base, logDir := apiKeyFixture(t)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "rig-key-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// The record is enqueued after the streamed response reaches the client
	// (forward's post-commit path); wait for it via the sanctioned poll —
	// under -race a single immediate read otherwise loses that race and sees
	// no records.
	var records []requestlog.Record
	testsyn.WaitUntil(t, "request-log record for the forwarded request", func() bool {
		records = readAPIKeyRecords(t, logDir)
		return len(records) > 0
	})
	wantID := webauth.KeyID("rig-key-1")
	tagged := false
	for _, rec := range records {
		if rec.APIKeyID == wantID {
			tagged = true
		}
	}
	if !tagged {
		t.Errorf("no record carries api_key=%s: %+v", wantID, records)
	}
	// The raw token never lands in the log.
	for _, rec := range records {
		b, _ := json.Marshal(rec)
		if strings.Contains(string(b), "rig-key-1") {
			t.Fatalf("raw token leaked into the request log: %s", b)
		}
	}
}

// TestAPIKeyBadKeyRejectedNoLog: a wrong key answers 401 at the gate and
// produces no request-log record (the request never entered the pipeline).
func TestAPIKeyBadKeyRejectedNoLog(t *testing.T) {
	_, base, logDir := apiKeyFixture(t)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "wrong")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if records := readAPIKeyRecords(t, logDir); len(records) != 0 {
		t.Errorf("gate-rejected request must not be logged: %+v", records)
	}
}

// TestAPIKeyLoopbackUntagged: with auth disabled (no api_keys_file) the
// request still serves and the record carries no api_key field.
func TestAPIKeyLoopbackUntagged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(upstream.Close)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: testProviderID, OpenAIBaseURL: upstream.URL},
		},
		Routes: map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Model: "m"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "ka"}
	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	t.Cleanup(p.Close)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	t.Cleanup(px.Close)

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	for _, rec := range readAPIKeyRecords(t, logDir) {
		if rec.APIKeyID != "" {
			t.Errorf("unauthenticated loopback record carries api_key: %+v", rec)
		}
	}
}
