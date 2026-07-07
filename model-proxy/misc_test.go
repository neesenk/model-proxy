package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// misc_test.go covers the last small testable functions: LoopbackServer.Port,
// openLogFile, pidFilePath, writePidFile, printConfigProviders, PollSession
// (via pollAt with mock).

// --- LoopbackServer.Port ---

func TestLoopbackServer_Port(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	if ls.Port() == 0 {
		t.Error("Port()=0 want non-zero after Start")
	}
	if ls.CallbackURL() == "" {
		t.Error("CallbackURL() empty after Start")
	}
}

// --- openLogFile ---

func TestOpenLogFile_CreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "model-proxy.log")
	f, err := openLogFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("log file not created: %v", err)
	}
}

func TestOpenLogFile_Appends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	f1, _ := openLogFile(p)
	f1.Write([]byte("first\n"))
	f1.Close()
	f2, _ := openLogFile(p)
	f2.Write([]byte("second\n"))
	f2.Close()
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("append lost content: %s", data)
	}
}

// --- pidFilePath (daemon_test.go already covers it; skipped here) ---

// --- writePidFile ---

func TestWritePidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pid")
	if err := writePidFile(p, 12345); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "12345") {
		t.Errorf("pid file content=%q want 12345", data)
	}
}

// --- printConfigProviders: reads config, lists providers ---

func TestPrintConfigProviders(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	os.WriteFile(cfgPath, []byte(`listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://open.bigmodel.cn/api/paas/v4
    provider_id: zhipu
    models:
      glm-5.2: {context: 1000, output: 1000, modalities: {input: [text], output: [text]}}
routes:
  glm-5.2:
    - {provider: zhipu, model: glm-5.2}
`), 0o644)

	out := grabStdout(t, func() { printConfigProviders([]string{"--config", cfgPath}) })
	if !strings.Contains(out, "zhipu") {
		t.Errorf("printConfigProviders missing zhipu:\n%s", out)
	}
	if !strings.Contains(out, "provider=zhipu") {
		t.Errorf("printConfigProviders missing provider_id:\n%s", out)
	}
}

func TestPrintConfigProviders_NoConfig(t *testing.T) {
	// Missing config → LoadConfig errors → early return (no output, no panic).
	out := grabStdout(t, func() {
		printConfigProviders([]string{"--config", filepath.Join(t.TempDir(), "nope.yaml")})
	})
	if strings.TrimSpace(out) != "" {
		t.Errorf("printConfigProviders(missing config) should print nothing: %q", out)
	}
}

// --- PollSession via pollAt with a mock (covers PollSession's 1-line delegate) ---
// PollSession() calls pollAt(compassAuthInfo, ...) — the real URL. We can't
// redirect it (no URL-param variant on PollSession). Instead cover pollAt +
// checkSessionAt directly (already 72.7%/83.3%), and exercise the timeout
// path of pollAt with a mock that always fails.

func TestPollAt_TimesOut(t *testing.T) {
	// A mock that returns 401 every time → pollAt loops until the deadline,
	// then returns the last error.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"retcode":1,"message":"pending"}`))
	}))
	defer srv.Close()
	c := newCompassClient(filepath.Join(t.TempDir(), "store.json"))
	_, err := c.pollAt(srv.URL, 1*time.Millisecond)
	if err == nil {
		t.Error("pollAt always-401: want error, got nil")
	}
}

// keep imports referenced.
var _ = strings.Contains
