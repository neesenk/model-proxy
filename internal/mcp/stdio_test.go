package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// TestHelperProcess doubles as the fake MCP stdio child: with
// MCP_STDIO_HELPER=1 it serves newline-delimited JSON-RPC on stdin/stdout.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	os.Exit(runStdioHelper())
}

func runStdioHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		line := scanner.Bytes()
		var v struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params *struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &v); err != nil {
			continue
		}
		respond := func(result string) {
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", v.ID, result)
			writer.Flush()
		}
		switch v.Method {
		case "initialize":
			respond(`{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fake-stdio","version":"1.0"}}`)
		case "notifications/initialized":
			// no response
		case "tools/list":
			respond(`{"tools":[{"name":"search","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			name := ""
			if v.Params != nil {
				name = v.Params.Name
			}
			respond(fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, "called:"+name+" key:"+os.Getenv("TEST_KEY")))
		}
	}
	return 0
}

func startHelperConn(t *testing.T) *StdioConn {
	t.Helper()
	conn, err := StartStdio(
		[]string{os.Args[0], "-test.run=TestHelperProcess"},
		[]string{"MCP_STDIO_HELPER=1", "TEST_KEY=env-value-1", "PATH=" + os.Getenv("PATH")},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

func TestStdioConn_HandshakeAndCall(t *testing.T) {
	conn := startHelperConn(t)
	initResp, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(initResp), `"fake-stdio"`) {
		t.Fatalf("initialize = %s", initResp)
	}
	// Notifications: write-only, immediate nil.
	if _, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	callResp, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"x"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(callResp), "called:search") || !strings.Contains(string(callResp), "key:env-value-1") {
		t.Fatalf("call = %s (env not injected?)", callResp)
	}
	// String ids correlate too.
	resp, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`))
	if err != nil || !strings.Contains(string(resp), `"search"`) {
		t.Fatalf("string id = %s err=%v", resp, err)
	}
}

func TestStdioConn_DeadChild(t *testing.T) {
	conn, err := StartStdio([]string{os.Args[0], "-test.run=TestHelperProcess"}, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The helper skips (env marker missing) and exits → every Call fails.
	if _, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err == nil {
		t.Fatal("call on dead child succeeded")
	}
}

// TestHelperProcessStderr doubles as a fake child that prints a secret token
// to stderr and exits non-zero.
func TestHelperProcessStderr(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") != "stderr" {
		t.Skip("not a helper subprocess")
	}
	fmt.Fprintln(os.Stderr, "secret-stderr-token-42")
	os.Exit(1)
}

// TestStdioConn_CallErrorOmitsStderr: a dead child must not leak raw stderr
// into the error returned to callers — stderr is untrusted and may contain
// credentials that would otherwise surface in HTTP responses. The captured
// stderr remains reachable for redacted debug logging.
func TestStdioConn_CallErrorOmitsStderr(t *testing.T) {
	conn, err := StartStdio(
		[]string{os.Args[0], "-test.run=TestHelperProcessStderr"},
		[]string{"MCP_STDIO_HELPER=stderr", "PATH=" + os.Getenv("PATH")},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil {
		t.Fatal("expected error from dead child")
	}
	if strings.Contains(err.Error(), "secret-stderr-token-42") {
		t.Fatalf("error leaks stderr: %v", err)
	}
	// stderr is still captured for debug logging.
	if !strings.Contains(conn.Stderr(), "secret-stderr-token-42") {
		t.Fatalf("stderr not captured for debug: %q", conn.Stderr())
	}
}

func TestStdioConn_CloseIdempotent(t *testing.T) {
	conn := startHelperConn(t)
	conn.Close()
	conn.Close() // must not panic or double-wait
	if _, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err == nil {
		t.Fatal("call after Close succeeded")
	}
	// The read loop must have finished (no goroutine leak).
	select {
	case <-conn.readDone:
	case <-time.After(5 * time.Second):
		t.Fatal("read loop leaked after Close")
	}
}

func TestStdioConn_EmptyCommand(t *testing.T) {
	if _, err := StartStdio(nil, nil); err == nil {
		t.Fatal("empty command accepted")
	}
}

// TestStdioConn_CallContextCancel: a hung child (never answers) must not park
// the caller — ctx cancel aborts the wait, drops the pending registration,
// and leaves the conn usable. Regression for the ctx-less Call blocking
// forever on a hung subprocess.
func TestStdioConn_CallContextCancel(t *testing.T) {
	conn := startHelperConn(t) // helper answers known methods only; "hang" gets no reply
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := conn.Call(ctx, []byte(`{"jsonrpc":"2.0","id":9,"method":"hang"}`))
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled call returned nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled call never returned (hung child parked the caller)")
	}
	// The pending registration must be gone (a late response would be
	// discarded) and the conn still works.
	conn.mu.Lock()
	pending := len(conn.pending)
	conn.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entry leaked: %d", pending)
	}
	resp, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":10,"method":"tools/list"}`))
	if err != nil || !strings.Contains(string(resp), `"search"`) {
		t.Fatalf("conn unusable after cancel: resp=%s err=%v", resp, err)
	}
}

