package main

import (
	"fmt"
	"testing"
	"time"
)

// TestPositionalArgs: --flag value pairs are skipped; bare positionals are kept
// in order (used by pin/unpin/replay to pull <route> [<provider>] / <id>).
func TestPositionalArgs(t *testing.T) {
	got := positionalArgs([]string{"glm", "--config", "x.yaml", "zhipu", "--ttl", "1h"})
	if len(got) != 2 || got[0] != "glm" || got[1] != "zhipu" {
		t.Errorf("positionalArgs=%v want [glm zhipu]", got)
	}
	// --flag=value form doesn't consume a following bare token.
	got = positionalArgs([]string{"--config=x.yaml", "glm"})
	if len(got) != 1 || got[0] != "glm" {
		t.Errorf("positionalArgs=%v want [glm]", got)
	}
}

// TestParsePinTTL: both --ttl DUR and --ttl=DUR forms parse; absent → 0.
func TestParsePinTTL(t *testing.T) {
	if d := parsePinTTL([]string{"--ttl", "90m"}); d != 90*time.Minute {
		t.Errorf("--ttl 90m = %v want 90m", d)
	}
	if d := parsePinTTL([]string{"--ttl=2h"}); d != 2*time.Hour {
		t.Errorf("--ttl=2h = %v want 2h", d)
	}
	if d := parsePinTTL([]string{"glm", "zhipu"}); d != 0 {
		t.Errorf("absent --ttl = %v want 0", d)
	}
}

// TestIsDaemonUnreachable: connection-refused errors match, others don't.
func TestIsDaemonUnreachable(t *testing.T) {
	if !isDaemonUnreachable(fmt.Errorf("dial tcp 127.0.0.1:8080: connect: connection refused")) {
		t.Error("connection refused should match")
	}
	if isDaemonUnreachable(fmt.Errorf("some other error")) {
		t.Error("non-refused error should not match")
	}
}

func TestMakeURLQuery(t *testing.T) {
	q := makeURLQuery([]string{"--from", "100", "--to", "200"})
	if q.Get("from") != "100" || q.Get("to") != "200" {
		t.Errorf("makeURLQuery: from=%q to=%q want 100/200", q.Get("from"), q.Get("to"))
	}
	q2 := makeURLQuery(nil)
	if q2.Encode() != "" {
		t.Errorf("makeURLQuery(nil) should be empty, got %q", q2.Encode())
	}
}
