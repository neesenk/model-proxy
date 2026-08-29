package login

import (
	"errors"
	"testing"
	"time"
)

// loopback_test.go covers LoopbackServer.WaitForCookie's three branches
// (cookie received, error received, timeout).

func TestWaitForCookie_Timeout(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if _, err := ls.WaitForCookie(20 * time.Millisecond); err == nil {
		t.Error("WaitForCookie with no signal: want timeout error, got nil")
	}
}

func TestWaitForCookie_CookieReceived(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	// Send a cookie on the channel before waiting.
	go func() {
		ls.CookieCh <- "SSO_C=got-it"
	}()
	got, err := ls.WaitForCookie(500 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if got != "SSO_C=got-it" {
		t.Errorf("WaitForCookie=%q want SSO_C=got-it", got)
	}
}

func TestWaitForCookie_ErrorReceived(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	go func() {
		ls.ErrCh <- errBoom
	}()
	// Assert the sentinel itself: err != nil alone would also pass via the
	// timeout branch (deleting the ErrCh case entirely would stay green).
	if _, err := ls.WaitForCookie(500 * time.Millisecond); !errors.Is(err, errBoom) {
		t.Errorf("WaitForCookie with errCh signal: got %v, want the errBoom sentinel", err)
	}
}

var errBoom = echoErr("boom")

type echoErr string

func (e echoErr) Error() string { return string(e) }
