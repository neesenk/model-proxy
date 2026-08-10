package appapi

import "time"

// --- /api/status decoded shapes ---
// Only fields a renderer actually reads are decoded. The --json path bypasses
// these structs entirely (raw json.RawMessage), and JSON decode ignores any
// extra upstream fields, so unused fields are omitted rather than maintained.

type StatusHealth struct {
	CircuitState     string `json:"circuit_state"` // closed | open | half_open
	Available        bool   `json:"available"`
	CircuitUntil     string `json:"circuit_until,omitempty"`      // RFC3339, only when in the future
	RateLimitedUntil string `json:"rate_limited_until,omitempty"` // RFC3339, only when in the future
	RateLimitKind    string `json:"rate_limit_kind,omitempty"`    // transient | quota | daily (with rate_limited_until)
}

type StatusCounters struct {
	Requests      uint64 `json:"requests"`
	Failovers     uint64 `json:"failovers"`
	RateLimited   uint64 `json:"rate_limited_429"`
	Failures      uint64 `json:"failures"`
	LastRequestAt int64  `json:"last_request_at"` // unix seconds
	LatencySum    uint64 `json:"latency_ms_sum"`
	TTFTSum       uint64 `json:"ttft_ms_sum"`
}

// Quota windows come from *provider.QuotaSnapshot: PascalCase, no json tags upstream.
type StatusWindow struct {
	Label        string    `json:"Label"`
	RemainingPct float64   `json:"RemainingPct"` // 0..1, -1 if unknown
	ResetsAt     time.Time `json:"ResetsAt"`
	Ultimate     bool      `json:"Ultimate"`
	Short        bool      `json:"Short"`
}

type StatusQuota struct {
	Account      string         `json:"Account"`
	Plan         string         `json:"Plan"`
	RemainingPct float64        `json:"RemainingPct"` // ultimate window remaining, 0..1; -1 if unknown
	Windows      []StatusWindow `json:"Windows"`
	Err          string         `json:"Err"`
}

type StatusOrdered struct {
	Provider  string  `json:"provider"`
	Priority  int     `json:"priority"`
	Tier      string  `json:"tier"`
	Surplus   float64 `json:"surplus"`
	Available bool    `json:"available"`
	Peak      bool    `json:"peak"`
}

type StatusPool struct {
	Parent    string `json:"parent"`
	Accounts  int    `json:"accounts"`
	Available int    `json:"available"`
}

type StatusRoute struct {
	First      string          `json:"first"`
	Ordered    []StatusOrdered `json:"ordered"`
	Sticky     string          `json:"sticky"`
	DwellRem   float64         `json:"sticky_dwell_remaining_sec"`
	Pools      []StatusPool    `json:"pools"`
	Pin        string          `json:"pin"`
	PinExpires string          `json:"pin_expires"`
}

// StatusModelLock is one model-level lock entry from /api/status model_locks.
type StatusModelLock struct {
	Model string `json:"model"`
	Until string `json:"until"`
}

type StatusSchedule struct {
	Models map[string]StatusRoute `json:"models"`
}

type StatusResp struct {
	Uptime     string                       `json:"uptime"`
	Version    string                       `json:"version"`
	Listen     string                       `json:"listen"`
	Health     map[string]StatusHealth      `json:"health"`
	ModelLocks map[string][]StatusModelLock `json:"model_locks"`
	Quota      map[string]StatusQuota       `json:"quota"`
	Schedule   StatusSchedule               `json:"schedule"`
	Counters   map[string]StatusCounters    `json:"counters"`
	Warnings   []string                     `json:"warnings"`
}

// --- /api/tokens decoded shape ---

type TokenEntry struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

type TokensResp struct {
	Usage []TokenEntry `json:"usage"`
}

// --- /api/logs decoded shape ---

type LogsResp struct {
	Lines []string `json:"lines"`
}
