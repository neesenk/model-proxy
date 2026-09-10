// Package stats owns the `stats` and `usage` commands: daemon statistics
// report reads (requests/analytics/agents + summaries) and per-provider
// quota/credit display.
package stats

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/display"
	"net/url"
	"sort"
	"strings"
	"time"

	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/internal/daemonctl"
	observestats "model-proxy/internal/observe/stats"
)

// StatsOpts holds parsed `stats` command flags.
type StatsOpts struct {
	From        string
	To          string
	Provider    string
	Model       string
	Bucket      string
	Granularity string
	Cost        bool
	ByAgent     bool
	Agent       string
	JSON        bool
}

// parseStatsFlags scans `stats` flags: --from/--to (unix sec or RFC3339),
// --provider/--model filters, --bucket (display granularity), --json. --config
// is left to configPath.
func ParseStatsFlags(args []string) StatsOpts {
	o := StatsOpts{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--from", a == "--to", a == "--provider", a == "--model", a == "--bucket", a == "--granularity":
			if i+1 < len(args) {
				switch a {
				case "--from":
					o.From = args[i+1]
				case "--to":
					o.To = args[i+1]
				case "--provider":
					o.Provider = args[i+1]
				case "--model":
					o.Model = args[i+1]
				case "--bucket":
					o.Bucket = args[i+1]
				case "--granularity":
					o.Granularity = args[i+1]
				}
				i++
			}
		case strings.HasPrefix(a, "--from="):
			o.From = strings.TrimPrefix(a, "--from=")
		case strings.HasPrefix(a, "--to="):
			o.To = strings.TrimPrefix(a, "--to=")
		case strings.HasPrefix(a, "--provider="):
			o.Provider = strings.TrimPrefix(a, "--provider=")
		case strings.HasPrefix(a, "--model="):
			o.Model = strings.TrimPrefix(a, "--model=")
		case strings.HasPrefix(a, "--bucket="):
			o.Bucket = strings.TrimPrefix(a, "--bucket=")
		case strings.HasPrefix(a, "--granularity="):
			o.Granularity = strings.TrimPrefix(a, "--granularity=")
		case a == "--json":
			o.JSON = true
		case a == "--cost":
			o.Cost = true
		case a == "--by-agent":
			o.ByAgent = true
		case a == "--agent":
			if i+1 < len(args) {
				o.Agent = args[i+1]
				i++
			}
		}
	}
	return o
}

// StatsResp is the decoded /api/stats shape.
type StatsResp struct {
	From    int64                 `json:"from"`
	To      int64                 `json:"to"`
	Bucket  int64                 `json:"bucket"`
	Buckets []observestats.Bucket `json:"buckets"`
}

// cmdStats queries the running daemon's /api/stats endpoint and prints per-
// (provider, model) call statistics from the SQLite store. The daemon
// (`model-proxy serve`) must be running with web.enabled (default true).
// CmdStats renders the stats table for a loaded config. Config loading, flag
// parsing of --config and process exit stay in the application; Run returns a
// process exit code instead of exiting so the CLI package is testable.
func CmdStats(args []string, listen string, stdout, stderr io.Writer) int {
	out, err := RenderStats(listen, ParseStatsFlags(args))
	if err != nil {
		fmt.Fprintf(stderr, "%s %s\n", "✗", err)
		return 1
	}
	fmt.Fprint(stdout, out)
	return 0
}

// renderStats fetches /api/stats from the daemon and returns either the rendered
// terminal table or the raw JSON (opts.JSON). listen is the daemon "host:port".
// Extracted from cmdStats so tests can drive it against an httptest server.
//
// If --granularity or --cost is set, the request is routed to /api/analytics
// (calendar day/month bucketing with optional equivalent-cost column). With
// neither flag set, behavior is byte-identical to the legacy /api/stats path
// (the CLI display contract).
func RenderStats(listen string, opts StatsOpts) (string, error) {
	if opts.ByAgent {
		return RenderAgents(listen, opts)
	}
	if opts.Granularity != "" || opts.Cost {
		return RenderAnalytics(listen, opts)
	}
	base := "http://" + listen
	q := url.Values{}
	if opts.From != "" {
		q.Set("from", opts.From)
	}
	if opts.To != "" {
		q.Set("to", opts.To)
	}
	if opts.Provider != "" {
		q.Set("provider", opts.Provider)
	}
	if opts.Model != "" {
		q.Set("model", opts.Model)
	}
	if opts.Bucket != "" {
		q.Set("bucket", opts.Bucket)
	}
	body, status, err := daemonctl.Get(base, "/api/stats?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available - is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, display.Truncate(string(body), 200))
	}
	if opts.JSON {
		return string(body), nil
	}
	var resp StatsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse stats response: %w", err)
	}
	return FormatStatsSummary(SummarizeStats(resp)) + "\n" + FormatStatsTable(resp), nil
}

