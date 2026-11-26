package requestlog

// GuardMark is one guard/adjudication annotation joined onto a request at
// QUERY time — the correlation of the security audit trail (interceptions,
// LLM verdicts, unblocks) with the request row, so the requests surfaces can
// show why a request was blocked or how it was judged without a second
// cross-page lookup. It is never part of the persisted request-log record:
// the authoritative row lives in the security audit log, keyed by RequestID.
// Fields mirror the audit projection (labels and scrubbed verdict text only —
// matched content never enters a mark).
type GuardMark struct {
	Ts       int64    `json:"ts"`
	Kind     string   `json:"kind"` // secret | path | unblock
	Names    []string `json:"names,omitempty"`
	Action   string   `json:"action,omitempty"`
	Verdict  string   `json:"verdict,omitempty"` // high | medium | low | error | skipped
	Reason   string   `json:"reason,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	Model    string   `json:"model,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Cached   bool     `json:"cached,omitempty"`
	// Source distinguishes the live adjudication ring ("judge" — includes the
	// ring-only low verdicts and cached attribution) from the durable audit
	// store ("audit").
	Source string `json:"source,omitempty"`
}

// FailedHop is one folded failover leg's metadata: which pool account was
// tried, how it failed (status) and how long it took. Bodies stay out (the
// failure evidence lives on the leg's own record, reachable via Detail).
type FailedHop struct {
	Provider  string `json:"provider"`
	Status    int    `json:"status"`
	Attempt   int    `json:"attempt"`
	LatencyMs int64  `json:"latency_ms"`
}

// Summary is the metadata-only list API projection.
type Summary struct {
	Ts        string `json:"ts"`
	RequestID string `json:"request_id"`
	// Kind is the traffic class discriminator: "" = LLM forward traffic,
	// "mcp" = MCP gateway exchange. Omitted from JSON for LLM rows
	// (back-compat with pre-kind consumers).
	Kind      string `json:"kind,omitempty"`
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol"`
	Method    string `json:"method"`
	Path      string `json:"path"`
	// Tool is the MCP tools/call tool name (client-facing); empty on LLM
	// rows, non-call MCP methods and records written before the field
	// existed.
	Tool          string `json:"tool,omitempty"`
	Exposed       string `json:"exposed"`
	CalledModel   string `json:"called_model"`
	UpstreamModel string `json:"upstream_model"`
	Provider      string `json:"provider"`
	Agent         string `json:"agent"`
	Attempt       int    `json:"attempt"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	TTFTMs        int64  `json:"ttft_ms,omitempty"`
	RequestSize   int    `json:"request_size"`
	ResponseSize  int64  `json:"response_size"`
	Input         uint64 `json:"input,omitempty"`
	Output        uint64 `json:"output,omitempty"`
	CacheRead     uint64 `json:"cache_read,omitempty"`
	CacheCreation uint64 `json:"cache_creation,omitempty"`
	Shadow        bool   `json:"shadow,omitempty"`
	// AttemptOnly marks an uncommitted failover leg (every hop of the chain,
	// the last one included): same request id as the request's committed row
	// or all-targets-failed terminal row,
	// request body intentionally not logged (the response body carries the
	// failure evidence), excluded from usage and session aggregates.
	AttemptOnly bool `json:"attempt_only,omitempty"`
	// FailedHops lists the failover legs folded into this row by the summary
	// list queries (index SQL and scan engine, identical semantics): the
	// hops that preceded the committed answer, in attempt order. Empty on
	// rows without failed hops and on unanswered legs themselves — except
	// the surviving (newest) leg of an unanswered request, which carries its
	// earlier hops. Metadata only: no error bodies (list-safe).
	FailedHops []FailedHop `json:"failed_hops,omitempty"`
	// TurnKey is the conversational-turn fingerprint persisted with the record.
	// Empty on records written before the field existed or when no user text
	// could be extracted.
	TurnKey string `json:"turn_key,omitempty"`
	// Guard carries the request's guard/adjudication annotations when the read
	// surface joins them in (admin's decorated query port); the raw
	// requestlog scan leaves it empty. Newest first.
	Guard []GuardMark `json:"guard,omitempty"`
}

// Summarize projects one full record to list-safe metadata.
func Summarize(record Record) Summary {
	return Summary{
		Ts: record.Ts, RequestID: record.RequestID, Kind: record.Kind, SessionID: record.SessionID,
		Protocol: record.Protocol, Method: record.Method, Path: record.Path, Tool: record.Tool,
		Exposed: record.Exposed, CalledModel: record.CalledModel,
		UpstreamModel: record.UpstreamModel, Provider: record.Provider,
		Agent: record.Agent, Attempt: record.Attempt, Status: record.Status, LatencyMs: record.LatencyMs,
		TTFTMs: record.TTFTMs, RequestSize: record.RequestSize, ResponseSize: record.ResponseSize,
		Input: record.ParsedUsage.Input, Output: record.ParsedUsage.Output,
		CacheRead: record.ParsedUsage.CacheRead, CacheCreation: record.ParsedUsage.CacheCreation,
		Shadow: record.Shadow, AttemptOnly: record.AttemptOnly, TurnKey: record.TurnKey,
		FailedHops: record.FailedHops,
	}
}

// QuerySummaries returns list-safe metadata. Bodies and response headers are
// discarded before top-K retention, so memory is bounded by metadata size.
func QuerySummaries(dir string, filter Filter) ([]Summary, error) {
	records, err := query(dir, filePrefix, filter, true, true, nil)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, nil
}

// QuerySummariesWithFacets is QuerySummaries plus the data-driven filter
// facets: the distinct providers/models observed in the scanned window (and
// the provider→models mapping for the linked dropdowns). Facets are collected
// from every decoded record before the request's own model/provider filter is
// applied, so the dropdowns never narrow themselves into a dead end.
func QuerySummariesWithFacets(dir string, filter Filter) ([]Summary, Facets, error) {
	collector := newFacetCollector(filter.Kind)
	records, err := query(dir, filePrefix, filter, true, true, collector)
	if err != nil {
		return nil, Facets{}, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, collector.facets(), nil
}

// QuerySummariesWithFacetsIn is QuerySummariesWithFacets for a non-default
// stream (only <prefix>*.log files are scanned — the split MCP stream's
// /api/requests?kind=mcp read path; facets stay data-driven over that
// stream's files).
func QuerySummariesWithFacetsIn(dir, prefix string, filter Filter) ([]Summary, Facets, error) {
	collector := newFacetCollector(filter.Kind)
	records, err := query(dir, prefix, filter, true, true, collector)
	if err != nil {
		return nil, Facets{}, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, collector.facets(), nil
}
