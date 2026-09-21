package requestlog

import "sort"

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
		Shadow: record.Shadow, TurnKey: record.TurnKey,
	}
}

// QuerySummaries returns list-safe metadata. Bodies and response headers are
// discarded before top-K retention, so memory is bounded by metadata size.
func QuerySummaries(dir string, filter Filter) ([]Summary, error) {
	records, err := query(dir, filePrefix, filter, true, nil)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, nil
}

// QuerySummariesIn is QuerySummaries for a non-default stream (only
// <prefix>*.log files are scanned — the split MCP stream's read path).
func QuerySummariesIn(dir, prefix string, filter Filter) ([]Summary, error) {
	records, err := query(dir, prefix, filter, true, nil)
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
	records, err := query(dir, filePrefix, filter, true, collector)
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
	records, err := query(dir, prefix, filter, true, collector)
	if err != nil {
		return nil, Facets{}, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, collector.facets(), nil
}

// ShadowReportEntry aggregates paired primary/shadow observations.
type ShadowReportEntry struct {
	Route            string  `json:"route"`
	PrimaryProvider  string  `json:"primary_provider"`
	ShadowProvider   string  `json:"shadow_provider"`
	Samples          int     `json:"samples"`
	StatusMatchRate  float64 `json:"status_match_rate"`
	PrimaryLatencyMs int64   `json:"primary_latency_ms"`
	ShadowLatencyMs  int64   `json:"shadow_latency_ms"`
	LatencyDiffMs    int64   `json:"latency_diff_ms"`
	PrimarySizeAvg   int64   `json:"primary_size_avg"`
	ShadowSizeAvg    int64   `json:"shadow_size_avg"`
}

// ShadowReport pairs primary records with shadow-<request-id> records and
// aggregates them by route and provider pair.
func ShadowReport(dir string, filter Filter) ([]ShadowReportEntry, error) {
	records, err := query(dir, filePrefix, filter, true, nil)
	if err != nil {
		return nil, err
	}
	rows := make([]shadowPairRow, len(records))
	for i := range records {
		rows[i] = shadowPairRow{
			RequestID: records[i].RequestID, Shadow: records[i].Shadow,
			Exposed: records[i].Exposed, Provider: records[i].Provider,
			Status: records[i].Status, LatencyMs: records[i].LatencyMs,
			ResponseSize: records[i].ResponseSize,
		}
	}
	return aggregateShadowPairs(rows), nil
}

// shadowPairRow is the metadata projection the shadow report aggregates over:
// request identity and outcome only, never bodies. The directory scan
// (ShadowReport) and the SQLite index (Indexer.ShadowReport) both feed these
// rows into aggregateShadowPairs so the two surfaces answer identically.
type shadowPairRow struct {
	RequestID    string
	Shadow       bool
	Exposed      string
	Provider     string
	Status       int
	LatencyMs    int64
	ResponseSize int64
}

// aggregateShadowPairs pairs primary rows with shadow-<request-id> rows and
// aggregates them by route and provider pair. Unpaired rows on either side
// are skipped; entries sort by sample count (desc) then route.
func aggregateShadowPairs(rows []shadowPairRow) []ShadowReportEntry {
	type pair struct {
		primary *shadowPairRow
		shadow  *shadowPairRow
	}
	pairs := map[string]*pair{}
	for i := range rows {
		row := &rows[i]
		id := row.RequestID
		if row.Shadow {
			id = trimShadowPrefix(id)
		}
		current := pairs[id]
		if current == nil {
			current = &pair{}
			pairs[id] = current
		}
		if row.Shadow {
			current.shadow = row
		} else {
			current.primary = row
		}
	}

	type aggregateKey struct {
		route   string
		primary string
		shadow  string
	}
	type aggregate struct {
		count       int64
		statusMatch int64
		primaryLat  int64
		shadowLat   int64
		primarySize int64
		shadowSize  int64
	}
	aggregates := map[aggregateKey]*aggregate{}
	for _, pair := range pairs {
		if pair.primary == nil || pair.shadow == nil {
			continue
		}
		key := aggregateKey{
			route:   pair.primary.Exposed,
			primary: pair.primary.Provider,
			shadow:  pair.shadow.Provider,
		}
		value := aggregates[key]
		if value == nil {
			value = &aggregate{}
			aggregates[key] = value
		}
		value.count++
		if (pair.primary.Status < 300) == (pair.shadow.Status < 300) {
			value.statusMatch++
		}
		value.primaryLat += pair.primary.LatencyMs
		value.shadowLat += pair.shadow.LatencyMs
		value.primarySize += pair.primary.ResponseSize
		value.shadowSize += pair.shadow.ResponseSize
	}

	result := make([]ShadowReportEntry, 0, len(aggregates))
	for key, value := range aggregates {
		primaryLatency := value.primaryLat / value.count
		shadowLatency := value.shadowLat / value.count
		result = append(result, ShadowReportEntry{
			Route: key.route, PrimaryProvider: key.primary, ShadowProvider: key.shadow,
			Samples:          int(value.count),
			StatusMatchRate:  float64(value.statusMatch) / float64(value.count),
			PrimaryLatencyMs: primaryLatency, ShadowLatencyMs: shadowLatency,
			LatencyDiffMs:  shadowLatency - primaryLatency,
			PrimarySizeAvg: value.primarySize / value.count,
			ShadowSizeAvg:  value.shadowSize / value.count,
		})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Samples != result[j].Samples {
			return result[i].Samples > result[j].Samples
		}
		return result[i].Route < result[j].Route
	})
	return result
}

func trimShadowPrefix(id string) string {
	const prefix = "shadow-"
	if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
		return id[len(prefix):]
	}
	return id
}
