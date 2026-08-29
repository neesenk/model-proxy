package cli_test

import (
	"encoding/json"
	"io"
	"model-proxy/internal/cli"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	observestats "model-proxy/internal/observe/stats"
)

// TestRenderStatsCLI verifies the `stats` CLI renders the daemon's /api/stats
// response into a terminal table (and --json passes raw JSON through).
func TestRenderStatsCLI(t *testing.T) {
	minute := time.Now().Unix() / 60 * 60
	resp := cli.StatsResp{From: minute - 60, To: minute, Bucket: 60, Buckets: []observestats.Bucket{
		{Provider: "zhipu", Model: "glm-5", Minute: minute, Requests: 7, Input: 100, Output: 20},
	}}
	raw, _ := json.Marshal(resp)

	// The handler runs on the server's goroutine; guard shared state with a
	// mutex so the test goroutine's reads are race-free.
	var mu sync.Mutex
	var gotQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/stats" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		gotQuery = r.URL.RawQuery
		mu.Unlock()
		io.WriteString(w, string(raw))
	}))
	defer up.Close()

	// renderStats takes "host:port"; derive from the httptest.Server URL.
	listen := strings.TrimPrefix(up.URL, "http://")
	out, err := cli.RenderStats(listen, cli.StatsOpts{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"zhipu", "glm-5", "7", "1m", "lat(ms)", "ttft(ms)"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered table missing %q:\n%s", want, out)
		}
	}
	// Human-readable mode prepends the one-line summary (built from the same
	// response, no second query): total requests, success rate, top provider.
	firstLine := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{"7 请求", "成功率 100.0%", "zhipu 承担 100%"} {
		if !strings.Contains(firstLine, want) {
			t.Errorf("summary line missing %q:\n%s", want, firstLine)
		}
	}
	// No --bucket flag -> query string omits bucket (server defaults to 1m).
	mu.Lock()
	query := gotQuery
	mu.Unlock()
	if strings.Contains(query, "bucket=") {
		t.Errorf("default query should omit bucket, got %q", query)
	}

	// --bucket 10m is forwarded to the daemon's query string.
	if _, err := cli.RenderStats(listen, cli.StatsOpts{Bucket: "10m"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	query = gotQuery
	mu.Unlock()
	if !strings.Contains(query, "bucket=10m") {
		t.Errorf("--bucket 10m not forwarded, query=%q", query)
	}

	// --json passes the raw body through.
	outJSON, err := cli.RenderStats(listen, cli.StatsOpts{JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if outJSON != string(raw) {
		t.Errorf("stats --json passthrough mismatch:\ngot:  %s\nwant: %s", outJSON, raw)
	}
}

// TestNormalizeBucket covers the granularity-spec parser: durations, bare
// seconds, defaults, clamping, and round-up-to-60-multiple.
func TestNormalizeBucket(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"", 60},       // default -> raw 1m
		{"0", 60},      // explicit zero -> raw 1m
		{"1m", 60},     // 1 minute
		{"10m", 600},   // 10 minutes
		{"1h", 3600},   // 1 hour
		{"24h", 86400}, // 1 day (as 24h; "d" is not a Go duration unit)
		{"30", 60},     // 30s < 60 -> clamp to 60
		{"90", 120},    // 90s not a 60-multiple -> round up to 120
		{"120", 120},   // exact multiple
		{"garbage", 60},
	}
	for _, c := range cases {
		if got := observestats.NormalizeBucket(c.in); got != c.want {
			t.Errorf("observestats.NormalizeBucket(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestBucketLabel covers the CLI column-header rendering.
func TestBucketLabel(t *testing.T) {
	cases := []struct {
		secs int64
		want string
	}{
		{0, "1m"}, {60, "1m"}, {600, "10m"}, {3600, "1h"}, {86400, "24h"},
	}
	for _, c := range cases {
		if got := cli.BucketLabel(c.secs); got != c.want {
			t.Errorf("cli.BucketLabel(%d) = %q, want %q", c.secs, got, c.want)
		}
	}
}

func TestParseStatsFlags(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want cli.StatsOpts
	}{
		{"empty", nil, cli.StatsOpts{}},
		{"space-separated", []string{"--from", "2026-01-01", "--to", "2026-02-01",
			"--provider", "zhipu", "--model", "glm-5.2", "--bucket", "5m", "--json"},
			cli.StatsOpts{From: "2026-01-01", To: "2026-02-01", Provider: "zhipu",
				Model: "glm-5.2", Bucket: "5m", JSON: true}},
		{"equals form", []string{"--from=2026-01-01", "--to=2026-02-01",
			"--provider=zhipu", "--model=glm-5.2", "--bucket=5m"},
			cli.StatsOpts{From: "2026-01-01", To: "2026-02-01", Provider: "zhipu",
				Model: "glm-5.2", Bucket: "5m"}},
		{"value at end without arg", []string{"--from"}, cli.StatsOpts{}},
		{"unknown flag ignored", []string{"--bogus", "x"}, cli.StatsOpts{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cli.ParseStatsFlags(tc.args)
			if got != tc.want {
				t.Errorf("cli.ParseStatsFlags(%v) = %+v, want %+v", tc.args, got, tc.want)
			}
		})
	}
}

// TestFormatStatsSummary covers the one-line summary: segment omission when a
// query path has no such data, the empty-range "暂无数据" line, window labels
// (今天 vs explicit range), success-rate clamping, and top-contributor share.
func TestFormatStatsSummary(t *testing.T) {
	// No data -> 暂无数据, one line, no table fragments.
	out := cli.FormatStatsSummary(cli.StatsSummary{From: 1700000000, To: 1700003600})
	if !strings.Contains(out, "暂无数据") {
		t.Errorf("empty summary missing 暂无数据: %q", out)
	}
	if strings.Contains(out, "请求 ·") || strings.Contains(out, "成功率") {
		t.Errorf("empty summary should have no request/success segments: %q", out)
	}
	// Old timestamps -> explicit range label instead of 今天.
	if !strings.Contains(out, " ~ ") {
		t.Errorf("non-today window should render an explicit range: %q", out)
	}

	// Full line: today window, success rate, cost, top contributor.
	now := time.Now()
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	cost := 3.21
	s := cli.StatsSummary{
		From: dayStart.Unix(), To: now.Unix(),
		Requests: 214, Failures: 6, HasSuccess: true,
		Cost: &cost, TopName: "kimi-code", TopShare: 0.61,
	}
	out = cli.FormatStatsSummary(s)
	want := "今天 214 请求 · 成功率 97.2% · 等价成本 $3.21 · kimi-code 承担 61%"
	if out != want {
		t.Errorf("full summary:\ngot:  %s\nwant: %s", out, want)
	}

	// Segments drop when the data is absent (analytics: no success/cost).
	out = cli.FormatStatsSummary(cli.StatsSummary{
		From: dayStart.Unix(), To: now.Unix(), Requests: 8,
		TopName: "deepseek", TopShare: 0.625,
	})
	if strings.Contains(out, "成功率") || strings.Contains(out, "等价成本") {
		t.Errorf("missing data must omit the segment: %q", out)
	}
	if !strings.Contains(out, "deepseek 承担 63%") {
		t.Errorf("top share rounding wrong: %q", out)
	}

	// Failures can exceed requests when 429s overlap terminal failures — clamp
	// the success rate at 0.0% instead of going negative.
	out = cli.FormatStatsSummary(cli.StatsSummary{
		From: dayStart.Unix(), To: now.Unix(),
		Requests: 10, Failures: 12, HasSuccess: true,
	})
	if !strings.Contains(out, "成功率 0.0%") {
		t.Errorf("success rate should clamp at 0.0%%: %q", out)
	}
}

// TestSummarizeStats verifies the /api/stats aggregation: requests, failures
// (terminal + 429), and the top provider by requests.
func TestSummarizeStats(t *testing.T) {
	s := cli.SummarizeStats(cli.StatsResp{From: 1, To: 2, Buckets: []observestats.Bucket{
		{Provider: "zhipu", Model: "glm-5", Requests: 7, Failures: 1, RateLimited429: 1},
		{Provider: "kimi-code", Model: "k2", Requests: 3},
	}})
	if s.Requests != 10 || s.Failures != 2 || !s.HasSuccess {
		t.Errorf("summarizeStats = %+v", s)
	}
	if s.TopName != "zhipu" || s.TopShare != 0.7 {
		t.Errorf("top = %s %.2f, want zhipu 0.70", s.TopName, s.TopShare)
	}
}

func TestFormatStatsTable(t *testing.T) {
	// Empty buckets -> "no stats" line.
	out := cli.FormatStatsTable(cli.StatsResp{From: 1700000000, To: 1700003600, Bucket: 60})
	if !strings.Contains(out, "no stats") || !strings.Contains(out, "bucket 1m") {
		t.Errorf("formatStatsTable empty missing 'no stats':\n%s", out)
	}
	// With buckets -> header + rows.
	resp := cli.StatsResp{From: 1700000000, To: 1700003600, Bucket: 3600, Buckets: []observestats.Bucket{
		{Provider: "zhipu", Model: "glm-5.2", Minute: 1700000000, Requests: 100, Failovers: 2, Failures: 1, Input: 5000, Output: 3000},
	}}
	out = cli.FormatStatsTable(resp)
	if !strings.Contains(out, "provider") || !strings.Contains(out, "zhipu") || !strings.Contains(out, "glm-5.2") {
		t.Errorf("formatStatsTable rows missing marker:\n%s", out)
	}
}

// TestStatsFlags_GranularityCost_Parsed verifies --granularity and --cost parse
// into cli.StatsOpts (the new analytics-routing flags).
func TestStatsFlags_GranularityCost_Parsed(t *testing.T) {
	o := cli.ParseStatsFlags([]string{"--granularity", "month", "--cost", "--provider", "deepseek"})
	if o.Granularity != "month" || !o.Cost || o.Provider != "deepseek" {
		t.Errorf("parsed = %+v, want granularity=month cost=true provider=deepseek", o)
	}
}

// TestStatsFlags_GranularityCost_DefaultOff verifies the new flags default off
// (the CLI display contract: no behavioral change without flags).
func TestStatsFlags_GranularityCost_DefaultOff(t *testing.T) {
	o := cli.ParseStatsFlags([]string{"--from", "1", "--bucket", "1h"})
	if o.Granularity != "" || o.Cost {
		t.Errorf("new flags should default off: %+v", o)
	}
}

// TestStatsFlags_GranularityCost_EqualsForm verifies --granularity=value parses.
func TestStatsFlags_GranularityCost_EqualsForm(t *testing.T) {
	o := cli.ParseStatsFlags([]string{"--granularity=day", "--cost"})
	if o.Granularity != "day" || !o.Cost {
		t.Errorf("equals form: parsed = %+v, want granularity=day cost=true", o)
	}
}

// TestRenderStatsCLI_AnalyticsPath verifies --granularity/--cost route to
// /api/analytics (not /api/stats) and the table renders provider/model/cost.
// The table shape comes from formatAnalyticsTable; the cost column appears only
// when --cost is set. The classic /api/stats path is still covered by
// TestRenderStatsCLI above (byte-identity guard).
func TestRenderStatsCLI_AnalyticsPath(t *testing.T) {
	// granularity=month, two series, one with cost and one without (n/a).
	body := `{"granularity":"month","from":1700000000,"to":1700000000,` +
		`"series":[` +
		`{"provider":"deepseek","model":"deepseek-chat","points":[` +
		`{"bucket":1700000000,"requests":5,"input":1000,"output":500,"cost":0.12,"priced":true}]},` +
		`{"provider":"zhipu","model":"glm-5","points":[` +
		`{"bucket":1700000000,"requests":3,"input":200,"output":80,"cost":null,"priced":false}]}` +
		`],"totals":{"input":1200,"output":580,"cost":0.12},` +
		`"price_coverage":{"priced":["deepseek-chat"],"unpriced":["glm-5"]}}`

	// Handler-side writes vs test-side reads: guard with a mutex (the CLI
	// client may run in a subprocess, so no in-process edge synchronizes them).
	var mu sync.Mutex
	var sawAnalytics, sawStats bool
	var lastQuery string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/analytics" {
			mu.Lock()
			sawAnalytics = true
			lastQuery = r.URL.RawQuery
			mu.Unlock()
			io.WriteString(w, body)
			return
		}
		if r.URL.Path == "/api/stats" {
			mu.Lock()
			sawStats = true
			mu.Unlock()
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	// --granularity month --cost: routes to /api/analytics, table has a cost
	// column with the summed cost ($0.12) and n/a for the unpriced series.
	out, err := cli.RenderStats(listen, cli.StatsOpts{Granularity: "month", Cost: true})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	analyticsHit, statsHit, query := sawAnalytics, sawStats, lastQuery
	mu.Unlock()
	if !analyticsHit {
		t.Error("expected request to /api/analytics")
	}
	if statsHit {
		t.Error("did not expect request to /api/stats when granularity/cost set")
	}
	if !strings.Contains(query, "granularity=month") {
		t.Errorf("query missing granularity=month: %q", query)
	}
	for _, want := range []string{"deepseek", "deepseek-chat", "$0.12", "n/a", "month"} {
		if !strings.Contains(out, want) {
			t.Errorf("analytics table missing %q:\n%s", want, out)
		}
	}
	// Summary line (from the same analytics response): total requests,
	// equivalent cost of priced points, top provider by requests (5/8 = 63%).
	firstLine := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{"8 请求", "等价成本 $0.12", "deepseek 承担 63%"} {
		if !strings.Contains(firstLine, want) {
			t.Errorf("analytics summary missing %q:\n%s", want, firstLine)
		}
	}
	// /api/analytics reports no failure counters -> no success-rate segment.
	if strings.Contains(firstLine, "成功率") {
		t.Errorf("analytics summary must not invent a success rate:\n%s", firstLine)
	}

	// --cost only (no granularity): defaults to day in the query string.
	mu.Lock()
	sawAnalytics = false
	mu.Unlock()
	if _, err := cli.RenderStats(listen, cli.StatsOpts{Cost: true}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	analyticsHit, query = sawAnalytics, lastQuery
	mu.Unlock()
	if !analyticsHit {
		t.Error("--cost alone should still route to /api/analytics")
	}
	if !strings.Contains(query, "granularity=day") {
		t.Errorf("--cost alone should default granularity=day in query: %q", query)
	}
}

// TestRenderStatsCLI_AnalyticsJSON verifies --json passes the analytics body
// through unchanged.
func TestRenderStatsCLI_AnalyticsJSON(t *testing.T) {
	body := `{"granularity":"day","series":[],"price_coverage":{"priced":[],"unpriced":[]}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")
	out, err := cli.RenderStats(listen, cli.StatsOpts{Granularity: "day", JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if out != body {
		t.Errorf("analytics --json passthrough mismatch:\ngot:  %s\nwant: %s", out, body)
	}
}

// TestFormatAnalyticsTable verifies the table renderer directly: header label
// reflects granularity, cost column appears only with withCost, and per-series
// cost sums correctly ( priced: $X.XX ; unpriced: n/a ).
func TestFormatAnalyticsTable(t *testing.T) {
	cost := 0.12
	resp := cli.AnalyticsResp{Granularity: "month"}
	resp.Series = []struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Points   []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		} `json:"points"`
	}{
		{Provider: "deepseek", Model: "deepseek-chat", Points: []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		}{
			{Requests: 3, Input: 500, Output: 100, Cost: &cost},
			{Requests: 2, Input: 500, Output: 100, Cost: &cost},
		}},
		{Provider: "zhipu", Model: "glm-5", Points: []struct {
			Requests uint64   `json:"requests"`
			Input    uint64   `json:"input"`
			Output   uint64   `json:"output"`
			Cost     *float64 `json:"cost"`
		}{
			{Requests: 1, Input: 10, Output: 5, Cost: nil},
		}},
	}

	// Without cost: 6-column table, no "cost" header, no $ values.
	out := cli.FormatAnalyticsTable(resp, false)
	if !strings.Contains(out, "month") || !strings.Contains(out, "deepseek") {
		t.Errorf("table missing markers:\n%s", out)
	}
	if strings.Contains(out, "cost") || strings.Contains(out, "$") {
		t.Errorf("without --cost, table should not mention cost/$:\n%s", out)
	}
	// Series totals are sums across points.
	if !strings.Contains(out, cli.CompactNum(5)) { // 3+2 requests
		t.Errorf("month-1 requests sum missing:\n%s", out)
	}

	// With cost: 7-column table; priced series shows $0.24 (0.12+0.12),
	// unpriced shows n/a.
	out = cli.FormatAnalyticsTable(resp, true)
	if !strings.Contains(out, "cost") {
		t.Errorf("with --cost, table should have a cost header:\n%s", out)
	}
	if !strings.Contains(out, "$0.24") {
		t.Errorf("priced series cost should sum to $0.24:\n%s", out)
	}
	if !strings.Contains(out, "n/a") {
		t.Errorf("unpriced series should show n/a:\n%s", out)
	}
}

// TestConvertHelpers: finish↔stop maps (all branches), text extraction, backend
// path, and the SSE reader selector.
// TestFormatAgentsTable_Latency: the --by-agent table includes the new latency
// + failure columns.
func TestFormatAgentsTable_Latency(t *testing.T) {
	resp := cli.AgentResp{Bucket: 60, Buckets: []observestats.AgentBucket{
		{Agent: "claude-code", Requests: 10, Input: 100, Output: 50, LatencySum: 2000, Failures: 1},
	}}
	out := cli.FormatAgentsTable(resp)
	for _, want := range []string{"claude-code", "lat", "fail", "10", "200", "1"} {
		if !strings.Contains(out, want) {
			t.Errorf("formatAgentsTable missing %q:\n%s", want, out)
		}
	}
}

// TestRenderAgentsCLI: `stats --by-agent` fetches /api/agents and renders a
// per-agent summary sorted by total tokens desc, with the exact header + the
// heaviest agent on top. Guards the CLI display contract for the agent view.
func TestRenderAgentsCLI(t *testing.T) {
	resp := cli.AgentResp{From: 1, To: 2, Bucket: 60, Buckets: []observestats.AgentBucket{
		{Agent: "claude-code", Requests: 10, Input: 5000, Output: 800},
		{Agent: "codex", Requests: 3, Input: 200, Output: 50},
	}}
	raw, _ := json.Marshal(resp)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, string(raw))
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	out, err := cli.RenderAgents(listen, cli.StatsOpts{ByAgent: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "claude-code") || !strings.Contains(out, "codex") {
		t.Errorf("agent table missing agents:\n%s", out)
	}
	// Exact header columns.
	if !strings.Contains(out, "agent") || !strings.Contains(out, "reqs") || !strings.Contains(out, "input") || !strings.Contains(out, "output") {
		t.Errorf("agent table missing a header:\n%s", out)
	}
	// claude-code (5800 tokens) sorts above codex (250 tokens).
	if strings.Index(out, "claude-code") > strings.Index(out, "codex") {
		t.Errorf("heaviest agent not on top:\n%s", out)
	}
	// Summary line: total requests, success rate, and the top agent by tokens
	// (claude-code 5800/6050 = 96%).
	firstLine := strings.SplitN(out, "\n", 2)[0]
	for _, want := range []string{"13 请求", "成功率 100.0%", "claude-code 承担 96%"} {
		if !strings.Contains(firstLine, want) {
			t.Errorf("agents summary missing %q:\n%s", want, firstLine)
		}
	}

	outJSON, err := cli.RenderAgents(listen, cli.StatsOpts{ByAgent: true, JSON: true})
	if err != nil {
		t.Fatal(err)
	}
	if outJSON != string(raw) {
		t.Errorf("agents --json passthrough mismatch:\ngot:  %s\nwant: %s", outJSON, raw)
	}
}

// TestRenderAgents_ProviderModelFilter: --provider/--model are forwarded to
// /api/agents as query params in --by-agent mode (the server side already
// filters on them); previously the CLI silently dropped them here.
func TestRenderAgents_ProviderModelFilter(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agents" {
			http.NotFound(w, r)
			return
		}
		if got := r.URL.Query().Get("provider"); got != "zhipu" {
			t.Errorf("provider query=%q want zhipu", got)
		}
		if got := r.URL.Query().Get("model"); got != "glm-5.2" {
			t.Errorf("model query=%q want glm-5.2", got)
		}
		io.WriteString(w, `{"from":1,"to":2,"bucket":60,"buckets":[]}`)
	}))
	defer up.Close()
	listen := strings.TrimPrefix(up.URL, "http://")

	if _, err := cli.RenderAgents(listen, cli.StatsOpts{ByAgent: true, Provider: "zhipu", Model: "glm-5.2"}); err != nil {
		t.Fatal(err)
	}
}
