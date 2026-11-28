package requestlog

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Filter narrows a request-log query. Shadow is "", "only", or "exclude".
// UsageOnly strips the request/response bodies and the header blob as records
// are read, after parsing each response body's usage into ParsedUsage — a
// top-K of N records then holds metadata-sized records instead of N full
// bodies (max_body_bytes is 5MiB per side by default, so N=2000 full records
// could transiently pin gigabytes).
type Filter struct {
	Model      string
	Provider   string
	Agent      string
	Status     int
	ErrorsOnly bool
	RequestID  string
	Session    string
	Shadow     string
	// Kind filters the traffic class: "" = all, "mcp" = MCP gateway records
	// only, "llm" = LLM forward records (kind-less) only.
	Kind      string
	From      time.Time
	To        time.Time
	Limit     int
	UsageOnly bool
}

func (f Filter) matches(record Record) bool {
	if f.RequestID != "" && record.RequestID != f.RequestID {
		return false
	}
	if f.Session != "" && record.SessionID != f.Session {
		return false
	}
	switch f.Kind {
	case "mcp":
		if record.Kind != "mcp" {
			return false
		}
	case "llm":
		if record.Kind != "" {
			return false
		}
	}
	if f.Model != "" &&
		!containsFold(record.CalledModel, f.Model) &&
		!containsFold(record.UpstreamModel, f.Model) &&
		!containsFold(record.Exposed, f.Model) {
		return false
	}
	if f.Provider != "" && !containsFold(record.Provider, f.Provider) {
		return false
	}
	// Agent is an exact match (not the substring match model/provider use):
	// the labels are a closed, short set produced by counters.DetectAgent and
	// the UI offers them as a facet-driven dropdown, so a substring would let
	// "claude-code" also select a hypothetical "claude-code-router".
	if f.Agent != "" && record.Agent != f.Agent {
		return false
	}
	if f.Status != 0 && record.Status != f.Status {
		return false
	}
	if f.ErrorsOnly && record.Status < 400 {
		return false
	}
	switch f.Shadow {
	case "only":
		if !record.Shadow {
			return false
		}
	case "exclude":
		if record.Shadow {
			return false
		}
	}
	if !f.From.IsZero() || !f.To.IsZero() {
		timestamp, err := time.Parse(time.RFC3339, record.Ts)
		if err != nil {
			return false
		}
		if !f.From.IsZero() && timestamp.Before(f.From) {
			return false
		}
		if !f.To.IsZero() && timestamp.After(f.To) {
			return false
		}
	}
	return true
}

func containsFold(value, substring string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substring))
}

// rawPrefilter is a cheap byte-level gate before json.Unmarshal. A RequestID or
// Session query only admits lines that literally contain the value, so a
// multi-MB body line that cannot match skips the decode (and its body string
// allocations) entirely. A false positive — the value appearing inside a body —
// is still rejected by Filter.matches after decoding.
func rawPrefilter(line []byte, filter Filter) bool {
	if filter.RequestID != "" && !bytes.Contains(line, []byte(filter.RequestID)) {
		return false
	}
	if filter.Session != "" && !bytes.Contains(line, []byte(filter.Session)) {
		return false
	}
	return true
}

// QueryRecords streams every request-log file and returns matching full records
// newest first. A positive limit retains only a timestamp top-K in memory.
//
// Early termination (file granularity): a single writer appends records to one
// file in non-decreasing Ts order, so a file's last admissible record bounds
// everything in it. Once the top-K heap is full, a record strictly older than
// the heap floor is pushed and immediately evicted — a no-op — so a file whose
// newest record is older than the floor cannot change the result and is
// skipped without streaming; likewise a file whose newest record is older than
// the From bound holds no match. The ordering premise is VERIFIED per file,
// not assumed: both the first and the last record are peeked, and any anomaly
// — unopenable file, oversized record, unparseable JSON, or firstTs after
// lastTs (hand-built or corrupted file) — falls back to streaming the file as
// before. Cross-file disorder (an orphan active file or a clock correction
// letting an older-named file hold newer records) stays tolerated: every file
// is peeked independently.
func QueryRecords(dir string, filter Filter) ([]Record, error) {
	return query(dir, filePrefix, filter, false, false, nil)
}

