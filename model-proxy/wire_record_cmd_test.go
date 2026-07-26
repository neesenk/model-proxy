package main

// cmdWire/cmdWireRecord happy path: every endpoint 2xx → full run with no
// failure exit (covers the thin CLI wrappers; failure exits are os.Exit by
// design and tested at the runWireRecord level).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestWireRecord_CmdHappyPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer up.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgYAML := "providers:\n  p: {provider_id: static, openai_base_url: " + up.URL + ", models: [m1]}\n"
	cfgPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	// cmdWire → cmdWireRecord → runWireRecord, all endpoints 2xx → returns
	// without os.Exit. Any exit would kill the test binary (caught as failure).
	cmdWire([]string{"record", "p", "--out", outDir, "--config", cfgPath})
	// Spot-check one recorded file exists.
	if _, err := os.Stat(filepath.Join(outDir, "responses_p.sse")); err != nil {
		t.Errorf("responses_p.sse missing after happy-path run: %v", err)
	}
}
