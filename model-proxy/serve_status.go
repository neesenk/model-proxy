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
// Only fields a renderer actually reads are decoded. The --json path bypasses
// these structs entirely (raw json.RawMessage), and JSON decode ignores any
// extra upstream fields, so unused fields are omitted rather than maintained.

type statusHealth struct {
	CircuitState     string `json:"circuit_state"` // closed | open | half_open
	Available        bool   `json:"available"`
	RateLimitedUntil string `json:"rate_limited_until,omitempty"` // RFC3339, only when in the future
}

type statusCounters struct {
	Requests      uint64 `json:"requests"`
	Failovers     uint64 `json:"failovers"`
	RateLimited   uint64 `json:"rate_limited_429"`
	Failures      uint64 `json:"failures"`
	LastRequestAt int64  `json:"last_request_at"` // unix seconds
	LatencySum    uint64 `json:"latency_ms_sum"`
	TTFTSum       uint64 `json:"ttft_ms_sum"`
}

// Quota windows come from *provider.QuotaSnapshot: PascalCase, no json tags upstream.
type statusWindow struct {
	Label        string    `json:"Label"`
	RemainingPct float64   `json:"RemainingPct"` // 0..1, -1 if unknown
	ResetsAt     time.Time `json:"ResetsAt"`
	Ultimate     bool      `json:"Ultimate"`
	Short        bool      `json:"Short"`
}

type statusQuota struct {
	Account string         `json:"Account"`
	Plan    string         `json:"Plan"`
	Windows []statusWindow `json:"Windows"`
	Err     string         `json:"Err"`
}

type statusOrdered struct {
	Provider  string  `json:"provider"`
	Priority  int     `json:"priority"`
	Tier      string  `json:"tier"`
	Surplus   float64 `json:"surplus"`
	Available bool    `json:"available"`
	Peak      bool    `json:"peak"`
}

type statusPool struct {
	Parent    string `json:"parent"`
	Accounts  int    `json:"accounts"`
	Available int    `json:"available"`
}

