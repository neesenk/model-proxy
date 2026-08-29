package login

import (
	"errors"
	"testing"
	"time"
)

// This file covers the LoopbackServer completion channels (CookieCh/ErrCh):
// cookie received, error received, and no-signal stays silent.

func TestLoopbackSignal_NoSignalStaysSilent(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	select {
	case cookie := <-ls.CookieCh:
		t.Errorf("no signal: unexpected cookie %q", cookie)
	case err := <-ls.ErrCh:
		t.Errorf("no signal: unexpected error %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLoopbackSignal_CookieReceived(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	// Send a cookie on the channel before waiting.
	go func() {
		ls.CookieCh <- "SSO_C=got-it"
	}()
	select {
	case got := <-ls.CookieCh:
		if got != "SSO_C=got-it" {
			t.Errorf("CookieCh=%q want SSO_C=got-it", got)
		}
	case err := <-ls.ErrCh:
		t.Fatal(err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cookie signal never arrived")
	}
}

func TestLoopbackSignal_ErrorReceived(t *testing.T) {
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
	select {
	case err := <-ls.ErrCh:
		if !errors.Is(err, errBoom) {
			t.Errorf("ErrCh: got %v, want the errBoom sentinel", err)
		}
	case cookie := <-ls.CookieCh:
		t.Fatalf("unexpected cookie %q, want the errBoom sentinel", cookie)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("error signal never arrived")
	}
}

var errBoom = echoErr("boom")

type echoErr string

func (e echoErr) Error() string { return string(e) }
