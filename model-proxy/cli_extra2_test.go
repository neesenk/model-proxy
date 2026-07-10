package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cli_extra2_test.go covers cmdConfig init/print, cmdSchedule's JSON-parse
// success path (via a mock daemon), and statusColor's default branch.

// --- statusColor: all branches ---

func TestStatusColor_AllBranches(t *testing.T) {
	// colorEnabled is off in tests (stdout not a tty), so statusColor returns s.
	if got := statusColor(200, "ok"); got != "ok" {
		t.Errorf("statusColor(200)=%q want ok", got)
	}
	if got := statusColor(301, "redir"); got != "redir" {
		t.Errorf("statusColor(301)=%q want redir", got)
	}
	if got := statusColor(404, "nf"); got != "nf" {
		t.Errorf("statusColor(404)=%q want nf", got)
	}
	if got := statusColor(500, "err"); got != "err" {
		t.Errorf("statusColor(500)=%q want err", got)
	}
	if got := statusColor(100, "info"); got != "info" {
		t.Errorf("statusColor(100)=%q want info (default branch)", got)
	}
	if got := statusColor(0, "zero"); got != "zero" {
		t.Errorf("statusColor(0)=%q want zero (default branch)", got)
	}
}

// --- cmdConfig init: writes config.yaml in the CWD ---

func TestCLI_ConfigInit(t *testing.T) {
	dir := t.TempDir()
	// The subprocess runs `config init` which writes "config.yaml" in its CWD.
	// We can't set CWD via runCLI directly; instead, use the helper subprocess
	// but chdir via a wrapper. Simpler: call cmdConfig init in-process in a temp
	// CWD (it doesn't os.Exit on success).
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	cmdConfig([]string{"init"})
	data, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("config init did not write config.yaml: %v", err)
	}
	if !strings.Contains(string(data), "providers:") {
		t.Errorf("config init wrote unexpected content:\n%s", data)
	}
}

// --- cmdConfig print: prints listen + providers + routes ---

func TestCLI_ConfigPrint(t *testing.T) {
	cfgPath := writeTempConfig(t, minimalConfig)
	// Run in-process: cmdConfig print reads configPath(args[1:]) where args[0]=="print".
	out := grabStdout(t, func() {
		cmdConfig([]string{"print", "--config", cfgPath})
	})
	if !strings.Contains(out, "listen:") || !strings.Contains(out, "aqp") {
		t.Errorf("config print missing content:\n%s", out)
	}
}

// --- cmdSchedule success path: mock daemon returning valid JSON ---
//
// cmdSchedule does http.Get("http://"+cfg.Listen+"/debug/schedule"). We point
// cfg.Listen at a local mock server that returns a valid schedule JSON, then
// call cmdSchedule in-process and assert it prints the first-choice provider.

func TestCmdSchedule_ParsesDaemonResponse(t *testing.T) {
	// Stand up a tiny mock daemon.
	mock := newMockScheduleDaemon(t, "glm-5.2", "aqp")
	defer mock.Close()

	// Write a config whose listen matches the mock.
	cfgPath := writeTempConfig(t, "listen: "+mock.listen+"\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n")

	// cmdSchedule reads configPath(args) and hits the daemon. In-process.
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

// --- cmdModels refresh: no provider → usage + available providers ---

func TestCLI_ModelsRefreshNoProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "models", cfg, "refresh")
	if code != 0 {
		t.Errorf("models refresh (no provider): exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage:") || !strings.Contains(stdout, "aqp") {
		t.Errorf("models refresh (no provider) missing usage/providers:\n%s", stdout)
	}
}

// --- cmdModels refresh <unknown> → non-zero ---

func TestCLI_ModelsRefreshUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "models", cfg, "refresh", "nope")
	if code == 0 {
		t.Error("models refresh nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models refresh nope stderr missing 'unknown provider':\n%s", stderr)
	}
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

// --- cmdTakeover opencode: rewrites opencode config ---

func TestCLI_TakeoverOpencode(t *testing.T) {
	dir := t.TempDir()
	opencodeFile := filepath.Join(dir, "opencode.json")
	os.WriteFile(opencodeFile, []byte(`{}`), 0o644)
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  opencode: %s\n  provider_id: model-proxy\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", opencodeFile)
	cfgPath := writeTempConfig(t, cfgBody)
	_, _, code := runCLI(t, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover opencode: exit=%d want 0", code)
	}
	data, _ := os.ReadFile(opencodeFile)
	if !strings.Contains(string(data), "model-proxy") || !strings.Contains(string(data), "/v1") {
		t.Errorf("opencode not rewritten:\n%s", data)
	}
}

// --- cmdRestore claude: restores from backup (full takeover→restore cycle) ---

