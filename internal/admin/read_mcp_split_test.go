package admin

import (
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/observe/requestlog"
)

// TestRequestLogQueriesMCPSplitRouting: with the split MCP stream configured
// (request_log.mcp_split on → MCPRequestLogDirectory port set), the query port
// routes kind=mcp listings to the split stream's directory scan, pins every
// other listing to LLM rows (legacy kind=mcp rows still present in old
// requests- files stop polluting the default view and its facets), and detail
// lookups fall through to the split stream for ids that only live there.
func TestRequestLogQueriesMCPSplitRouting(t *testing.T) {
	reqDir, mcpDir := t.TempDir(), t.TempDir()
	llmLine := `{"ts":"2026-09-18T10:00:00Z","request_id":"llm-1","called_model":"glm-5.3","provider":"zhipu","status":200}`
	// A legacy mcp row written before the split was enabled: it stays on disk
	// in the requests- stream but must leave the default listing + facets.
	legacyMcpLine := `{"ts":"2026-09-18T09:00:00Z","request_id":"old-mcp","kind":"mcp","exposed":"zhipu-search","provider":"zhipu#2","status":200}`
	if err := os.WriteFile(filepath.Join(reqDir, "requests-20260918.log"), []byte(llmLine+"\n"+legacyMcpLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcpLine := `{"ts":"2026-09-18T11:00:00Z","request_id":"mcp-1","kind":"mcp","exposed":"zhipu-search","provider":"zhipu#2","status":200,"request_body":"{}"}`
	if err := os.WriteFile(filepath.Join(mcpDir, "mcp-20260918.log"), []byte(mcpLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	service := New(Ports{
		RequestLogDirectory:    func() string { return reqDir },
		MCPRequestLogDirectory: func() string { return mcpDir },
	})
	queries := service.RequestLogQueries()
	if queries == nil {
		t.Fatal("RequestLogQueries = nil with the request log enabled")
	}

	// Default listing: LLM row only — the legacy mcp row is hidden.
	summaries, facets, err := queries.SummariesWithFacets(requestlog.Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "llm-1" {
		t.Fatalf("default summaries = %+v, want only llm-1", summaries)
	}
	if len(facets.Models) != 1 || facets.Models[0] != "glm-5.3" {
		t.Fatalf("default facets.Models = %v, want [glm-5.3] (no mcp server names)", facets.Models)
	}

	// kind=mcp: the split stream only (the legacy row is not served there).
	summaries, _, err = queries.SummariesWithFacets(requestlog.Filter{Kind: "mcp", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "mcp-1" || summaries[0].Kind != "mcp" {
		t.Fatalf("kind=mcp summaries = %+v, want the split-stream mcp-1 row", summaries)
	}

	// kind=llm: explicit LLM listing behaves like the default.
	summaries, _, err = queries.SummariesWithFacets(requestlog.Filter{Kind: "llm", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "llm-1" {
		t.Fatalf("kind=llm summaries = %+v, want llm-1", summaries)
	}

	// Detail: ids of both streams resolve (kind hints route directly).
	records, err := queries.Detail("llm-1", "")
	if err != nil || len(records) != 1 {
		t.Fatalf("detail llm-1 = (%+v, %v)", records, err)
	}
	records, err = queries.Detail("mcp-1", "")
	if err != nil || len(records) != 1 || records[0].RequestBody != "{}" {
		t.Fatalf("detail mcp-1 = (%+v, %v), want the split-stream record with body", records, err)
	}
	// The mcp hint resolves an MCP id that ALSO exists in the requests
	// stream (the pre-split era wrote mcp rows there): the hint wins —
	// the split stream is authoritative for MCP ids.
	dupMCP := `{"ts":"2026-09-18T08:00:00Z","request_id":"mcp-1","kind":"mcp","exposed":"stale","status":200}`
	if err := os.WriteFile(filepath.Join(reqDir, "requests-20260918.log"), []byte(llmLine+"\n"+legacyMcpLine+"\n"+dupMCP+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err = queries.Detail("mcp-1", "mcp")
	if err != nil || len(records) != 1 || records[0].RequestBody != "{}" {
		t.Fatalf("detail mcp-1 (hint) = (%+v, %v), want the split-stream record", records, err)
	}
}

// TestRequestLogQueriesWithoutSplitKeepsMixedSemantics: with the split off
// (no MCPRequestLogDirectory), the port keeps the historical semantics — the
// default listing serves every record in the requests- stream, mcp rows
// included (back-compat for deployments that never opt in).
func TestRequestLogQueriesWithoutSplitKeepsMixedSemantics(t *testing.T) {
	dir := t.TempDir()
	llmLine := `{"ts":"2026-09-18T10:00:00Z","request_id":"llm-1","called_model":"glm-5.3","provider":"zhipu","status":200}`
	mcpLine := `{"ts":"2026-09-18T11:00:00Z","request_id":"mcp-1","kind":"mcp","exposed":"zhipu-search","provider":"zhipu#2","status":200}`
	if err := os.WriteFile(filepath.Join(dir, "requests-20260918.log"), []byte(llmLine+"\n"+mcpLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Ports{RequestLogDirectory: func() string { return dir }})
	queries := service.RequestLogQueries()
	summaries, facets, err := queries.SummariesWithFacets(requestlog.Filter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("unsplit default summaries = %+v, want both rows", summaries)
	}
	if len(facets.Models) != 2 {
		t.Fatalf("unsplit facets.Models = %v, want both values", facets.Models)
	}
}
