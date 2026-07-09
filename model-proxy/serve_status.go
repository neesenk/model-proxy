package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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

// renderScheduleRoutes renders the per-route provider chains. ind is the indent
// for each route's name line; detail lines use ind + 4 spaces. Shared by the
// `schedule` command (ind "") and the serve-status Schedule section (ind "  "),
// so the two views never drift. Output ends with a trailing blank line, matching
// the original `schedule` command.
func renderScheduleRoutes(models map[string]statusRoute, ind string) string {
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, m := range names {
		ri := models[m]
		fmt.Fprintf(&b, "%s%s → %s\n", ind, cBold(m), cGreen(ri.First))
		for _, pool := range ri.Pools {
			fmt.Fprintf(&b, "%s    %s %s (%d accounts, %d available)\n",
				ind, cDim("pool:"), cBold(pool.Parent), pool.Accounts, pool.Available)
		}
		for _, t := range ri.Ordered {
			extra := ""
			if !t.Available {
				extra += " " + cRed("(unavailable)")
			}
			if t.Peak {
				extra += " " + cYellow("peak")
			}
			fmt.Fprintf(&b, "%s    %s %s  surplus %+.2f  p%d%s\n",
				ind, pad(t.Provider, 14), cGray(pad(t.Tier, 13)), t.Surplus, t.Priority, extra)
		}
		if ri.Sticky != "" {
			dwell := ""
			if ri.DwellRem > 0 {
				dwell = fmt.Sprintf(", %.0fs dwell left", ri.DwellRem)
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, cDim("sticky: "), ri.Sticky, cDim(dwell))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderSchedule renders the serve-status Schedule section: header + the shared
// per-route renderer at 2-space indent. Trailing blank line trimmed so the
// section ends with a single newline (appendSection adds the separator).
func renderSchedule(st *statusResp) string {
	if len(st.Schedule.Models) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s)\n", cBold("Schedule"), len(st.Schedule.Models), plural(len(st.Schedule.Models), "route", "routes"))
	b.WriteString(renderScheduleRoutes(st.Schedule.Models, "  "))
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderTokens renders the per provider/model token-usage table, sorted by
// provider then model, with totals in the header.
func renderTokens(t *tokensResp) string {
	if len(t.Usage) == 0 {
		return ""
	}
	sort.Slice(t.Usage, func(i, j int) bool {
		if t.Usage[i].Provider != t.Usage[j].Provider {
			return t.Usage[i].Provider < t.Usage[j].Provider
		}
		return t.Usage[i].Model < t.Usage[j].Model
	})
	var totalReqs uint64
	for _, e := range t.Usage {
		totalReqs += e.Requests
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s · %s requests)\n",
		cBold("Tokens"), len(t.Usage), plural(len(t.Usage), "model", "models"), compactNum(totalReqs))
	hdr := fmt.Sprintf("  %-14s %-22s %10s %10s %10s %10s %10s",
		"PROVIDER", "MODEL", "INPUT", "OUTPUT", "CACHE-CR", "CACHE-RD", "REQUESTS")
	fmt.Fprintln(&b, cDim(hdr))
	for _, e := range t.Usage {
		fmt.Fprintf(&b, "  %-14s %-22s %10s %10s %10s %10s %10s\n",
			e.Provider, e.Model,
			compactNum(e.Input), compactNum(e.Output),
			compactNum(e.CacheCreation), compactNum(e.CacheRead),
			compactNum(e.Requests))
	}
	return b.String()
}

// renderLogs renders the recent log lines (only with --logs).
func renderLogs(l *logsResp) string {
	if len(l.Lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (last %d)\n", cBold("Logs"), len(l.Lines))
	for _, line := range l.Lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

// renderQuota renders per-provider quota windows as label + tag + % + bar + reset.
func renderQuota(st *statusResp) string {
	names := make([]string, 0, len(st.Quota))
	for n := range st.Quota {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d)\n", cBold("Quota"), len(names))
	for _, name := range names {
		q := st.Quota[name]
		header := name
		if q.Account != "" {
			header += " · " + q.Account
		}
		if q.Plan != "" {
			header += " · " + q.Plan
		}
		fmt.Fprintf(&b, "  %s\n", cBold(header))
		if q.Err != "" {
			fmt.Fprintf(&b, "      %s\n", cDim("no data ("+q.Err+")"))
			continue
		}
		for _, w := range q.Windows {
			tag := "short"
			if w.Ultimate {
				tag = "ultimate"
			}
			pctStr := "—"
			usedPct := 0
			if w.RemainingPct >= 0 {
				pctStr = fmt.Sprintf("%.0f%%", w.RemainingPct*100)
				usedPct = int((1 - w.RemainingPct) * 100)
			}
			resets := ""
			if !w.ResetsAt.IsZero() {
				resets = cDim("  resets " + formatClockTime(w.ResetsAt))
			}
			fmt.Fprintf(&b, "      %s  %5s  %s%s\n",
				pad(w.Label+" ("+tag+")", 22), pctStr, progressBar(usedPct, 16), resets)
		}
	}
	return b.String()
}

// statusGet fetches base+path and returns the body, HTTP status, and transport
// error (if any). A non-2xx status is NOT an error here — the caller inspects it.
func statusGet(base, path string) (body []byte, status int, err error) {
	resp, err := http.Get(base + path)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ = io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

// renderStatus fetches the daemon's status endpoints and returns either the
// rendered terminal view or the merged JSON (opts.JSON). listen is the daemon's
// "host:port" (cfg.Listen); the http:// scheme is added here. Fetch plan:
// /api/status always; /api/tokens always; /api/logs?tail=N only when opts.Logs.
func renderStatus(listen string, opts statusOpts) (string, error) {
	base := "http://" + listen
	statusBody, status, err := statusGet(base, "/api/status")
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available — is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, truncate(string(statusBody), 200))
	}

	tokensBody, _, _ := statusGet(base, "/api/tokens") // non-fatal; absence just hides the section

	if opts.JSON {
		merged := map[string]json.RawMessage{"status": json.RawMessage(statusBody)}
		if len(tokensBody) > 0 {
			merged["tokens"] = json.RawMessage(tokensBody)
		}
		if opts.Logs {
			if lb, _, e := statusGet(base, "/api/logs?tail="+strconv.Itoa(opts.LogsN)); e == nil {
				merged["logs"] = json.RawMessage(lb)
			}
		}
		enc, _ := json.MarshalIndent(merged, "", "  ")
		return string(enc), nil
	}

	var st statusResp
	if err := json.Unmarshal(statusBody, &st); err != nil {
		return "", fmt.Errorf("parse status response: %v", err)
	}
	var tok tokensResp
	if len(tokensBody) > 0 {
		json.Unmarshal(tokensBody, &tok)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s · %s · %s\n\n",
		cBold("model-proxy"), cDim("v"+st.Version), cDim(st.Uptime), cDim(st.Listen))
	appendSection(&b, renderProviders(&st))
	appendSection(&b, renderSchedule(&st))
	appendSection(&b, renderQuota(&st))
	appendSection(&b, renderTokens(&tok))
	if opts.Logs {
		var lg logsResp
		if lb, ls, e := statusGet(base, "/api/logs?tail="+strconv.Itoa(opts.LogsN)); e == nil && ls == 200 {
			json.Unmarshal(lb, &lg)
		}
		appendSection(&b, renderLogs(&lg))
	}
	return b.String(), nil
}

// appendSection writes a non-empty section followed by one blank separator line.
func appendSection(b *strings.Builder, s string) {
	if strings.TrimSpace(s) == "" {
		return
	}
	b.WriteString(s)
	if !strings.HasSuffix(s, "\n") {
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
}

// cmdServeStatus prints a terminal-optimized snapshot of the running daemon's
// state — the same data the Web UI's Status tab shows: providers health +
// counters, schedule, quota, tokens, and optionally recent logs. One-shot.
func cmdServeStatus(args []string) {
	opts := parseStatusFlags(args)
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	out, err := renderStatus(cfg.Listen, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", cRed("✗"), err.Error())
		os.Exit(1)
	}
	fmt.Print(out)
}

// parseStatusFlags scans serve-status args for --json and --logs [N] (default
// N=20). --config is intentionally ignored here — configPath handles it.
func parseStatusFlags(args []string) statusOpts {
	o := statusOpts{LogsN: 20}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--json":
			o.JSON = true
		case a == "--logs":
			o.Logs = true
			if i+1 < len(args) {
				if n, err := strconv.Atoi(args[i+1]); err == nil && n > 0 {
					o.LogsN = n
					i++
				}
			}
		}
	}
	return o
}
