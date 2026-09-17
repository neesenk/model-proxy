package mcp

import (
	"bufio"
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
	initResp, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(initResp), `"fake-stdio"`) {
		t.Fatalf("initialize = %s", initResp)
	}
	// Notifications: write-only, immediate nil.
	if _, err := conn.Call([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)); err != nil {
		t.Fatal(err)
	}
	callResp, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search","arguments":{"q":"x"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(callResp), "called:search") || !strings.Contains(string(callResp), "key:env-value-1") {
		t.Fatalf("call = %s (env not injected?)", callResp)
	}
	// String ids correlate too.
	resp, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":"abc","method":"tools/list"}`))
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
	if _, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err == nil {
		t.Fatal("call on dead child succeeded")
	}
}

func TestStdioConn_CloseIdempotent(t *testing.T) {
	conn := startHelperConn(t)
	conn.Close()
	conn.Close() // must not panic or double-wait
	if _, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)); err == nil {
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
