package diag

import "testing"

// TestReplayTarget parses --to in both --to X and --to=X forms.
func TestReplayTarget(t *testing.T) {
	if got := ReplayTarget([]string{"id", "--to", "kimi"}); got != "kimi" {
		t.Errorf("--to X = %q want kimi", got)
	}
	if got := ReplayTarget([]string{"id", "--to=kimi"}); got != "kimi" {
		t.Errorf("--to=X = %q want kimi", got)
	}
	if got := ReplayTarget([]string{"id"}); got != "" {
		t.Errorf("missing --to = %q want empty", got)
	}
}
