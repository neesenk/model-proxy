package requestlog

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMCPStreamWritesOwnPrefixAndQueryScansOnlyIt: the split MCP logger
// (FilePrefix = MCPFilePrefix) writes mcp-YYYYMMDD.log files, and the
// prefixed query surface reads them while ignoring requests- files in the
// same directory — the storage half of request_log.mcp_split.
func TestMCPStreamWritesOwnPrefixAndQueryScansOnlyIt(t *testing.T) {
	dir := t.TempDir()
	// Both streams share one directory here on purpose: the prefixed query
	// must pick only its own files even with the other stream's files (and
	// same-day names) beside them.
	llm := New(Options{Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 1024})
	mcp := New(Options{Directory: dir, FilePrefix: MCPFilePrefix, MaxFileSize: 1 << 30, MaxBodyBytes: 1024})
	go llm.Run()
	go mcp.Run()
	llm.Enqueue(&Record{Ts: "2026-09-18T01:00:00Z", RequestID: "llm-1"})
	mcp.Enqueue(&Record{Ts: "2026-09-18T01:00:01Z", RequestID: "mcp-1", Kind: "mcp", Protocol: "mcp", Method: "initialize", Path: "/mcp/zs", Exposed: "zs"})
	mcp.Shutdown()
	llm.Shutdown()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var mcpFiles, llmFiles int
	for _, e := range entries {
		switch {
		case strings.HasPrefix(e.Name(), "mcp-") && strings.HasSuffix(e.Name(), ".log"):
			mcpFiles++
		case strings.HasPrefix(e.Name(), "requests-") && strings.HasSuffix(e.Name(), ".log"):
			llmFiles++
		}
	}
	if mcpFiles != 1 || llmFiles != 1 {
		t.Fatalf("files: mcp=%d llm=%d, want 1 each (entries: %v)", mcpFiles, llmFiles, entries)
	}

	records, err := QueryRecordsIn(dir, MCPFilePrefix, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != "mcp-1" || records[0].Kind != "mcp" {
		t.Fatalf("QueryRecordsIn(mcp) = %+v, want the single mcp-1 record", records)
	}
	// The default stream's query must not see the mcp- file either.
	records, err = QueryRecords(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].RequestID != "llm-1" {
		t.Fatalf("QueryRecords = %+v, want the single llm-1 record", records)
	}

	summaries, facets, err := QuerySummariesWithFacetsIn(dir, MCPFilePrefix, Filter{Kind: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Kind != "mcp" {
		t.Fatalf("QuerySummariesWithFacetsIn(mcp) = %+v, want the single mcp summary", summaries)
	}
	// The mcp stream's model facet is the server name (Exposed), never the
	// LLM record's model.
	if len(facets.Models) != 1 || facets.Models[0] != "zs" {
		t.Fatalf("mcp facets.Models = %v, want [zs]", facets.Models)
	}
}

// TestFacetsFollowKindStreamSelector: Kind is a stream selector rather than a
// data facet — an llm-filtered scan must not offer mcp server names in its
// facets (and vice versa), while the unfiltered default keeps every value.
func TestFacetsFollowKindStreamSelector(t *testing.T) {
	dir := t.TempDir()
	lines := []Record{
		{Ts: "2026-09-18T01:00:00Z", RequestID: "l1", Exposed: "glm-5.3", Provider: "zhipu", Kind: ""},
		{Ts: "2026-09-18T01:00:01Z", RequestID: "m1", Exposed: "zhipu-search", Provider: "zhipu#2", Kind: "mcp"},
	}
	writePlainRecords(t, dir, "requests-20260918.log", lines)

	_, facets, err := QuerySummariesWithFacets(dir, Filter{Kind: "llm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Models) != 1 || facets.Models[0] != "glm-5.3" {
		t.Fatalf("llm facets.Models = %v, want [glm-5.3]", facets.Models)
	}
	if len(facets.Providers) != 1 || facets.Providers[0] != "zhipu" {
		t.Fatalf("llm facets.Providers = %v, want [zhipu]", facets.Providers)
	}

	_, facets, err = QuerySummariesWithFacets(dir, Filter{Kind: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Models) != 1 || facets.Models[0] != "zhipu-search" {
		t.Fatalf("mcp facets.Models = %v, want [zhipu-search]", facets.Models)
	}

	_, facets, err = QuerySummariesWithFacets(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(facets.Models) != 2 {
		t.Fatalf("unfiltered facets.Models = %v, want both streams' values", facets.Models)
	}
}

// TestMCPStreamRetentionSweepsOwnPrefixOnly: the split stream's retention
// sweep deletes only its own rotated files — a stale mcp- archive goes, a
// same-age requests- archive in the same directory stays (each logger owns
// its prefix).
func TestMCPStreamRetentionSweepsOwnPrefixOnly(t *testing.T) {
	dir := t.TempDir()
	stale := time.Now().Add(-48 * time.Hour)
	for _, name := range []string{"mcp-20260916--010101-1.log", "requests-20260916--010101-1.log"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	mcp := New(Options{Directory: dir, FilePrefix: MCPFilePrefix, MaxFileSize: 1 << 30, Retention: time.Hour})
	go mcp.Run()
	mcp.Enqueue(&Record{Ts: "2026-09-18T01:00:00Z", RequestID: "mcp-1", Kind: "mcp"})
	mcp.Shutdown()

	if _, err := os.Stat(filepath.Join(dir, "mcp-20260916--010101-1.log")); !os.IsNotExist(err) {
		t.Errorf("stale mcp archive survived the sweep (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "requests-20260916--010101-1.log")); err != nil {
		t.Errorf("requests archive swept by the mcp logger: %v", err)
	}
}

func writePlainRecords(t *testing.T, dir, name string, records []Record) {
	t.Helper()
	var b strings.Builder
	for _, r := range records {
		b.WriteString(fmt.Sprintf("{\"ts\":%q,\"request_id\":%q,\"kind\":%q,\"exposed\":%q,\"provider\":%q}\n", r.Ts, r.RequestID, r.Kind, r.Exposed, r.Provider))
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}
