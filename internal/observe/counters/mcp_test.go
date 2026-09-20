package counters

import "testing"

// TestMCPStatsRecordTool pins the per-tool dimension: RecordTool accumulates
// per (name, tool), empty tool names are skipped, and Reset clears both maps.
func TestMCPStatsRecordTool(t *testing.T) {
	s := NewMCPStats()
	s.RecordTool("srv", "search", 200, 100)
	s.RecordTool("srv", "search", 500, 50)
	s.RecordTool("srv", "fetch", 200, 30)
	s.RecordTool("rt", "lookup", 200, 10)
	s.RecordTool("srv", "", 200, 10) // no tool name: skipped

	tools := s.RawToolSnapshot()
	if len(tools) != 3 {
		t.Fatalf("tool snapshot = %+v", tools)
	}
	search := tools[MCPToolKey{Name: "srv", Tool: "search"}]
	if search.Calls != 2 || search.Errors != 1 || search.LatencySum != 150 {
		t.Errorf("srv/search = %+v", search)
	}
	if fetch := tools[MCPToolKey{Name: "srv", Tool: "fetch"}]; fetch.Calls != 1 || fetch.Errors != 0 {
		t.Errorf("srv/fetch = %+v", fetch)
	}
	if lookup := tools[MCPToolKey{Name: "rt", Tool: "lookup"}]; lookup.Calls != 1 {
		t.Errorf("rt/lookup = %+v", lookup)
	}
	if search.LastCallAt == 0 {
		t.Error("LastCallAt must be stamped")
	}

	// The per-name view stays server-level: RecordTool never touches it.
	if names := s.RawSnapshot(); len(names) != 0 {
		t.Errorf("RecordTool polluted the per-name map: %+v", names)
	}

	s.Reset()
	if len(s.RawToolSnapshot()) != 0 || len(s.RawSnapshot()) != 0 {
		t.Error("Reset must clear both maps")
	}
}

// TestMCPStatsNilSafety pins the nil-receiver guards on the tool dimension.
func TestMCPStatsNilSafety(t *testing.T) {
	var s *MCPStats
	s.RecordTool("srv", "search", 200, 10)
	if got := s.RawToolSnapshot(); len(got) != 0 {
		t.Errorf("nil RawToolSnapshot = %+v", got)
	}
	s.Reset() // must not panic
}
