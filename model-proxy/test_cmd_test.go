package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// test_cmd_test.go covers `model-proxy test <model>` (test_cmd.go). cmdTest
// os.Exits, so the end-to-end cases run through the TestHelperProcess
// subprocess harness (cli_test.go) against an httptest fake upstream; the
// route-resolution helper testTargetsFor is covered in-process at the bottom.

// testCLIConfig renders a one-provider config pointing at the fake upstream.
func testCLIConfig(upstream string) string {
	return `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: ` + upstream + `, models: [glm-5.2]}
routes:
  glm-test:
    - {provider: zhipu, model: glm-5.2, priority: 1}
`
}

// TestCLI_TestOK: a 200 upstream makes the single target callable → ✓ line, exit 0.
func TestCLI_TestOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	cfgPath := writeTempConfig(t, testCLIConfig(srv.URL))

	stdout, stderr, code := runCLIWithHome(t, home, "test", cfgPath, "glm-test")
	if code != 0 {
		t.Fatalf("test exit=%d want 0; stderr=%s", code, stderr)
	}
	if !strings.Contains(stdout, "✓ glm-test → zhipu (glm-5.2) — HTTP 200") {
		t.Errorf("stdout missing ✓ probe line:\n%s", stdout)
	}
}

// TestCLI_TestAllFail: a 401 upstream fails every target → ✗ line with the
// upstream error code+message, exit 1.
func TestCLI_TestAllFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":{"code":"InvalidKey","message":"bad key"}}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	cfgPath := writeTempConfig(t, testCLIConfig(srv.URL))

	stdout, _, code := runCLIWithHome(t, home, "test", cfgPath, "glm-test")
	if code != 1 {
		t.Fatalf("test exit=%d want 1", code)
	}
	if !strings.Contains(stdout, "✗ glm-test → zhipu (glm-5.2) — HTTP 401") {
		t.Errorf("stdout missing ✗ probe line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "InvalidKey: bad key") {
		t.Errorf("stdout missing upstream reason:\n%s", stdout)
	}
}

// TestCLI_TestNoRoute: an unrouted model exits 1 and lists the available routes.
func TestCLI_TestNoRoute(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	cfgPath := writeTempConfig(t, testCLIConfig("https://example.invalid"))

	_, stderr, code := runCLIWithHome(t, home, "test", cfgPath, "ghost-model")
	if code != 1 {
		t.Fatalf("test exit=%d want 1", code)
	}
	if !strings.Contains(stderr, `no route for model "ghost-model"`) {
		t.Errorf("stderr missing no-route error:\n%s", stderr)
	}
	if !strings.Contains(stderr, "glm-test") {
		t.Errorf("stderr should list available routes:\n%s", stderr)
	}
}

// TestCLI_TestClaudeMapping: a claude alias is translated through
// claude_mapping before route resolution; the translation is announced and the
// mapped route is probed.
func TestCLI_TestClaudeMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	cfgPath := writeTempConfig(t, testCLIConfig(srv.URL)+`
claude_mapping:
  claude-sonnet-x: glm-test
`)

	stdout, _, code := runCLIWithHome(t, home, "test", cfgPath, "claude-sonnet-x")
	if code != 0 {
		t.Fatalf("test exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "claude_mapping: claude-sonnet-x → glm-test") {
		t.Errorf("stdout missing mapping announcement:\n%s", stdout)
	}
	if !strings.Contains(stdout, "✓ glm-test → zhipu (glm-5.2)") {
		t.Errorf("stdout missing ✓ probe line for mapped route:\n%s", stdout)
	}
}

// TestCLI_TestPriorityOrder: targets are probed in priority order (lower
// first), not config order; one callable target is enough for exit 0.
func TestCLI_TestPriorityOrder(t *testing.T) {
	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte(`{}`))
	}))
	defer okSrv.Close()
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer badSrv.Close()

	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A")
	writePoolFile(t, "deepseek", "deepseek", "KEY-B")
	cfgPath := writeTempConfig(t, `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: `+okSrv.URL+`, models: [glm-5.2]}
  deepseek: {provider_id: deepseek, openai_base_url: `+badSrv.URL+`, models: [deepseek-v4-pro]}
routes:
  glm-test:
    - {provider: zhipu, model: glm-5.2, priority: 2}
    - {provider: deepseek, model: deepseek-v4-pro, priority: 1}
`)

	stdout, _, code := runCLIWithHome(t, home, "test", cfgPath, "glm-test")
	if code != 0 {
		t.Fatalf("test exit=%d want 0 (zhipu callable)", code)
	}
	// deepseek (priority 1) probed before zhipu (priority 2) despite config order.
	di := strings.Index(stdout, "→ deepseek")
	zi := strings.Index(stdout, "→ zhipu")
	if di < 0 || zi < 0 || di > zi {
		t.Errorf("probe order wrong (deepseek p1 must precede zhipu p2):\n%s", stdout)
	}
	if !strings.Contains(stdout, "✗ glm-test → deepseek") || !strings.Contains(stdout, "✓ glm-test → zhipu") {
		t.Errorf("expected ✗ deepseek + ✓ zhipu lines:\n%s", stdout)
	}
}
