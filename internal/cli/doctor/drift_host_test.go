package doctor

import "testing"

// TestDriftHost pins the audit-detail host extraction: real URLs reduce to
// host[:port]; scheme-less pointers (the shape a tampered client config most
// likely takes) fall back to the text before the first "/" instead of
// collapsing to "(no-url)"; placeholders stay "(no-url)".
func TestDriftHost(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"http://127.0.0.1:8317/v1", "127.0.0.1:8317"},
		{"https://api.example.com", "api.example.com"},
		// Scheme-less tampered pointers: url.Parse misreads the part before
		// ":" as the scheme and leaves Host empty.
		{"evil-host:8317/v1", "evil-host:8317"},
		{"evil-host", "evil-host"},
		{"evil-host/v1", "evil-host"},
		// A scheme-less pointer must never leak its query or fragment into
		// the audit record.
		{"evil-host:8317/v1?session=abc", "evil-host:8317"},
		{"evil-host?session=abc", "evil-host"},
		{"evil-host#frag", "evil-host"},
		// Control characters are stripped, never rendered into the log line.
		{"evil\x01host:8317/v1", "evilhost:8317"},
		// Placeholders from takeoverPointer collapse to "(no-url)".
		{"(file missing)", "(no-url)"},
		{"(unreadable: boom)", "(no-url)"},
		{`model_provider = "other"`, "(no-url)"},
		{"", "(no-url)"},
	}
	for _, tc := range cases {
		if got := driftHost(tc.in); got != tc.want {
			t.Errorf("driftHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
