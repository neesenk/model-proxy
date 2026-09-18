package mcp

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestParseFrame_Request(t *testing.T) {
	f := ParseFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`))
	if f.Method != "initialize" || !f.HasID || f.IsResponse || f.IsBatch {
		t.Fatalf("request frame = %+v", f)
	}
}

func TestParseFrame_Notification(t *testing.T) {
	f := ParseFrame([]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	if f.Method != "notifications/initialized" || f.HasID || f.IsResponse {
		t.Fatalf("notification frame = %+v", f)
	}
}

func TestParseFrame_Response(t *testing.T) {
	f := ParseFrame([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[]}}`))
	if !f.IsResponse || f.Method != "" || !f.HasID {
		t.Fatalf("response frame = %+v", f)
	}
	f = ParseFrame([]byte(`{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"x"}}`))
	if !f.IsResponse {
		t.Fatalf("error response frame = %+v", f)
	}
}

func TestParseFrame_BatchAndOpaque(t *testing.T) {
	if f := ParseFrame([]byte(`[{"jsonrpc":"2.0","id":1,"method":"a"}]`)); !f.IsBatch {
		t.Fatalf("batch frame = %+v", f)
	}
	for _, body := range [][]byte{nil, {}, []byte("not json"), []byte(`"str"`)} {
		if f := ParseFrame(body); f.Method != "" || f.IsBatch || f.IsResponse {
			t.Fatalf("opaque %q = %+v", body, f)
		}
	}
}

func TestExtractSSEData(t *testing.T) {
	body := []byte("event: message\nid:1\ndata: {\"a\":1}\n\ndata:{\"b\":2}\n\n: comment\n")
	got := ExtractSSEData(body)
	if len(got) != 2 || string(got[0]) != `{"a":1}` || string(got[1]) != `{"b":2}` {
		t.Fatalf("ExtractSSEData = %q", got)
	}
	if got := ExtractSSEData([]byte("no sse here")); len(got) != 0 {
		t.Fatalf("plain body = %q", got)
	}
}

func TestSessionTable_GetPutDelete(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	if _, ok := tbl.Get("nope"); ok {
		t.Fatal("unknown id hit")
	}
	id := tbl.Put("srv", "acct#1", "up-1")
	s, ok := tbl.Get(id)
	if !ok || s.Server != "srv" || s.Account != "acct#1" || s.UpstreamID != "up-1" {
		t.Fatalf("Get = %+v ok=%v", s, ok)
	}
	tbl.Delete(id)
	if _, ok := tbl.Get(id); ok {
		t.Fatal("deleted id still present")
	}
	if tbl.Len() != 0 {
		t.Fatalf("Len = %d", tbl.Len())
	}
}

func TestSessionTable_LRUEviction(t *testing.T) {
	tbl := NewSessionTable(2, time.Hour)
	a := tbl.Put("s", "a", "u1")
	b := tbl.Put("s", "b", "u2")
	// Touch a so b becomes the LRU victim.
	if _, ok := tbl.Get(a); !ok {
		t.Fatal("a missing")
	}
	tbl.Put("s", "c", "u3")
	if _, ok := tbl.Get(b); ok {
		t.Fatal("LRU victim b survived eviction")
	}
	if tbl.Len() != 2 {
		t.Fatalf("Len = %d, want 2", tbl.Len())
	}
}

func TestSessionTable_TTLExpiry(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	now := time.Now()
	tbl.now = func() time.Time { return now }
	id := tbl.Put("s", "a", "u1")
	now = now.Add(2 * time.Minute)
	if _, ok := tbl.Get(id); ok {
		t.Fatal("expired session still served")
	}
	if tbl.Len() != 0 {
		t.Fatalf("expired entry not dropped, Len = %d", tbl.Len())
	}
}

func TestSessionTable_LastSeenRefresh(t *testing.T) {
	tbl := NewSessionTable(4, time.Minute)
	now := time.Now()
	tbl.now = func() time.Time { return now }
	id := tbl.Put("s", "a", "u1")
	now = now.Add(50 * time.Second)
	if _, ok := tbl.Get(id); !ok {
		t.Fatal("live session dropped")
	}
	now = now.Add(50 * time.Second) // 100s total, but LastSeen refreshed at 50s
	if _, ok := tbl.Get(id); !ok {
		t.Fatal("session expired despite refresh")
	}
}