// QueryRecordsIn is QueryRecords for a non-default stream: only files named
// <prefix>*.log in dir are scanned (e.g. the split MCP stream's mcp- files).
func QueryRecordsIn(dir, prefix string, filter Filter) ([]Record, error) {
	return query(dir, prefix, filter, false, false, nil)
}

// Facets are the distinct providers, models and agents observed in the scanned
// request-log window. The filter dropdowns must offer what actually appears in
// the log, NOT the configured provider/model catalog (config can list models
// with no traffic yet, and the log can hold models since removed from config);
// agents are never configured at all, so the log is their only source.
// ProviderModels maps each provider to the models seen with it, so the UI can
// link the provider and model dropdowns from data.
type Facets struct {
	Providers      []string            `json:"providers"`
	Models         []string            `json:"models"`
	Agents         []string            `json:"agents"`
	ProviderModels map[string][]string `json:"provider_models"`
}

// facetCollector accumulates the distinct values during one scan. It is fed
// every decoded record BEFORE filter.matches, so the dropdowns are not narrowed
// by the currently applied model/provider filter. The one exception is Kind:
// it is a stream selector, not a data facet (an LLM-only view must not offer
// mcp server names in its model dropdown), so records of the other stream are
// skipped.
type facetCollector struct {
	kind       string
	providers  map[string]bool
	models     map[string]bool
	agents     map[string]bool
	byProvider map[string]map[string]bool
}

func newFacetCollector(kind string) *facetCollector {
	return &facetCollector{
		kind:       kind,
		providers:  map[string]bool{},
		models:     map[string]bool{},
		agents:     map[string]bool{},
		byProvider: map[string]map[string]bool{},
	}
}

func (c *facetCollector) add(record Record) {
	switch c.kind {
	case "mcp":
		if record.Kind != "mcp" {
			return
		}
	case "llm":
		if record.Kind != "" {
			return
		}
	}
	model := record.Exposed
	if model == "" {
		model = record.CalledModel
	}
	if record.Provider != "" {
		c.providers[facetProviderName(record.Provider)] = true
	}
	if model != "" {
		c.models[model] = true
	}
	if record.Agent != "" {
		c.agents[record.Agent] = true
	}
	if record.Provider != "" && model != "" {
		provider := facetProviderName(record.Provider)
		if c.byProvider[provider] == nil {
			c.byProvider[provider] = map[string]bool{}
		}
		c.byProvider[provider][model] = true
	}
}

// facetProviderName folds a runtime provider key to its operator-facing facet
// name: pooled records carry the virtual "name#<accountID>" key, but the
// provider dropdown offers the PARENT name (the id hash is correlation-only
// and never user-facing). The provider FILTER stays substring-based, so
// picking the parent matches every account's rows.
func facetProviderName(provider string) string {
	if i := strings.IndexByte(provider, '#'); i >= 0 {
		return provider[:i]
	}
	return provider
}

func (c *facetCollector) facets() Facets {
	out := Facets{
		Providers:      sortedFacetKeys(c.providers),
		Models:         sortedFacetKeys(c.models),
		Agents:         sortedFacetKeys(c.agents),
		ProviderModels: make(map[string][]string, len(c.byProvider)),
	}
	for provider, models := range c.byProvider {
		out.ProviderModels[provider] = sortedFacetKeys(models)
	}
	return out
}

func sortedFacetKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// peekLastRecordTs reads only the trailing chunk of the JSONL file at path and
// returns the Ts of the last record the streaming scan would admit (the last
// non-empty line, newline-terminated or not — the reader parses an
// unterminated final line too). ok=false on any anomaly — unreadable or empty
// file, a final line longer than the peek chunk, unparseable JSON — in which
// case the caller must stream the file as before.
func peekLastRecordTs(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false
	}
	const chunk = 128 << 10
	size := info.Size()
	offset := int64(0)
	if size > chunk {
		offset = size - chunk
	}
	buf := make([]byte, size-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return "", false
	}
	buf = bytes.TrimRight(buf, "\n")
	if len(buf) == 0 {
		return "", false
	}
	start := bytes.LastIndexByte(buf, '\n')
	if start < 0 && offset > 0 {
		// The final line begins before the chunk (an up-to-max_body_bytes
		// record): its start is unverifiable, so do not trust it as a bound.
		return "", false
	}
	var record struct {
		Ts string `json:"ts"`
	}
	if json.Unmarshal(buf[start+1:], &record) != nil {
		return "", false
	}
	return record.Ts, true
}

// peekFirstRecordTs reads only the leading chunk of the JSONL file at path
// and returns the Ts of the FIRST non-empty line (newline-terminated or not —
// the streaming reader parses an unterminated line too). Together with
// peekLastRecordTs it verifies the single-writer append-order premise the
// file-level early termination relies on: a first record newer than the last
// means a hand-built or corrupted file, and the caller must stream it.
// ok=false on any anomaly — unreadable or empty file, a first line longer
// than the peek chunk, unparseable JSON — in which case the caller must
// stream the file as before.
func peekFirstRecordTs(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false
	}
	const chunk = 128 << 10
	size := info.Size()
	if size > chunk {
		size = chunk
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, 0); err != nil {
		return "", false
	}
	end := bytes.IndexByte(buf, '\n')
	if end < 0 {
		// The first line fills the whole chunk (an up-to-max_body_bytes
		// record): its end is unverifiable, so do not trust it as a bound.
		return "", false
	}
	line := bytes.TrimSpace(buf[:end])
	if len(line) == 0 {
		return "", false
	}
	var record struct {
		Ts string `json:"ts"`
	}
	if json.Unmarshal(line, &record) != nil {
		return "", false
	}
	return record.Ts, true
}

