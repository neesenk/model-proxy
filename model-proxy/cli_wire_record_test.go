package main

// wire_record_test.go — `wire record` against a mock upstream: byte-faithful
// .sse recording, .err on non-2xx (never overwriting a good .sse), full
// runWireRecord path (config → providerImplFor → endpoints), arg splitting.

import (
	"io"
	"model-proxy/internal/cli"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireRecord_Run: responses/chat endpoints record byte-identical .sse;
// the anthropic 404s land in .err without touching a pre-existing good .sse;
// runWireRecord reports the failures.
func TestWireRecord_Run(t *testing.T) {
	bodies := map[string]string{
		"/responses":        "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n",
		"/chat/completions": "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if body, ok := bodies[r.URL.Path]; ok {
			if r.Header.Get("Authorization") == "" {
				t.Errorf("record request missing auth header")
			}
			w.Header().Set("content-type", "text/event-stream")
			io.WriteString(w, body)
			return
		}
		w.WriteHeader(http.StatusNotFound) // /v1/messages does not exist
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
	}
	outDir := t.TempDir()
	// Pre-existing good file for one endpoint — the 404 must not clobber it.
	goodFile := filepath.Join(outDir, "anthropic_p.sse")
	if err := os.WriteFile(goodFile, []byte("GOOD-KEEP"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := cli.RunWireRecord("p", "", "hi", outDir, cfg)
	if err == nil {
		t.Fatal("expected error (anthropic endpoints 404)")
	}
	if !strings.Contains(err.Error(), "endpoint(s) failed") {
		t.Errorf("error = %v", err)
	}

	// 2xx endpoints: 3 scenarios each, byte-identical content.
	expected := map[string]string{"responses": bodies["/responses"], "chat": bodies["/chat/completions"]}
	for _, stem := range []string{"responses_p", "responses_p_tool", "responses_p_thinking", "chat_p", "chat_p_tool", "chat_p_thinking"} {
		proto := strings.SplitN(stem, "_", 2)[0]
		raw, rerr := os.ReadFile(filepath.Join(outDir, stem+".sse"))
		if rerr != nil {
			t.Errorf("%s.sse missing: %v", stem, rerr)
			continue
		}
		if string(raw) != expected[proto] {
			t.Errorf("%s.sse content mismatch:\n got: %s\nwant: %s", stem, raw, expected[proto])
		}
	}
	// anthropic 404s: .err written (status recorded), good .sse untouched.
	for _, stem := range []string{"anthropic_p", "anthropic_p_tool", "anthropic_p_thinking"} {
		raw, rerr := os.ReadFile(filepath.Join(outDir, stem+".err"))
		if rerr != nil {
			t.Errorf("%s.err missing: %v", stem, rerr)
			continue
		}
		if !strings.Contains(string(raw), "status: 404") {
			t.Errorf("%s.err = %q", stem, raw)
		}
	}
	kept, _ := os.ReadFile(goodFile)
	if string(kept) != "GOOD-KEEP" {
		t.Errorf("pre-existing .sse overwritten: %q", kept)
	}

	// Unknown provider → error (no exit).
	if err := cli.RunWireRecord("nope", "", "hi", t.TempDir(), cfg); err == nil {
		t.Error("unknown provider must error")
	}
}

// TestWireRecord_SplitArgs: interspersed flags and positionals split
// correctly (`wire record <prov> --out DIR` and `--out DIR <prov>` both work).
func TestWireRecord_SplitArgs(t *testing.T) {
	flagArgs, pos := cli.SplitWireRecordArgs([]string{"p1", "--out", "/tmp/x", "--model=m2"})
	if len(pos) != 1 || pos[0] != "p1" {
		t.Errorf("pos = %v", pos)
	}
	if len(flagArgs) != 3 || flagArgs[0] != "--out" || flagArgs[1] != "/tmp/x" || flagArgs[2] != "--model=m2" {
		t.Errorf("flagArgs = %v", flagArgs)
	}
	flagArgs2, pos2 := cli.SplitWireRecordArgs([]string{"--model", "m2", "p1"})
	if len(pos2) != 1 || pos2[0] != "p1" || len(flagArgs2) != 2 || flagArgs2[1] != "m2" {
		t.Errorf("flagArgs2=%v pos2=%v", flagArgs2, pos2)
	}
}

func TestWireRecord_CmdHappyPath(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "data: {\"ok\":true}\n\n")
	}))
	defer up.Close()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	writePoolFile(t, "p", "static", "STATIC-TEST-KEY")
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