func TestHeaderPolicy(t *testing.T) {
	src := makeHeader(map[string]string{
		"Accept":               "application/json, text/event-stream",
		"Content-Type":         "application/json",
		"MCP-Protocol-Version": "2025-03-26",
		"Authorization":        "Bearer client-secret",
		"Cookie":               "sid=1",
		"Mcp-Session-Id":       "abc",
	})
	dst := makeHeader(nil)
	CopyClientHeaders(dst, src)
	for _, h := range []string{"Accept", "Content-Type", "MCP-Protocol-Version"} {
		if dst.Get(h) == "" {
			t.Errorf("client header %s not forwarded", h)
		}
	}
	for _, h := range []string{"Authorization", "Cookie", "Mcp-Session-Id"} {
		if dst.Get(h) != "" {
			t.Errorf("client header %s leaked upstream", h)
		}
	}

	usrc := makeHeader(map[string]string{
		"Content-Type":     "text/event-stream",
		"Cache-Control":    "no-cache",
		"Mcp-Session-Id":   "up-1",
		"Www-Authenticate": "Bearer realm=x",
	})
	udst := makeHeader(nil)
	CopyUpstreamHeaders(udst, usrc)
	if udst.Get("Content-Type") != "text/event-stream" || udst.Get("Cache-Control") != "no-cache" {
		t.Errorf("upstream headers not forwarded: %v", udst)
	}
	if udst.Get("Mcp-Session-Id") != "" || udst.Get("Www-Authenticate") != "" {
		t.Errorf("upstream sensitive headers leaked: %v", udst)
	}
}

func TestSessionTable_OnEvict(t *testing.T) {
	tbl := NewSessionTable(2, time.Minute)
	var evicted []string
	tbl.SetOnEvict(func(s Session) { evicted = append(evicted, s.ID+":"+s.Server) })
	a := tbl.Put("srvA", "acct", "up")
	b := tbl.PutRoute("routeB")
	if a == "" || b == "" {
		t.Fatal("put failed")
	}
	// Explicit delete fires with the session's id and server.
	tbl.Delete(a)
	if len(evicted) != 1 || evicted[0] != a+":srvA" {
		t.Fatalf("delete evict = %v", evicted)
	}
	// LRU eviction (cap 2: c, d evict b then c).
	tbl.Put("srvC", "", "")
	tbl.Put("srvD", "", "")
	if len(evicted) != 2 || evicted[1] != b+":routeB" {
		t.Fatalf("lru evict = %v", evicted)
	}
	// Expiry on access fires too (Put itself evicted srvC to stay within cap).
	e := tbl.Put("srvE", "", "")
	if len(evicted) != 3 || evicted[2] == "" {
		t.Fatalf("put evict = %v", evicted)
	}
	tbl.now = func() time.Time { return time.Now().Add(2 * time.Minute) }
	if _, ok := tbl.Get(e); ok {
		t.Fatal("expired session served")
	}
	if len(evicted) != 4 || evicted[3] != e+":srvE" {
		t.Fatalf("expiry evict = %v", evicted)
	}
}

// TestSessionTable_RouteOpsConcurrent hammers one route session's RouteXxx
// methods from many goroutines. Meaningful under -race: the Subs/Sticky maps
// are shared state and must be serialized under the table lock (regression
// for map access after Unlock, which crashed as concurrent map writes).
func TestSessionTable_RouteOpsConcurrent(t *testing.T) {
	tbl := NewSessionTable(64, time.Minute)
	sid := tbl.PutRoute("r")
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			server := fmt.Sprintf("srv-%d", g%3)
			tool := fmt.Sprintf("tool-%d", g%4)
			for i := 0; i < 200; i++ {
				select {
				case <-stop:
					return
				default:
				}
				tbl.RouteSubPut(sid, SubSession{Server: server, Account: "a", UpstreamID: "u", Initialized: true})
				tbl.RouteSubGet(sid, server)
				tbl.RouteStickyPut(sid, tool, server)
				tbl.RouteStickyGet(sid, tool)
				tbl.RouteToolsPut(sid, []ToolSpec{{Name: tool}})
				tbl.RouteToolsGet(sid)
				tbl.RouteSubs(sid)
				if i%17 == 0 {
					tbl.RouteSubDrop(sid, server)
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	// The session survived and its state is coherent.
	if !tbl.RouteSessionValid(sid) {
		t.Fatal("route session lost")
	}
	if tools, ok := tbl.RouteToolsGet(sid); !ok || len(tools) != 1 {
		t.Fatalf("tools cache = %v %v", tools, ok)
	}
}
