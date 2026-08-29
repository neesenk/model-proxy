package login

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Regression (TOCTOU): NewLoopbackServer used to bind a port, read the number,
// CLOSE the listener, and Start re-listened the same address — another socket
// could take the port in between and the printed callback URL went dead. The
// construction listener must be kept and served by Start.
func TestLoopbackServer_PortHeldFromConstruction(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()

	// The port must still be OWNED (bound, not serving yet) before Start: if
	// another socket can bind it, the old bind-close-rebind race is back.
	if steal, err := net.Listen("tcp", ls.addr); err == nil {
		steal.Close()
		t.Fatal("port was released between construction and Start")
	}

	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	// And Start serves on the very port the callback URL was derived from.
	resp, err := http.Get(ls.CallbackURL())
	if err != nil {
		t.Fatalf("callback URL unreachable after Start: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("callback status=%d want %d", resp.StatusCode, http.StatusOK)
	}
	select {
	case <-ls.CookieCh:
	case err := <-ls.ErrCh:
		t.Fatalf("self-callback did not complete the login signal: %v", err)
	case <-time.After(time.Second):
		t.Fatal("self-callback did not complete the login signal: timed out")
	}
}

// --- LoopbackServer.CallbackURL ---

func TestLoopbackServer_CallbackURL(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if err := ls.Start(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ls.CallbackURL(), "http://127.0.0.1:") {
		t.Errorf("CallbackURL()=%q want a 127.0.0.1 loopback URL with the bound port", ls.CallbackURL())
	}
	if !strings.HasSuffix(ls.CallbackURL(), LoginCompletePath) {
		t.Errorf("CallbackURL()=%q want suffix %q", ls.CallbackURL(), LoginCompletePath)
	}
}
