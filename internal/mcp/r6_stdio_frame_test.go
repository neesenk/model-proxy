package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/testsyn"
)

// r6_stdio_frame_test.go pins the stdio frame bound (R6 F9): MaxFrameBytes is
// the contract between the gateway's read loop and every stdio child. A frame
// one byte below it is delivered intact; a frame at it is past what the
// scanner accepts and kills the whole conn — every pending and later call
// fails — which is why children must keep their lines strictly below it.

// TestR6StdioFrameHelper doubles as the frame-size child: with
// MCP_STDIO_R6_FRAME_HELPER=1 it answers `frame` calls with a response line
// of exactly the requested byte length (newline excluded). Other requests are
// held until a frame is under way (so a caller can park one on the conn
// before the frame is even requested) and then answered with an empty result.
func TestR6StdioFrameHelper(t *testing.T) {
	if os.Getenv("MCP_STDIO_R6_FRAME_HELPER") != "1" {
		t.Skip("not a helper subprocess")
	}
	os.Exit(r6FrameHelperMain())
}

func r6FrameHelperMain() int {
	dec := json.NewDecoder(os.Stdin)
	var writeMu sync.Mutex // one whole line per write: lines never interleave
	frameStarted := make(chan struct{})
	var startOnce sync.Once
	for {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Params *struct {
				Name      string `json:"name"`
				Arguments *struct {
					Size int `json:"size"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := dec.Decode(&req); err != nil {
			return 0 // stdin closed: the session is over
		}
		if len(req.ID) == 0 {
			continue // notification: never answered
		}
		go func() {
			line := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":{}}`, req.ID)
			if req.Params != nil && req.Params.Name == "frame" {
				// The stream is held for the whole frame BEFORE the frame is
				// announced: a sibling waiting for the frame to start then
				// cannot slip its line in before (or into) the oversized one.
				writeMu.Lock()
				defer writeMu.Unlock()
				startOnce.Do(func() { close(frameStarted) })
				prefix := `{"jsonrpc":"2.0","id":` + string(req.ID) + `,"result":{"content":[{"type":"text","text":"`
				const suffix = `"}]}}`
				line = prefix + strings.Repeat("x", req.Params.Arguments.Size-len(prefix)-len(suffix)) + suffix
			} else {
				<-frameStarted
				writeMu.Lock()
				defer writeMu.Unlock()
			}
			if _, err := os.Stdout.WriteString(line + "\n"); err != nil {
				os.Exit(1)
			}
		}()
	}
}

func r6FrameConn(t *testing.T) *StdioConn {
	t.Helper()
	// The conn owns a read-loop goroutine (and exec's pipe copiers): they must
	// be gone once Close ran — cleanups fire LIFO, so Close precedes the check.
	baseline := testsyn.Goroutines()
	t.Cleanup(func() { testsyn.LeakCheck(t, baseline, "frame helper conn") })
	conn, err := StartStdio(
		[]string{os.Args[0], "-test.run=^TestR6StdioFrameHelper$"},
		[]string{"MCP_STDIO_R6_FRAME_HELPER=1", "PATH=" + os.Getenv("PATH")},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// r6FrameCall builds a `frame` request whose response line is size bytes.
func r6FrameCall(id, size int) []byte {
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"frame","arguments":{"size":%d}}}`, id, size))
}

// r6AwaitPending blocks until the call with the given id is registered on the
// conn. A Call registers and writes its request inside one conn.mu critical
// section, so once the id is pending its bytes are already in the pipe.
func r6AwaitPending(t *testing.T, conn *StdioConn, id string) {
	t.Helper()
	testsyn.WaitUntil(t, "call "+id+" to be in flight", func() bool {
		conn.mu.Lock()
		defer conn.mu.Unlock()
		_, ok := conn.pending[id]
		return ok
	})
}

// TestR6StdioConn_FrameJustUnderLimitIsDelivered: the longest line the bound
// admits — MaxFrameBytes-1 bytes, the newline taking the last byte of the
// scanner buffer — reaches its caller intact and leaves the conn usable. The
// bound is an exact edge, not a rounding: one more byte is the next test.
func TestR6StdioConn_FrameJustUnderLimitIsDelivered(t *testing.T) {
	conn := r6FrameConn(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const size = MaxFrameBytes - 1
	resp, err := conn.Call(ctx, r6FrameCall(1, size))
	if err != nil {
		t.Fatalf("frame of %d bytes = %v, want delivered", size, err)
	}
	if len(resp) != size {
		t.Fatalf("delivered %d bytes, want the whole %d-byte frame", len(resp), size)
	}
	var rpc struct {
		ID     int `json:"id"`
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(resp, &rpc); err != nil {
		t.Fatalf("delivered frame is not intact JSON: %v", err)
	}
	if rpc.ID != 1 || len(rpc.Result.Content) != 1 || rpc.Result.Content[0].Type != "text" {
		t.Fatalf("delivered frame = id %d, %d content blocks", rpc.ID, len(rpc.Result.Content))
	}
	if text := rpc.Result.Content[0].Text; strings.Trim(text, "x") != "" {
		t.Fatal("delivered frame text is not the padding (frame corrupted)")
	}

	// The read loop survived the largest admitted frame.
	if resp, err := conn.Call(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)); err != nil || !strings.Contains(string(resp), `"id":2`) {
		t.Fatalf("call after the largest frame = %q, %v", resp, err)
	}
}

// TestR6StdioConn_FrameAtLimitKillsConn: a line of MaxFrameBytes bytes does
// not fit (the newline would need a byte beyond the scanner buffer), and the
// failure is not a lost frame — the read loop fails with ErrTooLong and the
// conn dies: a call already pending on the same conn AND the oversized call
// both fail, and nothing works afterwards.
func TestR6StdioConn_FrameAtLimitKillsConn(t *testing.T) {
	conn := r6FrameConn(t)
	siblingErr := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
		siblingErr <- err
	}()
	// The sibling is parked on the conn before the frame even leaves, so its
	// fate is settled by the oversized frame and not by scheduling.
	r6AwaitPending(t, conn, "2")
	frameErr := make(chan error, 1)
	go func() {
		_, err := conn.Call(context.Background(), r6FrameCall(1, MaxFrameBytes))
		frameErr <- err
	}()
	select {
	case <-conn.readDone:
	case <-time.After(30 * time.Second):
		t.Fatal("oversized frame left the read loop running")
	}
	for _, c := range []struct {
		what string
		ch   <-chan error
	}{
		{"oversized frame call", frameErr},
		{"sibling call pending on the same conn", siblingErr},
	} {
		select {
		case err := <-c.ch:
			if err == nil || !strings.Contains(err.Error(), "child exited") {
				t.Fatalf("%s = %v, want the dead-child failure", c.what, err)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s still pending after the conn died", c.what)
		}
	}
	if !errors.Is(conn.readErr, bufio.ErrTooLong) {
		t.Fatalf("read loop error = %v, want bufio.ErrTooLong", conn.readErr)
	}
	if _, err := conn.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)); err == nil {
		t.Fatal("conn still serves calls after an oversized frame")
	}
}