// query streams the prefix's files newest-first. metadataOnly strips bodies
// as records match; foldLegs switches the summary-list fold on: attempt_only
// legs stop competing for row slots — paired legs attach to their committed
// row's FailedHops instead, and an unanswered request (no committed row,
// e.g. the client disconnecting mid-failover) keeps its newest leg as its
// only row. The fold and the row order must stay semantics-identical to the
// index SQL in Indexer.SummariesWithFacets (pinned by index==scan tests):
// newest-first means ORDER BY ts DESC, rowid DESC, and Ts has one-second
// resolution, so rows sharing a second are ordered by write position
// (see scanned).
func query(dir, prefix string, filter Filter, metadataOnly, foldLegs bool, facets *facetCollector) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".log") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var rows []scanned
	var newest *timestampMinHeap
	if filter.Limit > 0 {
		newest = &timestampMinHeap{}
	}
	keep := func(entry scanned) {
		if newest == nil {
			rows = append(rows, entry)
			return
		}
		heap.Push(newest, entry)
		if newest.Len() > filter.Limit {
			heap.Pop(newest)
		}
	}
	// Fold state: legs are held per request id as hop telemetry (bypassing
	// filter.matches and the top-K — the index fold's hop subquery ignores
	// the filter the same way), and every decoded committed id is tracked
	// pre-filter so a filtered-out commit (e.g. ErrorsOnly dropping the 200)
	// still folds its legs away.
	var legsByID map[string][]scanned
	var committedIDs map[string]struct{}
	if foldLegs {
		legsByID = make(map[string][]scanned)
		committedIDs = make(map[string]struct{})
	}
	// Request-id lookup state: the smallest attempt decoded for the id.
	// A request's rows carry attempt 0 for its first row, so once that has
	// been decoded no older file can hold another row of the request.
	firstAttempt, matched := 0, false
	found := false
	for i := len(names) - 1; i >= 0 && !found; i-- {
		path := filepath.Join(dir, names[i])
		lastTs, lastOK := peekLastRecordTs(path)
		firstTs, firstOK := peekFirstRecordTs(path)
		// RFC3339 offsets make lexicographic order unreliable; compare
		// parsed instants. Any parse failure is an anomaly → stream.
		ordered := false
		if lastOK && firstOK {
			if first, err1 := time.Parse(time.RFC3339, firstTs); err1 == nil {
				if last, err2 := time.Parse(time.RFC3339, lastTs); err2 == nil {
					ordered = !first.After(last)
				}
			}
		}
		if ordered {
			// Ordering verified: the file's NEWEST record bounds everything
			// in it. A full heap makes every strictly-older record a
			// push-then-evict no-op, and From is an inclusive lower bound
			// (matches drops only timestamps before From) — either way the
			// whole file is provably irrelevant once that record qualifies.
			if newest != nil && newest.Len() >= filter.Limit && lastTs < (*newest)[0].record.Ts {
				continue
			}
			if !filter.From.IsZero() {
				if t, err := time.Parse(time.RFC3339, lastTs); err == nil && t.Before(filter.From) {
					continue
				}
			}
			// Mirror image on the OLDEST record: files iterate newest-first,
			// so a historical To window (the UI's minute filter) meets every
			// file ENTIRELY newer than To before it reaches the window's own
			// files — skipping them keeps the scan from streaming and
			// JSON-decoding all records logged since that minute. To is an
			// inclusive upper bound (matches drops only timestamps after To).
			if !filter.To.IsZero() {
				if t, err := time.Parse(time.RFC3339, firstTs); err == nil && t.After(filter.To) {
					continue
				}
			}
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := bufio.NewReaderSize(file, 64*1024)
		line := 0
		for {
			raw, readErr := reader.ReadBytes('\n')
			entry := scanned{rank: i, line: line}
			line++
			if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && rawPrefilter(trimmed, filter) {
				var record Record
				if json.Unmarshal(trimmed, &record) == nil {
					// Legs never feed the facets: the dropdowns describe the
					// traffic that produces rows, and legs fold away.
					if facets != nil && !record.AttemptOnly {
						facets.add(record)
					}
					if foldLegs && record.AttemptOnly {
						// Hold every leg as hop telemetry, bodies stripped. A
						// leg is a row only when its request never commits,
						// which is known once the scan has decoded every
						// commit — so legs join the row set after the scan and
						// never the top-K: paired legs must not take slots the
						// index's fold (its predicate runs before LIMIT) leaves
						// to committed rows.
						record.RequestBody, record.ResponseBody, record.ResponseHeaders = "", "", ""
						entry.record = record
						legsByID[record.RequestID] = append(legsByID[record.RequestID], entry)
					} else {
						if foldLegs {
							committedIDs[record.RequestID] = struct{}{}
						}
						if filter.matches(record) {
							if metadataOnly || filter.UsageOnly {
								if filter.UsageOnly {
									record.ParsedUsage = ExtractUsage(record.ResponseBody)
								}
								record.RequestBody = ""
								record.ResponseBody = ""
								record.ResponseHeaders = ""
							}
							entry.record = record
							keep(entry)
						}
					}
					if filter.RequestID != "" && record.RequestID == filter.RequestID {
						if !matched || record.Attempt < firstAttempt {
							firstAttempt = record.Attempt
						}
						matched = true
					}
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = file.Close()
		if matched && firstAttempt == 0 {
			// A request-id lookup streams to the end of the file its rows
			// were found in (the answer is written after the legs) and stops
			// before any older file once the request's first row is in hand
			// — this is what keeps opening a record cheap on a multi-GB log.
			found = true
		}
	}
	if newest != nil {
		rows = *newest
	}
	if foldLegs {
		// An unanswered request surfaces as exactly one row: its newest leg.
		for id, legs := range legsByID {
			if _, answered := committedIDs[id]; answered {
				continue
			}
			survivor := legs[0]
			for _, leg := range legs[1:] {
				if newerFirst(leg, survivor) {
					survivor = leg
				}
			}
			if filter.matches(survivor.record) {
				rows = append(rows, survivor)
			}
		}
	}
	sort.Slice(rows, func(i, j int) bool { return newerFirst(rows[i], rows[j]) })
	if filter.Limit > 0 && len(rows) > filter.Limit {
		rows = rows[:filter.Limit]
	}
	records := make([]Record, len(rows))
	for i := range rows {
		records[i] = rows[i].record
	}
	if foldLegs {
		records = foldHeldLegs(records, legsByID)
	}
	return records, nil
}

// scanned is one streamed record with its write position. Ts has one-second
// resolution and a failover writes a request's rows milliseconds apart, so
// newest-first needs the write order the index records as rowid (ORDER BY
// ts DESC, rowid DESC): the scan streams files newest-first but each file's
// lines oldest-first, so that order is (file rank in name order, line).
type scanned struct {
	record Record
	rank   int
	line   int
}

// newerFirst is the scan's row order — the index's ORDER BY ts DESC, rowid DESC.
func newerFirst(a, b scanned) bool {
	if a.record.Ts != b.record.Ts {
		return a.record.Ts > b.record.Ts
	}
	if a.rank != b.rank {
		return a.rank > b.rank
	}
	return a.line > b.line
}

// foldHeldLegs attaches each row's held legs as FailedHops on the newest-first
// row set: a committed row carries its request's whole hop chain, and an
// unanswered request's surviving leg carries the earlier legs. Paired legs
// never surface as rows — they enter the row set only without a commit.
func foldHeldLegs(records []Record, legsByID map[string][]scanned) []Record {
	for i := range records {
		exclude := Record{}
		if records[i].AttemptOnly {
			exclude = records[i]
		}
		records[i].FailedHops = heldHops(legsByID[records[i].RequestID], exclude)
	}
	return records
}

// heldHops projects a request's held legs into FailedHops, attempt-ascending
// (write order; Ts as the tie-break). exclude drops the survivor leg itself
// from an unanswered request's own hop chain (zero Record excludes nothing).
func heldHops(legs []scanned, exclude Record) []FailedHop {
	if len(legs) == 0 {
		return nil
	}
	sorted := make([]scanned, len(legs))
	copy(sorted, legs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].record.Attempt != sorted[j].record.Attempt {
			return sorted[i].record.Attempt < sorted[j].record.Attempt
		}
		return sorted[i].record.Ts < sorted[j].record.Ts
	})
	hops := make([]FailedHop, 0, len(sorted))
	for _, leg := range sorted {
		leg := leg.record
		if exclude.RequestID != "" && leg.Attempt == exclude.Attempt && leg.Ts == exclude.Ts {
			continue
		}
		hops = append(hops, FailedHop{
			Provider: leg.Provider, Status: leg.Status,
			Attempt: leg.Attempt, LatencyMs: leg.LatencyMs,
		})
	}
	if len(hops) == 0 {
		return nil
	}
	return hops
}

// timestampMinHeap keeps the newest Limit rows: Less ranks the row a full
// heap evicts first — the smallest Ts and, among rows sharing a second, the
// earliest written, the one the index's LIMIT drops too.
type timestampMinHeap []scanned

func (h timestampMinHeap) Len() int           { return len(h) }
func (h timestampMinHeap) Less(i, j int) bool { return newerFirst(h[j], h[i]) }
func (h timestampMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timestampMinHeap) Push(value any)    { *h = append(*h, value.(scanned)) }
func (h *timestampMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}
