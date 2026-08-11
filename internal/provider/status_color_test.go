package provider

import "testing"

// --- statusColor: all branches ---

func TestStatusColor_AllBranches(t *testing.T) {
	// Force log color on (it's off in tests: stderr is not a tty) so the
	// status→ANSI mapping is actually exercised — including branch edges.
	old := LogColorEnabled
	LogColorEnabled = true
	defer func() { LogColorEnabled = old }()
	for _, c := range []struct {
		status int
		code   string
	}{
		{200, LogAnsiGreen}, {299, LogAnsiGreen},
		{300, LogAnsiYellow}, {499, LogAnsiYellow},
		{500, LogAnsiRed},
		{100, LogAnsiGray}, {0, LogAnsiGray},
	} {
		want := c.code + "x" + LogAnsiReset
		if got := StatusColor(c.status, "x"); got != want {
			t.Errorf("StatusColor(%d)=%q, want %q", c.status, got, want)
		}
	}
	// Color off: identity passthrough.
	LogColorEnabled = false
	if got := StatusColor(200, "ok"); got != "ok" {
		t.Errorf("StatusColor(200) with color off=%q, want ok", got)
	}
}