type statusRoute struct {
	First      string          `json:"first"`
	Ordered    []statusOrdered `json:"ordered"`
	Sticky     string          `json:"sticky"`
	DwellRem   float64         `json:"sticky_dwell_remaining_sec"`
	Pools      []statusPool    `json:"pools"`
	Pin        string          `json:"pin"`
	PinExpires string          `json:"pin_expires"`
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
	Warnings []string                  `json:"warnings"`
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
// Rounding that would carry a value up to 1000 of its unit promotes to the next
// unit instead (e.g. 999999 → "1M", not "1000k").
func compactNum(n uint64) string {
	switch {
	case n >= 1_000_000_000:
		return scaleNum(float64(n)/1e9, "B", "")
	case n >= 1_000_000:
		return scaleNum(float64(n)/1e6, "M", "B")
	case n >= 1_000:
		return scaleNum(float64(n)/1e3, "k", "M")
	}
	return strconv.FormatUint(n, 10)
}

// scaleNum formats v (already divided into unit suf) to one decimal with a
// trailing ".0" stripped. If v rounds up to 1000 of this unit, promote to
// "1"+next instead ("1M" rather than "1000k"). next=="" at the top unit (B).
func scaleNum(v float64, suf, next string) string {
	r := int64(v*10 + 0.5) // rounded tenths
	if r >= 10000 {        // 1000.0 of this unit — carry to the next unit
		if next != "" {
			return "1" + next
		}
		return "1000" + suf // top unit (B): no larger unit to promote to
	}
	return trimNumZero(fmt.Sprintf("%.1f", float64(r)/10)) + suf
}

// trimNumZero strips a trailing ".0" from a "%.1f" number string.
func trimNumZero(s string) string {
	if i := strings.Index(s, "."); i >= 0 && strings.HasSuffix(s, "0") {
		return s[:i]
	}
	return s
}

// renderAvgMs returns the average latency/ttft in ms (sum/requests) as a display
// string, or "—" when no requests were served.
func renderAvgMs(sum, reqs uint64) string {
	if reqs == 0 {
		return "—"
	}
	return compactNum(sum / reqs)
}

// formatClock renders a unix-seconds timestamp as local HH:MM:SS, or "—" when ≤0.
func formatClock(unixSec int64) string {
	if unixSec <= 0 {
		return "—"
	}
	return time.Unix(unixSec, 0).Local().Format("15:04:05")
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
	hdr := fmt.Sprintf("  %s  %s  %8s  %9s  %5s  %8s  %6s  %6s  %s",
		pad("PROVIDER", 16), pad("HEALTH", 13), "REQS", "FAILOVERS", "429", "FAILURES", "LAT", "TTFT", "LAST")
	fmt.Fprintln(&b, cDim(hdr))
	for _, name := range names {
		label, color := healthLabel(st.Health[name])
		c := st.Counters[name]
		fmt.Fprintf(&b, "  %s  %s  %8s  %9s  %5s  %8s  %6s  %6s  %s\n",
			pad(name, 16),
			color(pad(label, 13)),
			compactNum(c.Requests),
			compactNum(c.Failovers),
			compactNum(c.RateLimited),
			compactNum(c.Failures),
			renderAvgMs(c.LatencySum, c.Requests),
			renderAvgMs(c.TTFTSum, c.Requests),
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
		if ri.Pin != "" {
			exp := ""
			if ri.PinExpires != "" {
				exp = cDim(" (" + ri.PinExpires + ")")
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, cYellow("pinned: "), ri.Pin, exp)
		}
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
// per-route renderer at 2-space indent. (Trailing-newline normalization is
// handled once by appendSection.)
func renderSchedule(st *statusResp) string {
	if len(st.Schedule.Models) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s)\n", cBold("Schedule"), len(st.Schedule.Models), plural(len(st.Schedule.Models), "route", "routes"))
	b.WriteString(renderScheduleRoutes(st.Schedule.Models, "  "))
	return b.String()
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

// renderQuota renders per-provider quota windows as label (+ ultimate/short tag)
// + remaining % + bar + reset time. A window that is neither Ultimate nor Short
// (e.g. volcengine daily/weekly, codex primary/weekly, zhipu TIME_LIMIT) gets no
// tag rather than being mislabeled "(short)".
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
			label := w.Label
			switch {
			case w.Ultimate:
				label += " (ultimate)"
			case w.Short:
				label += " (short)"
			}
			pctStr := "—"
			usedPct := 0
			if w.RemainingPct >= 0 {
				pct := int(w.RemainingPct*100 + 0.5) // round to nearest %; text and bar share it
				pctStr = fmt.Sprintf("%d%%", pct)
				usedPct = 100 - pct
			}
			resets := ""
			if !w.ResetsAt.IsZero() {
				resets = cDim("  resets " + formatResetAt(w.ResetsAt.UnixMilli()))
			}
			fmt.Fprintf(&b, "      %s  %5s  %s%s\n",
				pad(label, 22), pctStr, progressBar(usedPct, 16), resets)
		}
	}
	return b.String()
}

// daemonHTTPClient caps each request to the running daemon (used by serve status
// and schedule) so a wedged listener fails fast instead of hanging the command.
var daemonHTTPClient = &http.Client{Timeout: 10 * time.Second}

// statusGet fetches base+path and returns the body, HTTP status, and transport
// error (if any). A non-2xx status is NOT an error here — the caller inspects it.
func statusGet(base, path string) (body []byte, status int, err error) {
	resp, err := daemonHTTPClient.Get(base + path)
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
// /api/status always; /api/tokens always; /api/logs?tail=N once, only when
// opts.Logs (shared by the JSON and render paths; a non-200 is "no logs", not
// embedded as data).
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

	var logsBody []byte
	logsOK := false
	if opts.Logs {
		lb, ls, e := statusGet(base, "/api/logs?tail="+strconv.Itoa(opts.LogsN))
		logsBody, logsOK = lb, (e == nil && ls == 200)
	}

	if opts.JSON {
		merged := map[string]json.RawMessage{"status": json.RawMessage(statusBody)}
		if len(tokensBody) > 0 {
			merged["tokens"] = json.RawMessage(tokensBody)
		}
		if logsOK {
			merged["logs"] = json.RawMessage(logsBody)
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
	if len(st.Warnings) > 0 {
		appendSection(&b, renderWarnings(&st))
	}
	appendSection(&b, renderTokens(&tok))
	if logsOK {
		var lg logsResp
		json.Unmarshal(logsBody, &lg)
		appendSection(&b, renderLogs(&lg))
	}
	return b.String(), nil
}

// appendSection writes a non-empty section followed by one blank separator line.
// Trailing newlines are normalized away so each renderer need not worry about
// its exact trailing whitespace.
func appendSection(b *strings.Builder, s string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	b.WriteString(s)
	b.WriteString("\n\n")
}

// renderWarnings renders the implicit-route ambiguity warnings (a model served
// by >1 logged-in provider with no explicit route). Mirrors the `models` CLI.
func renderWarnings(st *statusResp) string {
	if len(st.Warnings) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  implicit-route warnings\n", cYellow("⚠"))
	for _, w := range st.Warnings {
		fmt.Fprintf(&b, "  %s\n", w)
	}
	return b.String()
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
// N=20). Both "--logs 50" and "--logs=50" are accepted. --config is intentionally
// ignored here — configPath handles it.
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
		case strings.HasPrefix(a, "--logs="):
			o.Logs = true
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--logs=")); err == nil && n > 0 {
				o.LogsN = n
			}
		}
	}
	return o
}