// AnalyticsResp is the decoded /api/analytics shape (the subset the table needs;
// cache_creation/cache_read and the totals/price_coverage objects are ignored
// by the CLI renderer).
type AnalyticsResp struct {
	Granularity string `json:"granularity"`
	From        int64  `json:"from"`
	To          int64  `json:"to"`
	Series      []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Points   []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		} `json:"points"`
	} `json:"series"`
}

// renderAnalytics fetches /api/analytics and renders a table. granularity
// defaults to day when only --cost is given. The /api/analytics endpoint is
// gated by web.enabled; a 404 surfaces the same web-UI hint as /api/stats.
func RenderAnalytics(listen string, opts StatsOpts) (string, error) {
	base := "http://" + listen
	q := url.Values{}
	g := opts.Granularity
	if g == "" {
		g = "day"
	}
	q.Set("granularity", g)
	if opts.From != "" {
		q.Set("from", opts.From)
	}
	if opts.To != "" {
		q.Set("to", opts.To)
	}
	if opts.Provider != "" {
		q.Set("provider", opts.Provider)
	}
	if opts.Model != "" {
		q.Set("model", opts.Model)
	}
	body, status, err := daemonctl.Get(base, "/api/analytics?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available - is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, display.Truncate(string(body), 200))
	}
	if opts.JSON {
		return string(body), nil
	}
	var resp AnalyticsResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse analytics response: %w", err)
	}
	return FormatStatsSummary(SummarizeAnalytics(resp)) + "\n" + FormatAnalyticsTable(resp, opts.Cost), nil
}

// formatAnalyticsTable renders analytics series as a compact terminal table.
// Per-series totals are SUMs across the series' points. The Cost column
// appears only when withCost is true (the --cost flag); a series with no
// priced points shows "n/a". The bucket column header is the granularity
// label (day/month); per-point buckets are not rendered (the CLI shows one
// row per (provider, model), aggregated across the window).
func FormatAnalyticsTable(resp AnalyticsResp, withCost bool) string {
	label := resp.Granularity
	if label == "" {
		label = "day"
	}
	if len(resp.Series) == 0 {
		from := time.Unix(resp.From, 0).Format("01-02 15:04")
		to := time.Unix(resp.To, 0).Format("01-02 15:04")
		return fmt.Sprintf("(no analytics in range %s .. %s, granularity %s)\n", from, to, label)
	}
	hdr := "%-16s %-20s %-12s %8s %10s %10s"
	if withCost {
		hdr += " %9s"
	}
	hdr += "\n"
	args := []any{"provider", "model", label, "reqs", "input", "output"}
	if withCost {
		args = append(args, "cost")
	}
	out := fmt.Sprintf(hdr, args...)
	for _, s := range resp.Series {
		var reqs, in, out2 uint64
		var costSum *float64
		for _, p := range s.Points {
			reqs += p.Requests
			in += p.Input
			out2 += p.Output
			if p.Cost != nil {
				if costSum == nil {
					costSum = new(float64)
				}
				*costSum += *p.Cost
			}
		}
		row := "%-16.16s %-20.20s %-12s %8s %10s %10s"
		rowArgs := []any{s.Provider, s.Model, label, cliframework.CompactNum(reqs), cliframework.CompactNum(in), cliframework.CompactNum(out2)}
		if withCost {
			row += " %9s"
			if costSum != nil {
				rowArgs = append(rowArgs, fmt.Sprintf("$%.2f", *costSum))
			} else {
				rowArgs = append(rowArgs, "n/a")
			}
		}
		out += fmt.Sprintf(row+"\n", rowArgs...)
	}
	return out
}