func TestCLI_RestoreClaudeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	os.WriteFile(claudeFile, []byte(`{"env":{"ORIGINAL":"1"}}`), 0o644)
	// backupDir = <configDir>/.model-proxy — config lives in dir, so backup in dir/.model-proxy.
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  claude: %s\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", claudeFile)
	cfgPath := writeTempConfig(t, cfgBody)
	// First takeover (creates backup + rewrites), then restore.
	if _, _, code := runCLI(t, "takeover", cfgPath, "claude"); code != 0 {
		t.Fatalf("takeover claude: exit=%d", code)
	}
	if _, _, code := runCLI(t, "restore", cfgPath, "claude"); code != 0 {
		t.Fatalf("restore claude: exit=%d", code)
	}
	data, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(data), "ORIGINAL") {
		t.Errorf("restore did not bring back original:\n%s", data)
	}
}

// --- cmdModels refresh <zhipu> happy path: mock /models + cred file ---

func TestCLI_ModelsRefreshZhipuMock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("models refresh auth=%q want Bearer test-key", r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"},{"id":"glm-4.5","object":"model"}]}`))
	}))
	defer srv.Close()

	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    openai_base_url: " + srv.URL + "\n    provider_id: zhipu\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: zhipu, model: glm-5.2}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"test-key"}`), 0o600)

	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath, "refresh", "zhipu")
	if code != 0 {
		t.Fatalf("models refresh zhipu: exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") || !strings.Contains(stdout, "glm-4.5") {
		t.Errorf("models refresh zhipu missing models:\n%s", stdout)
	}
}

// --- models pull: force-refresh from a mocked models.dev endpoint ---

func TestCLI_ModelsPull_MockedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag := r.Header.Get("If-None-Match"); etag != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(`{"zhipuai":{"api":"https://open.bigmodel.cn/api/paas/v4","models":{"glm-4.6":{"limit":{"context":204800,"output":131072},"modalities":{"input":["text"],"output":["text"]}}}}}`))
	}))
	defer srv.Close()

	cfgPath := writeTempConfig(t, minimalConfig)
	t.Setenv("MP_MODELSDEV_URL", srv.URL)
	home := t.TempDir()
	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath, "pull")
	if code != 0 {
		t.Fatalf("models pull exit=%d", code)
	}
	if !strings.Contains(stdout, "models.dev catalog refreshed") || !strings.Contains(stdout, "1 unique models") {
		t.Errorf("models pull output unexpected:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, ".model-proxy", "models_cache.json")); err != nil {
		t.Errorf("cache file not created: %v", err)
	}
}

// --- models display: hydrates from a fresh pre-seeded cache (no network) ---

func TestCLI_ModelsDisplay_HydratesFromCache(t *testing.T) {
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  zhipu:\n    provider_id: zhipu\n    openai_base_url: https://open.bigmodel.cn/api/paas/v4\nroutes:\n  glm-4.6:\n    - {provider: zhipu, model: glm-4.6}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// pre-seed a FRESH cache (within TTL) so no fetch happens
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"\"v1\"","by_name":{"glm-4.6":{"ctx":204800,"out":131072,"in":["text"],"out_mod":["text"]}},"by_endpoint":{"https://open.bigmodel.cn/api/paas/v4":["glm-4.6"]}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	// endpoint that FAILS if contacted (proves the fresh cache was used instead)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(500)
	}))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	stdout, _, code := runCLIWithHome(t, home, "models", cfgPath)
	if code != 0 {
		t.Fatalf("models display exit=%d", code)
	}
	if called {
		t.Error("fresh cache should NOT have fetched from endpoint")
	}
	if !strings.Contains(stdout, "glm-4.6") || !strings.Contains(stdout, "models.dev") {
		t.Errorf("display should show glm-4.6 with models.dev source:\n%s", stdout)
	}
	// ctx 204800 came from the cache, not config (config had no models:)
	if !strings.Contains(stdout, "204800") {
		t.Errorf("display should show cached context 204800:\n%s", stdout)
	}
}

// --- takeover opencode: warns on default-sourced models ---

func TestCLI_TakeoverOpencode_WarnsDefault(t *testing.T) {
	ocPath := filepath.Join(t.TempDir(), "oc.json")
	os.WriteFile(ocPath, []byte(`{}`), 0o644) // takeover backs up the target first; it must exist
	cfgBody := "listen: 127.0.0.1:15721\ntakeover:\n  provider_id: model-proxy\n  opencode: " + ocPath + "\nproviders:\n  codex:\n    provider_id: codex\n    openai_base_url: https://chatgpt.com/backend-api/codex\nroutes:\n  gpt-5.5:\n    - {provider: codex, model: gpt-5.5}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// fresh EMPTY cache (no models) → gpt-5.5 unmatched → default
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"","by_name":{},"by_endpoint":{}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	_, stderr, code := runCLIWithHome(t, home, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover exit=%d", code)
	}
	if !strings.Contains(stderr, "gpt-5.5") || !strings.Contains(stderr, "default") {
		t.Errorf("takeover should warn about gpt-5.5 default:\n%s", stderr)
	}
	// opencode config written with default ctx 200000 (proves defaults were written, not omitted)
	b, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("opencode config not written: %v", err)
	}
	oc := string(b)
	if !strings.Contains(oc, "200000") {
		t.Errorf("opencode config should contain default ctx 200000:\n%s", oc)
	}
}
