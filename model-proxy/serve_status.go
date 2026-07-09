package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// statusOpts selects what serve status fetches/renders.
type statusOpts struct {
	Logs  bool // include the Logs section (--logs)
	LogsN int  // number of log lines (default 20)
	JSON  bool // dump merged raw JSON (--json)
}

// --- /api/status decoded shapes ---

type statusHealth struct {
	CircuitState     string `json:"circuit_state"` // closed | open | half_open
	Available        bool   `json:"available"`
	CircuitUntil     string `json:"circuit_until,omitempty"`      // RFC3339, only when in the future
	RateLimitedUntil string `json:"rate_limited_until,omitempty"` // RFC3339, only when in the future
}

type statusCounters struct {
	Requests      uint64 `json:"requests"`
	Failovers     uint64 `json:"failovers"`
	RateLimited   uint64 `json:"rate_limited_429"`
	Failures      uint64 `json:"failures"`
	LastRequestAt int64  `json:"last_request_at"` // unix seconds
}

// Quota windows come from *provider.QuotaSnapshot: PascalCase, no json tags upstream.
type statusWindow struct {
	Label        string    `json:"Label"`
	Kind         string    `json:"Kind"`
	Used         float64   `json:"Used"`
	Total        float64   `json:"Total"`
	RemainingPct float64   `json:"RemainingPct"` // 0..1, -1 if unknown
	ResetsAt     time.Time `json:"ResetsAt"`
	Ultimate     bool      `json:"Ultimate"`
	Short        bool      `json:"Short"`
}

type statusQuota struct {
	Billing      int            `json:"Billing"`
	RemainingPct float64        `json:"RemainingPct"`
	Account      string         `json:"Account"`
	Plan         string         `json:"Plan"`
	Level        string         `json:"Level"`
	Windows      []statusWindow `json:"Windows"`
	Notes        []string       `json:"Notes"`
	Err          string         `json:"Err"`
}

type statusOrdered struct {
	Provider   string  `json:"provider"`
	PoolParent string  `json:"pool_parent"`
	Priority   int     `json:"priority"`
	Tier       string  `json:"tier"`
	Surplus    float64 `json:"surplus"`
	Available  bool    `json:"available"`
	Peak       bool    `json:"peak"`
}

type statusPool struct {
	Parent    string `json:"parent"`
	Accounts  int    `json:"accounts"`
	Available int    `json:"available"`
}

type statusRoute struct {
	First    string          `json:"first"`
	Ordered  []statusOrdered `json:"ordered"`
	Sticky   string          `json:"sticky"`
	DwellRem float64         `json:"sticky_dwell_remaining_sec"`
	Pools    []statusPool    `json:"pools"`
}

type statusSchedule struct {
	Models map[string]statusRoute `json:"models"`
}

type statusResp struct {
	Uptime   string                    `json:"uptime"`
	Version  string                    `json:"version"`
	Listen   string                    `json:"listen"`
	Health   map[string]statusHealth   `json:"health"`
	Quota    map[string]statusQuota    `json:"quota"`
	Schedule statusSchedule            `json:"schedule"`
	Counters map[string]statusCounters `json:"counters"`
}

// --- /api/tokens decoded shape ---

type tokenEntry struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

type tokensResp struct {
	Usage []tokenEntry `json:"usage"`
}

// --- /api/logs decoded shape ---

type logsResp struct {
	Lines []string `json:"lines"`
}

// compactNum renders a count compactly: 0, 5, 567, 1k, 1.2k, 450k, 1.2M, 5.6B.
func compactNum(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return trimNumZero(fmt.Sprintf("%.1f", float64(n)/1e9)) + "B"
	case n >= 1_000_000:
		return trimNumZero(fmt.Sprintf("%.1f", float64(n)/1e6)) + "M"
	case n >= 1_000:
		return trimNumZero(fmt.Sprintf("%.1f", float64(n)/1e3)) + "k"
	}
	return strconv.FormatUint(n, 10)
}

// trimNumZero strips a trailing ".0" from a "%.1f" number string.
func trimNumZero(s string) string {
	if i := strings.Index(s, "."); i >= 0 && strings.HasSuffix(s, "0") {
		return s[:i]
	}
	return s
}

// formatClock renders a unix-seconds timestamp as local HH:MM:SS, or "—" when ≤0.
func formatClock(unixSec int64) string {
	if unixSec <= 0 {
		return "—"
	}
	return time.Unix(unixSec, 0).Local().Format("15:04:05")
}

// formatClockTime renders a time as HH:MM if today, else MM-DD HH:MM.
func formatClockTime(t time.Time) string {
	t = t.Local()
	if t.Format("20060102") == time.Now().Format("20060102") {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

// plural returns sing for n==1 else plur.
func plural(n int, sing, plur string) string {
	if n == 1 {
		return sing
	}
	return plur
}

// renderProviders renders the Providers table: one row per health entry (sorted),
// with counters looked up by name. The API only emits circuit_until /
// rate_limited_until when they are in the future, so field presence ⇒ active.
func renderProviders(st *statusResp) string {
	names := make([]string, 0, len(st.Health))
	for n := range st.Health {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d)\n", cBold("Providers"), len(names))
	hdr := fmt.Sprintf("  %s  %s  %8s  %9s  %5s  %8s  %s",
		pad("PROVIDER", 16), pad("HEALTH", 13), "REQS", "FAILOVERS", "429", "FAILURES", "LAST")
	fmt.Fprintln(&b, cDim(hdr))
	for _, name := range names {
		label, color := healthLabel(st.Health[name])
		c := st.Counters[name]
		fmt.Fprintf(&b, "  %s  %s  %8s  %9s  %5s  %8s  %s\n",
			pad(name, 16),
			color(pad(label, 13)),
			compactNum(c.Requests),
			compactNum(c.Failovers),
			compactNum(c.RateLimited),
			compactNum(c.Failures),
			formatClock(c.LastRequestAt))
	}
	return b.String()
}

// healthLabel returns the visible label + color func for a provider's health cell.
func healthLabel(h statusHealth) (string, func(string) string) {
	switch {
	case h.CircuitState == "open":
		return "circuit open", cRed
	case h.CircuitState == "half_open":
		return "half-open", cRed
	case h.RateLimitedUntil != "":
		return "rate-limited", cYellow
	case h.Available:
		return "available", cGreen
	default:
		return "unavailable", cDim
	}
}