// formatStatsTable renders the bucket rows as a compact terminal table. The
// "bucket" column header reflects the display granularity (1m / 10m / 1h ...);
// the cell value is the bucket's start time. Empty result -> a short note.
func FormatStatsTable(resp StatsResp) string {
	bucketLabel := BucketLabel(resp.Bucket)
	if len(resp.Buckets) == 0 {
		from := time.Unix(resp.From, 0).Format("01-02 15:04")
		to := time.Unix(resp.To, 0).Format("01-02 15:04")
		return fmt.Sprintf("(no stats in range %s .. %s, bucket %s)\n", from, to, bucketLabel)
	}
	hdr := fmt.Sprintf("%-16s %-18s %-12s %8s %8s %8s %8s %10s %10s %8s %8s\n",
		"provider", "model", bucketLabel, "reqs", "failover", "429", "fail", "input", "output", "lat(ms)", "ttft(ms)")
	out := hdr
	for _, b := range resp.Buckets {
		out += fmt.Sprintf("%-16.16s %-18.18s %-12s %8s %8s %8s %8s %10s %10s %8s %8s\n",
			b.Provider, b.Model,
			time.Unix(b.Minute, 0).Format("01-02 15:04"),
			cliframework.CompactNum(b.Requests), cliframework.CompactNum(b.Failovers),
			cliframework.CompactNum(b.RateLimited429), cliframework.CompactNum(b.Failures),
			cliframework.CompactNum(b.Input), cliframework.CompactNum(b.Output),
			cliframework.CompactNum(uint64(b.AvgLatencyMs+0.5)), cliframework.CompactNum(uint64(b.AvgTtftMs+0.5)))
	}
	return out
}

// bucketLabel renders a bucket width (seconds) as a short column header. 60 ->
// "1m"; multiples of 3600 -> "Nh"; else "<min>m".
func BucketLabel(secs int64) string {
	if secs <= 60 {
		return "1m"
	}
	if secs%3600 == 0 {
		return fmt.Sprintf("%dh", secs/3600)
	}
	return fmt.Sprintf("%dm", secs/60)
}

// AgentResp is the decoded /api/agents shape (mirrors the StatsResp envelope,
// with agent-dimension buckets).
type AgentResp struct {
	From    int64                      `json:"from"`
	To      int64                      `json:"to"`
	Bucket  int64                      `json:"bucket"`
	Buckets []observestats.AgentBucket `json:"buckets"`
}

// renderAgents fetches /api/agents and renders a per-agent summary ("who is
// burning my quota") with a per-(provider, model) breakdown under each agent:
// requests, all four token buckets and their total, latency, failures. --json
// passes the raw /api/agents response through.
func RenderAgents(listen string, opts StatsOpts) (string, error) {
	base := "http://" + listen
	q := url.Values{}
	if opts.From != "" {
		q.Set("from", opts.From)
	}
	if opts.To != "" {
		q.Set("to", opts.To)
	}
	if opts.Bucket != "" {
		q.Set("bucket", opts.Bucket)
	}
	if opts.Agent != "" {
		q.Set("agent", opts.Agent)
	}
	// --provider/--model narrow the agent view server-side (/api/agents
	// supports them) — forward them like the plain /api/stats path does.
	if opts.Provider != "" {
		q.Set("provider", opts.Provider)
	}
	if opts.Model != "" {
		q.Set("model", opts.Model)
	}
	body, status, err := daemonctl.Get(base, "/api/agents?"+q.Encode())
	if err != nil {
		return "", fmt.Errorf("cannot reach daemon at %s: %v\nis `model-proxy serve` running?", listen, err)
	}
	if status == 404 {
		return "", fmt.Errorf("web UI endpoints not available - is web.enabled true on the daemon?")
	}
	if status != 200 {
		return "", fmt.Errorf("daemon returned HTTP %d: %s", status, display.Truncate(string(body), 200))
	}
	if opts.JSON {
		return string(body), nil
	}
	var resp AgentResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parse agents response: %w", err)
	}
	return FormatStatsSummary(SummarizeAgents(resp)) + "\n" + FormatAgentsTable(resp), nil
}

