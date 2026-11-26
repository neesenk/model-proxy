package requestlog

import "testing"

func TestSummarizeProjectsAgentAndUsage(t *testing.T) {
	got := Summarize(Record{
		Agent:       "pi",
		ParsedUsage: Usage{Input: 7, Output: 3, CacheRead: 2, CacheCreation: 1},
	})
	if got.Agent != "pi" {
		t.Errorf("agent = %q, want pi", got.Agent)
	}
	if got.Input != 7 || got.Output != 3 || got.CacheRead != 2 || got.CacheCreation != 1 {
		t.Errorf("usage = %+v, want 7/3/2/1", got)
	}
}
