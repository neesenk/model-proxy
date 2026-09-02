package login

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// WithNextCallback appends the loopback callback as the login URL's next
// parameter; a malformed URL passes through unchanged.
func TestWithNextCallback(t *testing.T) {
	got := WithNextCallback("http://sso.test/login?x=1", "http://127.0.0.1:1/cb")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.Query().Get("next") != "http://127.0.0.1:1/cb" || u.Query().Get("x") != "1" {
		t.Fatalf("next not appended: %q", got)
	}
	if got := WithNextCallback("://bad url", "http://cb"); got != "://bad url" {
		t.Fatalf("malformed URL not passed through: %q", got)
	}
}

// WaitForLoginSignal surfaces the deadline as the user-facing timeout error.
func TestWaitForLoginSignalTimeout(t *testing.T) {
	ls, err := NewLoopbackServer()
	if err != nil {
		t.Fatal(err)
	}
	defer ls.Stop()
	if err := WaitForLoginSignal(ls, 10*time.Millisecond); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("WaitForLoginSignal timeout error = %v", err)
	}
}