// formatAgentsTable collapses agent-dimension buckets into one summary row per
// agent plus one breakdown row per (provider, model) underneath it — requests,
// all four token buckets and their total, latency and failures summed across
// minutes in range. Agents sort by total tokens desc (heaviest on top), and so
// do the models within each agent.
func FormatAgentsTable(resp AgentResp) string {
	if len(resp.Buckets) == 0 {
		from := time.Unix(resp.From, 0).Format("01-02 15:04")
		to := time.Unix(resp.To, 0).Format("01-02 15:04")
		return fmt.Sprintf("(no agent stats in range %s .. %s)\n", from, to)
	}
	type agentTotals struct {
		Requests      uint64
		Input         uint64
		Output        uint64
		CacheCreation uint64
		CacheRead     uint64
		Latency       uint64
		Failures      uint64
	}
	totalTokens := func(t *agentTotals) uint64 {
		return t.Input + t.Output + t.CacheCreation + t.CacheRead
	}
	per := map[string]*agentTotals{}
	perModel := map[string]map[string]*agentTotals{}
	for _, b := range resp.Buckets {
		t := per[b.Agent]
		if t == nil {
			t = &agentTotals{}
			per[b.Agent] = t
		}
		t.Requests += b.Requests
		t.Input += b.Input
		t.Output += b.Output
		t.CacheCreation += b.CacheCreation
		t.CacheRead += b.CacheRead
		t.Latency += b.LatencySum
		t.Failures += b.Failures
		models := perModel[b.Agent]
		if models == nil {
			models = map[string]*agentTotals{}
			perModel[b.Agent] = models
		}
		pm := models[b.Provider+"/"+b.Model]
		if pm == nil {
			pm = &agentTotals{}
			models[b.Provider+"/"+b.Model] = pm
		}
		pm.Requests += b.Requests
		pm.Input += b.Input
		pm.Output += b.Output
		pm.CacheCreation += b.CacheCreation
		pm.CacheRead += b.CacheRead
		pm.Latency += b.LatencySum
		pm.Failures += b.Failures
	}
	agents := make([]string, 0, len(per))
	for a := range per {
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		ti, tj := totalTokens(per[agents[i]]), totalTokens(per[agents[j]])
		if ti != tj {
			return ti > tj
		}
		return agents[i] < agents[j]
	})
	out := fmt.Sprintf("%-24s %8s %10s %10s %12s %11s %10s %8s %8s\n",
		"agent / model", "reqs", "input", "output", "cache_create", "cache_read", "total", "lat", "fail")
	formatRow := func(label string, t *agentTotals) string {
		avgLat := uint64(0)
		if t.Requests > 0 {
			avgLat = t.Latency / t.Requests
		}
		return fmt.Sprintf("%-24.24s %8s %10s %10s %12s %11s %10s %8s %8s\n",
			label, cliframework.CompactNum(t.Requests), cliframework.CompactNum(t.Input),
			cliframework.CompactNum(t.Output), cliframework.CompactNum(t.CacheCreation),
			cliframework.CompactNum(t.CacheRead), cliframework.CompactNum(totalTokens(t)),
			cliframework.CompactNum(avgLat), cliframework.CompactNum(t.Failures))
	}
	for _, a := range agents {
		out += formatRow(a, per[a])
		models := perModel[a]
		names := make([]string, 0, len(models))
		for name := range models {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool {
			ti, tj := totalTokens(models[names[i]]), totalTokens(models[names[j]])
			if ti != tj {
				return ti > tj
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			out += formatRow("  "+name, models[name])
		}
	}
	return out
}

// StatsSummary is the pre-aggregated data behind the one-line human summary
// prepended to every human-readable (non --json) stats output. It is computed
// from the already-fetched query response — the CLI never issues a second
// request for it. Segments with no data are simply omitted from the line.
type StatsSummary struct {
	From       int64
	To         int64
	Requests   uint64
	Failures   uint64   // terminal failures + 429s; rendered only when HasSuccess
	HasSuccess bool     // whether the query path reports failure counters at all
	Cost       *float64 // equivalent cost; nil when nothing in range was priced
	TopName    string   // heaviest provider (stats/analytics) or agent (--by-agent)
	TopShare   float64  // TopName's share, 0..1 (of requests; of tokens for --by-agent)
}

// FormatStatsSummary renders the one-line summary, e.g.
// "今天 214 请求 · 成功率 97.2% · 等价成本 $3.21 · kimi-code 承担 61%".
// With no requests in range it returns "<窗口> 暂无数据".
func FormatStatsSummary(s StatsSummary) string {
	win := summaryWindowLabel(s.From, s.To)
	if s.Requests == 0 {
		return win + " 暂无数据"
	}
	parts := []string{fmt.Sprintf("%s %s 请求", win, cliframework.CompactNum(s.Requests))}
	if s.HasSuccess {
		failed := s.Failures
		if failed > s.Requests {
			failed = s.Requests
		}
		parts = append(parts, fmt.Sprintf("成功率 %.1f%%", float64(s.Requests-failed)/float64(s.Requests)*100))
	}
	if s.Cost != nil {
		parts = append(parts, fmt.Sprintf("等价成本 $%.2f", *s.Cost))
	}
	if s.TopName != "" {
		parts = append(parts, fmt.Sprintf("%s 承担 %d%%", s.TopName, int(s.TopShare*100+0.5)))
	}
	return strings.Join(parts, " · ")
}

// summaryWindowLabel names the query window: "今天" when [from, to] falls
// inside the current local day, else an explicit "MM-DD HH:MM ~ MM-DD HH:MM"
// range.
func summaryWindowLabel(from, to int64) string {
	f := time.Unix(from, 0)
	t := time.Unix(to, 0)
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if !f.Before(dayStart) && t.Before(dayStart.Add(24*time.Hour)) {
		return "今天"
	}
	return f.Format("01-02 15:04") + " ~ " + t.Format("01-02 15:04")
}

// topShare returns the heaviest contributor in per and its share of total.
// Ties break on the lexicographically smaller name for stable output.
func topShare(per map[string]uint64, total uint64) (string, float64) {
	best := ""
	var bestN uint64
	for name, n := range per {
		if n > bestN || (n == bestN && n > 0 && (best == "" || name < best)) {
			best, bestN = name, n
		}
	}
	if best == "" || total == 0 {
		return "", 0
	}
	return best, float64(bestN) / float64(total)
}

// SummarizeStats aggregates the /api/stats response for the summary line.
// Success rate counts terminal failures and 429s as non-successful attempts.
func SummarizeStats(resp StatsResp) StatsSummary {
	s := StatsSummary{From: resp.From, To: resp.To, HasSuccess: true}
	per := map[string]uint64{}
	for _, b := range resp.Buckets {
		s.Requests += b.Requests
		s.Failures += b.Failures + b.RateLimited429
		per[b.Provider] += b.Requests
	}
	s.TopName, s.TopShare = topShare(per, s.Requests)
	return s
}

// SummarizeAnalytics aggregates the /api/analytics response: requests,
// equivalent cost (only when at least one priced point exists) and the top
// provider. The endpoint reports no failure counters, so the summary carries
// no success-rate segment.
func SummarizeAnalytics(resp AnalyticsResp) StatsSummary {
	s := StatsSummary{From: resp.From, To: resp.To}
	per := map[string]uint64{}
	var cost *float64
	for _, series := range resp.Series {
		for _, p := range series.Points {
			s.Requests += p.Requests
			per[series.Provider] += p.Requests
			if p.Cost != nil {
				if cost == nil {
					cost = new(float64)
				}
				*cost += *p.Cost
			}
		}
	}
	s.Cost = cost
	s.TopName, s.TopShare = topShare(per, s.Requests)
	return s
}

// SummarizeAgents aggregates the /api/agents response. The top contributor is
// the heaviest agent by total tokens (all four buckets: input + output +
// cache_creation + cache_read), matching the table's sort order.
func SummarizeAgents(resp AgentResp) StatsSummary {
	s := StatsSummary{From: resp.From, To: resp.To, HasSuccess: true}
	perTokens := map[string]uint64{}
	var totalTokens uint64
	for _, b := range resp.Buckets {
		s.Requests += b.Requests
		s.Failures += b.Failures
		tokens := b.Input + b.Output + b.CacheCreation + b.CacheRead
		perTokens[b.Agent] += tokens
		totalTokens += tokens
	}
	s.TopName, s.TopShare = topShare(perTokens, totalTokens)
	return s
}
