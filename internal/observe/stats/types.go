package stats

import "time"

// Options controls a Store.
type Options struct {
	Path      string
	Retention time.Duration
}

// Key identifies a provider/model counter stream.
type Key struct {
	Provider string
	Model    string
}

// Counters is the complete persisted counter set for a provider/model stream.
// Every field except LastRequestAt is additive. LastRequestAt is merged with
// MAX when multiple flushes target the same minute.
type Counters struct {
	Requests       uint64
	Failovers      uint64
	RateLimited429 uint64
	Failures       uint64
	Input          uint64
	Output         uint64
	CacheCreation  uint64
	CacheRead      uint64
	TokenRequests  uint64
	LastRequestAt  int64
	LatencySum     uint64
	TTFTSum        uint64
	DurationSum    uint64
}

// AgentKey identifies an agent/provider/model counter stream.
type AgentKey struct {
	Agent    string
	Provider string
	Model    string
}

// AgentCounters is the persisted counter set for an agent stream.
type AgentCounters struct {
	Requests      uint64
	Input         uint64
	Output        uint64
	CacheCreation uint64
	CacheRead     uint64
	LatencySum    uint64
	TTFTSum       uint64
	DurationSum   uint64
	Failures      uint64
}

// Bucket is one provider/model time bucket returned by QueryRange.
type Bucket struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Minute         int64   `json:"minute"`
	Requests       uint64  `json:"requests"`
	Failovers      uint64  `json:"failovers"`
	RateLimited429 uint64  `json:"rate_limited_429"`
	Failures       uint64  `json:"failures"`
	Input          uint64  `json:"input"`
	Output         uint64  `json:"output"`
	CacheCreation  uint64  `json:"cache_creation"`
	CacheRead      uint64  `json:"cache_read"`
	TokenRequests  uint64  `json:"token_requests"`
	LastRequestAt  int64   `json:"last_request_at"`
	LatencySum     uint64  `json:"latency_ms_sum"`
	TTFTSum        uint64  `json:"ttft_ms_sum"`
	DurationSum    uint64  `json:"duration_ms_sum"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	AvgTtftMs      float64 `json:"avg_ttft_ms"`
}

// AnalyticsBucket is one local calendar-hour/day/month aggregate. The
// provider/model dimension reads minute_buckets; the agent dimension
// (AnalyticsBucket.Agent set) reads agent_buckets, which has no
// failover/429 columns — those stay zero there.
type AnalyticsBucket struct {
	Provider       string  `json:"provider"`
	Model          string  `json:"model"`
	Agent          string  `json:"agent,omitempty"`
	Bucket         int64   `json:"bucket"`
	Requests       uint64  `json:"requests"`
	Failovers      uint64  `json:"failovers"`
	RateLimited429 uint64  `json:"rate_limited_429"`
	Failures       uint64  `json:"failures"`
	Input          uint64  `json:"input"`
	Output         uint64  `json:"output"`
	CacheCreation  uint64  `json:"cache_creation"`
	CacheRead      uint64  `json:"cache_read"`
	LatencySum     uint64  `json:"latency_ms_sum"`
	TTFTSum        uint64  `json:"ttft_ms_sum"`
	DurationSum    uint64  `json:"duration_ms_sum"`
	LastRequestAt  int64   `json:"last_request_at"`
	AvgLatencyMs   float64 `json:"avg_latency_ms"`
	AvgTtftMs      float64 `json:"avg_ttft_ms"`
	AvgDurationMs  float64 `json:"avg_duration_ms"`
}

// AgentBucket is one agent/provider/model time bucket returned by QueryAgents.
type AgentBucket struct {
	Agent         string `json:"agent"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Minute        int64  `json:"minute"`
	Requests      uint64 `json:"requests"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	LatencySum    uint64 `json:"latency_ms_sum"`
	TTFTSum       uint64 `json:"ttft_ms_sum"`
	DurationSum   uint64 `json:"duration_ms_sum"`
	Failures      uint64 `json:"failures"`
}
