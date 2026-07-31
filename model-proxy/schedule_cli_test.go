package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- cmdSchedule success path: mock daemon returning valid JSON ---
//
// cmdSchedule does http.Get("http://"+cfg.listen+"/debug/schedule"). We point
// cfg.listen at a local mock server that returns a valid schedule JSON, then
// call cmdSchedule in-process and assert it prints the first-choice provider.

func TestCmdSchedule_ParsesDaemonResponse(t *testing.T) {
	// Stand up a tiny mock daemon.
	mock := newMockScheduleDaemon(t, "glm-5.2", "aqp")
	defer mock.Close()

	// Write a config whose listen matches the mock.
	cfgPath := writeTempConfig(t, "listen: "+mock.listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n")

	// cmdSchedule reads cliframework.ConfigPath(args) and hits the daemon. In-process.
	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "glm-5.2") {
		t.Errorf("schedule output missing model glm-5.2:\n%s", out)
	}
	if !strings.Contains(out, "aqp") {
		t.Errorf("schedule output missing provider aqp:\n%s", out)
	}
}

// mockScheduleDaemon is a minimal /debug/schedule server for cmdSchedule tests.
type mockScheduleDaemon struct {
	*httptest.Server
	listen string
}

func newMockScheduleDaemon(t *testing.T, model, first string) *mockScheduleDaemon {
	t.Helper()
	// Capture the address the test server binds to.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/schedule" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		// Minimal valid schedule payload matching cmdSchedule's parser.
		body := `{"models":{"` + model + `":{"first":"` + first + `","ordered":[{"provider":"` + first + `","priority":1,"tier":"unknown","surplus":0,"available":true,"peak":false}],"sticky":"` + first + `","sticky_dwell_remaining_sec":300}}}`
		w.Write([]byte(body))
	}))
	// Derive the host:port from srv.URL (http://127.0.0.1:PORT).
	listen := strings.TrimPrefix(srv.URL, "http://")
	return &mockScheduleDaemon{Server: srv, listen: listen}
}

// --- cmdSchedule: no routes → "(no routes)" ---

func TestCmdSchedule_NoRoutes(t *testing.T) {
	// Mock daemon returning an empty models map.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"models":{}}`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "no routes") {
		t.Errorf("schedule empty models: want '(no routes)':\n%s", out)
	}
}

// --- cmdSchedule: daemon returns non-200 → exits non-zero ---

func TestCmdSchedule_DaemonError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`internal error`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	// cmdSchedule os.Exit(1) on non-200 — run in subprocess.
	_, stderr, code := runCLI(t, "schedule", cfgPath)
	if code == 0 {
		t.Error("schedule daemon 500: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "HTTP 500") {
		t.Errorf("schedule daemon 500 stderr missing 'HTTP 500':\n%s", stderr)
	}
}

// --- schedule: live daemon payload rendered with pool grouping ---

// TestCmdSchedule_PoolGrouping stands up a mock /debug/schedule daemon whose
// payload describes a 2-account zhipu pool (virtuals carrying pool_parent), and
// asserts cmdSchedule's text output groups them under a "pool:" header.
func TestCmdSchedule_PoolGrouping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/schedule" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("content-type", "application/json")
		// Payload mirrors what scheduleStatus would emit for a 2-account pool.
		w.Write([]byte(`{"models":{"glm-5.2":{"first":"zhipu#acc-a","ordered":[{"provider":"zhipu#acc-a","pool_parent":"zhipu","priority":1,"tier":"plan","surplus":0.5,"available":true,"peak":false},{"provider":"zhipu#acc-b","pool_parent":"zhipu","priority":1,"tier":"plan","surplus":0.4,"available":true,"peak":false}],"pools":[{"parent":"zhipu","accounts":2,"available":2}]}}}`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n")

	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "glm-5.2") {
		t.Errorf("schedule output missing model:\n%s", out)
	}
	// Pool header: "pool: zhipu (2 accounts)" or similar — at minimum the
	// parent name + account count must appear together so a human can see it's
	// a pool.
	if !strings.Contains(out, "pool:") {
		t.Errorf("schedule output missing 'pool:' header:\n%s", out)
	}
	if !strings.Contains(out, "zhipu") {
		t.Errorf("schedule output missing parent name zhipu:\n%s", out)
	}
	if !strings.Contains(out, "2 accounts") {
		t.Errorf("schedule output missing '2 accounts':\n%s", out)
	}
	// Virtual ids are still listed (each carries its own surplus).
	if !strings.Contains(out, "zhipu#acc-a") || !strings.Contains(out, "zhipu#acc-b") {
		t.Errorf("schedule output missing virtual ids:\n%s", out)
	}
}

// TestCmdSchedule_NonPooledUnchanged verifies a non-pooled schedule payload
// (no pool_parent, no pools) renders without a pool header.
func TestCmdSchedule_NonPooledUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"models":{"m":{"first":"aqp","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":0,"available":true,"peak":false}]}}}`))
	}))
	defer srv.Close()
	listen := strings.TrimPrefix(srv.URL, "http://")
	cfgPath := writeTempConfig(t, "listen: "+listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	out := grabStdout(t, func() { cmdSchedule([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "aqp") {
		t.Errorf("schedule output missing provider aqp:\n%s", out)
	}
	if strings.Contains(out, "pool:") {
		t.Errorf("non-pooled schedule should not emit pool header:\n%s", out)
	}
}