// TestStdioConn_CallContextTimeout: the timeout path of the same contract.
func TestStdioConn_CallContextTimeout(t *testing.T) {
	conn := startHelperConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := conn.Call(ctx, []byte(`{"jsonrpc":"2.0","id":11,"method":"hang"}`))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("timeout error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed-out call never returned")
	}
}

// TestHelperProcessStderrDiag doubles as a fake backend dying at startup (bad
// key, missing runtime): it echoes the secret it was handed and prints a stack
// trace on its way out, after enough noise to prove the tail stays bounded.
func TestHelperProcessStderrDiag(t *testing.T) {
	if os.Getenv("MCP_STDIO_HELPER") != "diag" {
		t.Skip("not a helper subprocess")
	}
	fmt.Fprintln(os.Stderr, strings.Repeat("debug noise ", 100))
	fmt.Fprintln(os.Stderr, "Error: boot failed")
	fmt.Fprintf(os.Stderr, "config: key=%s home=%s\n", os.Getenv("CHILD_SECRET"), os.Getenv("HOME"))
	fmt.Fprintln(os.Stderr, "    at Object.<anonymous> (server.js:1:1)")
	os.Exit(3)
}

// TestStdioConn_ExitErrorCarriesStatusAndRedactedStderr: a dead child's
// failure must say how it died without echoing what it was handed. The bare
// error — all an HTTP answer may carry — gains the wait status; the
// diagnostics riding on it add a bounded, single-line stderr tail in which
// every credential-bearing environment value is masked (inherited names such
// as PATH and HOME stay readable). Before the fix the error was the bare
// "stdio: child exited" and the captured stderr had no consumer at all, so a
// child dying at startup left no way to tell why.
func TestStdioConn_ExitErrorCarriesStatusAndRedactedStderr(t *testing.T) {
	const secret = "sk-live-key-0123456789abcdef"
	conn, err := StartStdio(
		[]string{os.Args[0], "-test.run=TestHelperProcessStderrDiag"},
		[]string{
			"MCP_STDIO_HELPER=diag",
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + os.Getenv("HOME"),
			"CHILD_SECRET=" + secret,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, err = conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err == nil {
		t.Fatal("expected the dead child to fail the call")
	}
	if got, want := err.Error(), "stdio: child exited (exit status 3)"; got != want {
		t.Errorf("error = %q, want the bare failure plus the wait status %q", got, want)
	}
	if strings.Contains(err.Error(), "boot failed") || strings.Contains(err.Error(), secret) {
		t.Errorf("error carries child stderr: %q", err.Error())
	}
	diag := ExitDiag(err)
	for _, want := range []string{"exit status 3", "Error: boot failed", "at Object.<anonymous> (server.js:1:1)", os.Getenv("HOME"), "<redacted:CHILD_SECRET>"} {
		if !strings.Contains(diag, want) {
			t.Errorf("diag = %q, want it to carry %q", diag, want)
		}
	}
	if strings.Contains(diag, secret) {
		t.Errorf("diag leaks the credential the child was handed: %q", diag)
	}
	if strings.ContainsAny(diag, "\n\r") {
		t.Errorf("diag = %q, want a single line (serve.log is line-oriented)", diag)
	}
	// The tail keeps the end of the stderr and stays bounded.
	if !strings.HasPrefix(diag, "exit status 3") || !strings.Contains(diag, "…") {
		t.Errorf("diag = %q, want the wait status first and a marked truncation", diag)
	}
	if len(diag) > len("exit status 3; stderr: …")+maxStderrTail {
		t.Errorf("diag length = %d, want at most the status plus a %d-byte tail", len(diag), maxStderrTail)
	}
}
