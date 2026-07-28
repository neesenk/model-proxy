package requestlog

import "sort"

// Summary is the metadata-only list API projection.
type Summary struct {
	Ts            string `json:"ts"`
	RequestID     string `json:"request_id"`
	SessionID     string `json:"session_id"`
	Protocol      string `json:"protocol"`
	Method        string `json:"method"`
	Path          string `json:"path"`
	Exposed       string `json:"exposed"`
	CalledModel   string `json:"called_model"`
	UpstreamModel string `json:"upstream_model"`
	Provider      string `json:"provider"`
	Attempt       int    `json:"attempt"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	RequestSize   int    `json:"request_size"`
	ResponseSize  int64  `json:"response_size"`
	Shadow        bool   `json:"shadow,omitempty"`
}

// Summarize projects one full record to list-safe metadata.
func Summarize(record Record) Summary {
	return Summary{
		Ts: record.Ts, RequestID: record.RequestID, SessionID: record.SessionID,
		Protocol: record.Protocol, Method: record.Method, Path: record.Path,
		Exposed: record.Exposed, CalledModel: record.CalledModel,
		UpstreamModel: record.UpstreamModel, Provider: record.Provider,
		Attempt: record.Attempt, Status: record.Status, LatencyMs: record.LatencyMs,
		RequestSize: record.RequestSize, ResponseSize: record.ResponseSize,
		Shadow: record.Shadow,
	}
}

// QuerySummaries returns list-safe metadata. Bodies and response headers are
// discarded before top-K retention, so memory is bounded by metadata size.
func QuerySummaries(dir string, filter Filter) ([]Summary, error) {
	records, err := query(dir, filter, true)
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(records))
	for _, record := range records {
		summaries = append(summaries, Summarize(record))
	}
	return summaries, nil
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
	records, err := query(dir, filter, true)
	if err != nil {
		return nil, err
	}
	type pair struct {
		primary *Record
		shadow  *Record
	}
	pairs := map[string]*pair{}
	for i := range records {
		record := &records[i]
		id := record.RequestID
		if record.Shadow {
			id = trimShadowPrefix(id)
		}
		current := pairs[id]
		if current == nil {
			current = &pair{}
			pairs[id] = current
		}
		if record.Shadow {
			current.shadow = record
		} else {
			current.primary = record
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
	return result, nil
}

func trimShadowPrefix(id string) string {
	const prefix = "shadow-"
	if len(id) >= len(prefix) && id[:len(prefix)] == prefix {
		return id[len(prefix):]
	}
	return id
}
