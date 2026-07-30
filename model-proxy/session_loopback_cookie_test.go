package main

import (
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
	if _, err := ls.WaitForCookie(500 * time.Millisecond); err == nil {
		t.Error("WaitForCookie with errCh signal: want error, got nil")
	}
}

var errBoom = echoErr("boom")

type echoErr string

func (e echoErr) Error() string { return string(e) }
