package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// makeHeader builds an http.Header from a map (test readability helper).
func makeHeader(kv map[string]string) http.Header {
	h := http.Header{}
	for k, v := range kv {
		h.Set(k, v)
	}
	return h
}

// fakeMCPServer serves the MCP handshake: initialize returns an SSE-framed
// result with a session id; tools/list returns a JSON result. It records the
// auth and session headers it saw.
type fakeMCPServer struct {
	sawAuth     []string
	sawSession  []string
	sawNotif    bool
	sessionID   string
	sseResponse bool
}

func (f *fakeMCPServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.sawAuth = append(f.sawAuth, r.Header.Get("Authorization"))
		f.sawSession = append(f.sawSession, r.Header.Get("Mcp-Session-Id"))
		frame := ParseFrame(readBody(r))
		switch frame.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", f.sessionID)
			payload := `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake-mcp","version":"0.1"}}}`
			if f.sseResponse {
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			} else {
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(payload))
			}
		case "notifications/initialized":
			f.sawNotif = true
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"search"},{"name":"read"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func readBody(r *http.Request) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

func TestProbe_JSONAndSSE(t *testing.T) {
	for _, sse := range []bool{false, true} {
		fk := &fakeMCPServer{sessionID: "up-session-1", sseResponse: sse}
		up := httptest.NewServer(fk.handler())
		defer up.Close()
		res, err := Probe(context.Background(), up.Client(), up.URL, func(r *http.Request) error {
			r.Header.Set("Authorization", "Bearer k")
			return nil
		})
		if err != nil {
			t.Fatalf("sse=%v: %v", sse, err)
		}
		if res.ServerName != "fake-mcp" || res.ServerVersion != "0.1" || res.Protocol != "2025-03-26" {
			t.Errorf("sse=%v: serverInfo = %+v", sse, res)
		}
		if !res.Sessionful {
			t.Errorf("sse=%v: session not detected", sse)
		}
		if len(res.Tools) != 2 || res.Tools[0] != "search" || res.Tools[1] != "read" {
			t.Errorf("sse=%v: tools = %v", sse, res.Tools)
		}
		if !fk.sawNotif {
			t.Errorf("sse=%v: initialized notification not sent", sse)
		}
		// tools/list must carry the upstream session id captured at initialize.
		last := fk.sawSession[len(fk.sawSession)-1]
		if last != "up-session-1" {
			t.Errorf("sse=%v: tools/list session = %q", sse, last)
		}
		for i, a := range fk.sawAuth {
			if a != "Bearer k" {
				t.Errorf("sse=%v: request %d auth = %q", sse, i, a)
			}
		}
	}
}

func TestProbe_HTTPError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer up.Close()
	_, err := Probe(context.Background(), up.Client(), up.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestProbe_RPCError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"quota exhausted"}}`))
	}))
	defer up.Close()
	_, err := Probe(context.Background(), up.Client(), up.URL, nil)
	if err == nil || !strings.Contains(err.Error(), "quota exhausted") {
		t.Fatalf("err = %v", err)
	}
}
