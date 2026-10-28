package mcpserver

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestRunServerDispatch covers the process entry: argument validation and
// the clean EOF exit for the only bundled server.
func TestRunServerDispatch(t *testing.T) {
	var out, errOut strings.Builder
	if code := RunServer(nil, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "usage: mcp-server") {
		t.Errorf("no args: code=%d stderr=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := RunServer([]string{"nope"}, strings.NewReader(""), &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "unknown bundled server") {
		t.Errorf("unknown server: code=%d stderr=%q", code, errOut.String())
	}
	errOut.Reset()
	if code := RunServer([]string{"deepseek-search"}, strings.NewReader(""), &out, &errOut); code != 0 || out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("EOF exit: code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}
}

// TestDeepSeekChildProc is the subprocess entry for the end-to-end test
// below: MP_DEEPSEEK_CHILD=1 turns this test into the bundled server itself,
// exactly as the gateway's stdio transport would spawn the deployed binary
// (minimal env + the configured env entries).
func TestDeepSeekChildProc(t *testing.T) {
	if os.Getenv("MP_DEEPSEEK_CHILD") != "1" {
		return
	}
	if err := ServeDeepSeek(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "child server: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// TestDeepSeekChildEndToEnd spawns this test binary as the deepseek-search
// stdio child (like `mcp test` / the gateway would), drives the full
// initialize → notification → tools/call exchange over real pipes, and
// verifies the credential env contract end to end: the key the gateway
// injects via ${account.api_key} is the one that reaches the API.
func TestDeepSeekChildEndToEnd(t *testing.T) {
	var sawAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"results":[{"rerank_score":2.0,"title":"Child hit","url":"https://example.com/c","summary":"via spawned child","site_name":"Ex","date":"2026-01-02T00:00:00+00:00"}]}`)
	}))
	defer ts.Close()

	cmd := exec.Command(os.Args[0], "-test.run=TestDeepSeekChildProc", "--")
	cmd.Env = []string{
		"MP_DEEPSEEK_CHILD=1",
		"DEEPSEEK_API_KEY=pool-injected-key",
		"DEEPSEEK_BASE_URL=" + ts.URL,
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var childErr strings.Builder
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(stdout)

	send := func(line string) {
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatalf("write %s: %v", line, err)
		}
	}
	read := func() map[string]any {
		raw, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read response line: %v (child stderr: %s)", err, childErr.String())
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(raw), &v); err != nil {
			t.Fatalf("parse response %q: %v", raw, err)
		}
		return v
	}

	send(initializeLine("1", "2025-03-26"))
	init := read()
	if init["error"] != nil {
		t.Fatalf("initialize failed: %v", init)
	}

	// The notification must stay unanswered: the next line on stdout has to
	// be the ping response, proving no stray frame interleaved.
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	ping := read()
	if fmt.Sprint(ping["id"]) != "2" {
		t.Fatalf("expected ping response id 2 first, got %v", ping)
	}

	send(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"child e2e"}}}`)
	call := read()
	if call["error"] != nil {
		t.Fatalf("tools/call failed: %v", call)
	}
	if sawAuth != "Bearer pool-injected-key" {
		t.Errorf("Authorization seen by API = %q, want the env-injected pool key", sawAuth)
	}
	result, _ := json.Marshal(call["result"])
	if !strings.Contains(string(result), "Child hit") || !strings.Contains(string(result), "via spawned child") {
		t.Errorf("call result missing upstream content: %s", result)
	}

	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("child exit: %v (stderr: %s)", err, childErr.String())
	}
}
