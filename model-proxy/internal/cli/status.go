package cli

import (
	"model-proxy/internal/appapi"
	"encoding/json"
	"fmt"
	"io"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/daemonctl"
	"model-proxy/provider"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StatusOpts selects what serve status fetches/renders.
type StatusOpts struct {
	Logs  bool // include the Logs section (--logs)
	LogsN int  // number of log lines (default 20)
	JSON  bool // dump merged raw JSON (--json)
}

// Status DTOs live in internal/appapi (shared with the Web/app layers); the CLI
// aliases keep the renderers readable.
type StatusHealth = appapi.StatusHealth
type StatusCounters = appapi.StatusCounters
type StatusWindow = appapi.StatusWindow
type StatusQuota = appapi.StatusQuota
type StatusOrdered = appapi.StatusOrdered
type StatusPool = appapi.StatusPool
type StatusRoute = appapi.StatusRoute
type StatusModelLock = appapi.StatusModelLock
type StatusSchedule = appapi.StatusSchedule
type StatusResp = appapi.StatusResp
type TokenEntry = appapi.TokenEntry
type TokensResp = appapi.TokensResp
type LogsResp = appapi.LogsResp

// compactNum delegates to the CLI formatting package (single owner for
// terminal number rendering shared by stats/status/shadow-report).
func compactNum(n uint64) string { return CompactNum(n) }

// renderAvgMs returns the average latency/ttft in ms (sum/requests) as a display
// string, or "—" when no requests were served.
func RenderAvgMs(sum, reqs uint64) string {
	if reqs == 0 {
		return "—"
	}
	return compactNum(sum / reqs)
}

// formatClock renders a unix-seconds timestamp as local HH:MM:SS, or "—" when ≤0.
func FormatClock(unixSec int64) string {
	if unixSec <= 0 {
		return "—"
	}
	return time.Unix(unixSec, 0).Local().Format("15:04:05")
}

// renderProviders renders the Providers table: one row per health entry (sorted),
// with counters looked up by name. The API only emits circuit_until /
// rate_limited_until when they are in the future, so field presence ⇒ active.
func RenderProviders(st *StatusResp) string {
	names := make([]string, 0, len(st.Health))
	for n := range st.Health {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d)\n", provider.Bold("Providers"), len(names))
	hdr := fmt.Sprintf("  %s  %s  %8s  %9s  %5s  %8s  %6s  %6s  %s",
		provider.Pad("PROVIDER", 16), provider.Pad("HEALTH", 13), "REQS", "FAILOVERS", "429", "FAILURES", "LAT", "TTFT", "LAST")
	fmt.Fprintln(&b, provider.Dim(hdr))
	for _, name := range names {
		label, color := HealthLabel(st.Health[name])
		c := st.Counters[name]
		fmt.Fprintf(&b, "  %s  %s  %8s  %9s  %5s  %8s  %6s  %6s  %s\n",
			provider.Pad(name, 16),
			color(provider.Pad(label, 13)),
			compactNum(c.Requests),
			compactNum(c.Failovers),
			compactNum(c.RateLimited),
			compactNum(c.Failures),
			RenderAvgMs(c.LatencySum, c.Requests),
			RenderAvgMs(c.TTFTSum, c.Requests),
			FormatClock(c.LastRequestAt))
	}
	return b.String()
}

// healthLabel returns the visible label + color func for a provider's health cell.
func HealthLabel(h StatusHealth) (string, func(string) string) {
	switch {
	case h.CircuitState == "open":
		return "circuit open", provider.Red
	case h.CircuitState == "half_open":
		return "half-open", provider.Red
	case h.RateLimitedUntil != "" && h.RateLimitKind == "quota":
		return "rl:quota", provider.Yellow
	case h.RateLimitedUntil != "" && h.RateLimitKind == "daily":
		return "rl:daily", provider.Yellow
	case h.RateLimitedUntil != "":
		return "rate-limited", provider.Yellow
	case h.Available:
		return "available", provider.Green
	default:
		return "unavailable", provider.Dim
	}
}

// renderScheduleRoutes renders the per-route provider chains. ind is the indent
// for each route's name line; detail lines use ind + 4 spaces. Shared by the
// `schedule` command (ind "") and the serve-status Schedule section (ind "  "),
// so the two views never drift. Output ends with a trailing blank line, matching
// the original `schedule` command.
func RenderScheduleRoutes(models map[string]StatusRoute, ind string) string {
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, m := range names {
		ri := models[m]
		fmt.Fprintf(&b, "%s%s → %s\n", ind, provider.Bold(m), provider.Green(ri.First))
		if ri.Pin != "" {
			exp := ""
			if ri.PinExpires != "" {
				exp = provider.Dim(" (" + ri.PinExpires + ")")
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, provider.Yellow("pinned: "), ri.Pin, exp)
		}
		for _, pool := range ri.Pools {
			fmt.Fprintf(&b, "%s    %s %s (%d accounts, %d available)\n",
				ind, provider.Dim("pool:"), provider.Bold(pool.Parent), pool.Accounts, pool.Available)
		}
		for _, t := range ri.Ordered {
			extra := ""
			if !t.Available {
				extra += " " + provider.Red("(unavailable)")
			}
			if t.Peak {
				extra += " " + provider.Yellow("peak")
			}
			fmt.Fprintf(&b, "%s    %s %s  surplus %+.2f  p%d%s\n",
				ind, provider.Pad(t.Provider, 14), provider.Gray(provider.Pad(t.Tier, 13)), t.Surplus, t.Priority, extra)
		}
		if ri.Sticky != "" {
			dwell := ""
			if ri.DwellRem > 0 {
				dwell = fmt.Sprintf(", %.0fs dwell left", ri.DwellRem)
			}
			fmt.Fprintf(&b, "%s    %s%s%s\n", ind, provider.Dim("sticky: "), ri.Sticky, provider.Dim(dwell))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// renderSchedule renders the serve-status Schedule section: header + the shared
// per-route renderer at 2-space indent. (Trailing-newline normalization is
// handled once by appendSection.)
func RenderSchedule(st *StatusResp) string {
	if len(st.Schedule.Models) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s)\n", provider.Bold("Schedule"), len(st.Schedule.Models), cliframework.Plural(len(st.Schedule.Models), "route", "routes"))
	b.WriteString(RenderScheduleRoutes(st.Schedule.Models, "  "))
	return b.String()
}

// renderTokens renders the per provider/model token-usage table, sorted by
// provider then model, with totals in the header.
func RenderTokens(t *TokensResp) string {
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
		provider.Bold("Tokens"), len(t.Usage), cliframework.Plural(len(t.Usage), "model", "models"), compactNum(totalReqs))
	hdr := fmt.Sprintf("  %-14s %-22s %10s %10s %10s %10s %10s",
		"PROVIDER", "MODEL", "INPUT", "OUTPUT", "CACHE-CR", "CACHE-RD", "REQUESTS")
	fmt.Fprintln(&b, provider.Dim(hdr))
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
func RenderLogs(l *LogsResp) string {
	if len(l.Lines) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (last %d)\n", provider.Bold("Logs"), len(l.Lines))
	for _, line := range l.Lines {
		fmt.Fprintf(&b, "  %s\n", line)
	}
	return b.String()
}

// renderQuota renders per-provider quota windows as label (+ ultimate/short tag)
// + remaining % + bar + reset time. A window that is neither Ultimate nor Short
// (e.g. volcengine daily/weekly, codex primary/weekly, zhipu TIME_LIMIT) gets no
// tag rather than being mislabeled "(short)".
func RenderQuota(st *StatusResp) string {
	names := make([]string, 0, len(st.Quota))
	for n := range st.Quota {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d)\n", provider.Bold("Quota"), len(names))
	for _, name := range names {
		q := st.Quota[name]
		header := name
		if q.Account != "" {
			header += " · " + q.Account
		}
		if q.Plan != "" {
			header += " · " + q.Plan
		}
		fmt.Fprintf(&b, "  %s\n", provider.Bold(header))
		if q.Err != "" {
			fmt.Fprintf(&b, "      %s\n", provider.Dim("no data ("+q.Err+")"))
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
				resets = provider.Dim("  resets " + formatResetAt(w.ResetsAt.UnixMilli()))
			}
			fmt.Fprintf(&b, "      %s  %5s  %s%s\n",
				provider.Pad(label, 22), pctStr, provider.ProgressBar(usedPct, 16), resets)
		}
	}
	return b.String()
}

// daemonHTTPClient caps each request to the running daemon (used by serve status
// and schedule) so a wedged listener fails fast instead of hanging the command.
var daemonHTTPClient = daemonctl.Client

// statusGet fetches base+path and returns the body, HTTP status, and transport
// error (if any). A non-2xx status is NOT an error here — the caller inspects it.
func StatusGet(base, path string) (body []byte, status int, err error) {
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
func RenderStatus(listen string, opts StatusOpts) (string, error) {
	base := "http://" + listen
	statusBody, status, err := StatusGet(base, "/api/status")
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available — is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, provider.Truncate(string(statusBody), 200))
	}

	tokensBody, _, _ := StatusGet(base, "/api/tokens") // non-fatal; absence just hides the section

	var logsBody []byte
	logsOK := false
	if opts.Logs {
		lb, ls, e := StatusGet(base, "/api/logs?tail="+strconv.Itoa(opts.LogsN))
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

	var st StatusResp
	if err := json.Unmarshal(statusBody, &st); err != nil {
		return "", fmt.Errorf("parse status response: %v", err)
	}
	var tok TokensResp
	if len(tokensBody) > 0 {
		json.Unmarshal(tokensBody, &tok)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s · %s · %s\n\n",
		provider.Bold("model-proxy"), provider.Dim("v"+st.Version), provider.Dim(st.Uptime), provider.Dim(st.Listen))
	AppendSection(&b, RenderProviders(&st))
	AppendSection(&b, RenderSchedule(&st))
	AppendSection(&b, RenderQuota(&st))
	if len(st.Warnings) > 0 {
		AppendSection(&b, RenderWarnings(&st))
	}
	AppendSection(&b, RenderTokens(&tok))
	if logsOK {
		var lg LogsResp
		json.Unmarshal(logsBody, &lg)
		AppendSection(&b, RenderLogs(&lg))
	}
	return b.String(), nil
}

// appendSection writes a non-empty section followed by one blank separator line.
// Trailing newlines are normalized away so each renderer need not worry about
// its exact trailing whitespace.
func AppendSection(b *strings.Builder, s string) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return
	}
	b.WriteString(s)
	b.WriteString("\n\n")
}

// renderWarnings renders the implicit-route ambiguity warnings (a model served
// by >1 logged-in provider with no explicit route). Mirrors the `models` CLI.
func RenderWarnings(st *StatusResp) string {
	if len(st.Warnings) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s  implicit-route warnings\n", provider.Yellow("⚠"))
	for _, w := range st.Warnings {
		fmt.Fprintf(&b, "  %s\n", w)
	}
	return b.String()
}

// cmdServeStatus prints a terminal-optimized snapshot of the running daemon's
// state — the same data the Web UI's Status tab shows: providers health +
// counters, schedule, quota, tokens, and optionally recent logs. One-shot.
func CmdServeStatus(args []string, cfg *configdomain.Config) {
	opts := ParseStatusFlags(args)
	out, err := RenderStatus(cfg.Listen, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s\n", provider.Red("✗"), err.Error())
		os.Exit(1)
	}
	fmt.Print(out)
}

// parseStatusFlags scans serve-status args for --json and --logs [N] (default
// N=20). Both "--logs 50" and "--logs=50" are accepted. --config is intentionally
// ignored here — configPath handles it.
func ParseStatusFlags(args []string) StatusOpts {
	o := StatusOpts{LogsN: 20}
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
