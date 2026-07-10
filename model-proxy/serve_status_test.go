package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompactNum(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{5, "5"},
		{567, "567"},
		{1000, "1k"},
		{1234, "1.2k"},
		{450000, "450k"},
		{1000000, "1M"},
		{1200000, "1.2M"},
		{1000000000, "1B"},
		{5600000000, "5.6B"},
		{999, "999"},
		{950000, "950k"},
		{999949, "999.9k"},
		{999999, "1M"},
		{999999999, "1B"},
	}
	for _, c := range cases {
		if got := compactNum(c.in); got != c.want {
			t.Errorf("compactNum(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatClock(t *testing.T) {
	if got := formatClock(0); got != "—" {
		t.Errorf("formatClock(0) = %q, want —", got)
	}
	if got := formatClock(-5); got != "—" {
		t.Errorf("formatClock(-5) = %q, want —", got)
	}
	want := time.Unix(1700000000, 0).Local().Format("15:04:05")
	if got := formatClock(1700000000); got != want {
		t.Errorf("formatClock(1700000000) = %q, want %q", got, want)
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1, "route", "routes"); got != "route" {
		t.Errorf("plural(1) = %q, want route", got)
	}
	if got := plural(3, "route", "routes"); got != "routes" {
		t.Errorf("plural(3) = %q, want routes", got)
	}
}

func TestRenderProviders(t *testing.T) {
	st := &statusResp{
		Health: map[string]statusHealth{
			"aqp":   {CircuitState: "closed", Available: true},
			"codex": {CircuitState: "open"},
			"zhipu": {CircuitState: "closed", RateLimitedUntil: "2099-01-01T00:00:00Z"},
		},
		Counters: map[string]statusCounters{
			"aqp":   {Requests: 1234, Failovers: 12, RateLimited: 3, Failures: 5, LastRequestAt: 1700000000},
			"codex": {Requests: 567, Failovers: 45, RateLimited: 8, Failures: 20, LastRequestAt: 0},
		},
	}
	out := renderProviders(st)
	for _, want := range []string{"PROVIDER", "HEALTH", "REQS", "FAILOVERS", "429", "FAILURES", "LAST"} {
		if !strings.Contains(out, want) {
			t.Errorf("header missing %q in:\n%s", want, out)
		}
	}
	// sorted rows: aqp before codex before zhipu
	if i, j := strings.Index(out, "aqp"), strings.Index(out, "codex"); !(i >= 0 && j > i) {
		t.Errorf("want aqp before codex, got aqp@%d codex@%d", i, j)
	}
	if !strings.Contains(out, "1.2k") {
		t.Errorf("want aqp reqs compact 1.2k, got:\n%s", out)
	}
	if !strings.Contains(out, "available") {
		t.Errorf("want 'available' label, got:\n%s", out)
	}
	if !strings.Contains(out, "circuit open") {
		t.Errorf("want 'circuit open' label, got:\n%s", out)
	}
	if !strings.Contains(out, "rate-limited") {
		t.Errorf("want 'rate-limited' label, got:\n%s", out)
	}
}

func TestRenderScheduleRoutes(t *testing.T) {
	models := map[string]statusRoute{
		"claude-sonnet": {
			First: "aqp",
			Ordered: []statusOrdered{
				{Provider: "aqp", Priority: 1, Tier: "plan", Surplus: 12.3, Available: true},
				{Provider: "codex", Priority: 1, Tier: "plan", Surplus: 8.1, Available: false},
			},
			Sticky:   "aqp",
			DwellRem: 320,
		},
	}
	// ind="" reproduces the `schedule` command layout (route at col 0, details +4);
	// ind="  " is used by the serve-status section (route +2, details +6).
	cases := []struct{ ind, routePfx, detailPfx string }{
		{"", "claude-sonnet → aqp\n", "    aqp"},
		{"  ", "  claude-sonnet → aqp\n", "      aqp"},
	}
	for _, tc := range cases {
		out := renderScheduleRoutes(models, tc.ind)
		if !strings.HasPrefix(out, tc.routePfx) {
			t.Errorf("ind=%q: want prefix %q, got:\n%s", tc.ind, tc.routePfx, out)
		}
		foundDetail := false
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, "surplus +12.30") {
				foundDetail = true
				if !strings.HasPrefix(line, tc.detailPfx) {
					t.Errorf("ind=%q: detail line %q want prefix %q", tc.ind, line, tc.detailPfx)
				}
			}
		}
		if !foundDetail {
			t.Errorf("ind=%q: missing surplus detail line", tc.ind)
		}
		for _, want := range []string{"surplus +12.30", "surplus +8.10", "(unavailable)", "sticky: aqp, 320s dwell left"} {
			if !strings.Contains(out, want) {
				t.Errorf("ind=%q: missing %q in:\n%s", tc.ind, want, out)
			}
		}
	}
}

func TestRenderSchedule(t *testing.T) {
	st := &statusResp{Schedule: statusSchedule{Models: map[string]statusRoute{
		"gpt-5.5": {First: "codex"},
	}}}
	out := renderSchedule(st)
	if !strings.HasPrefix(out, "Schedule (1 route)\n") {
		t.Errorf("header wrong, got:\n%s", out)
	}
	if !strings.Contains(out, "  gpt-5.5 → codex\n") {
		t.Errorf("route should be indented 2 under the section, got:\n%s", out)
	}
}

func TestRenderScheduleEmpty(t *testing.T) {
	if got := renderSchedule(&statusResp{}); got != "" {
		t.Errorf("empty schedule should render nothing, got %q", got)
	}
}

func TestRenderQuota(t *testing.T) {
	st := &statusResp{
		Quota: map[string]statusQuota{
			"aqp": {
				Account: "work", Plan: "plan",
				Windows: []statusWindow{
					{Label: "Monthly", RemainingPct: 0.62, Ultimate: true, ResetsAt: time.Now().Add(time.Hour)},
					{Label: "5h tokens", RemainingPct: 0.88, Short: true},
					{Label: "daily", RemainingPct: 0.5}, // neither Ultimate nor Short → no tag
				},
			},
			"codex": {Err: "rate limited"},
		},
	}
	out := renderQuota(st)
	// sorted: aqp before codex
	if i, j := strings.Index(out, "aqp"), strings.Index(out, "codex"); !(i >= 0 && j > i) {
		t.Errorf("want aqp before codex, got aqp@%d codex@%d", i, j)
	}
	for _, want := range []string{"work", "plan", "Monthly (ultimate)", "62%", "5h tokens (short)", "88%", "resets", "daily"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// an intermediate window (neither Ultimate nor Short) must NOT be mislabeled "(short)"
	if strings.Contains(out, "daily (short)") || strings.Contains(out, "daily (ultimate)") {
		t.Errorf("intermediate window mislabeled with a tag, got:\n%s", out)
	}
	if !strings.Contains(out, "no data") {
		t.Errorf("want 'no data' for codex error, got:\n%s", out)
	}
}

func TestRenderQuotaEmpty(t *testing.T) {
	if got := renderQuota(&statusResp{}); got != "" {
		t.Errorf("empty quota should render nothing, got %q", got)
	}
}

func TestRenderWarnings(t *testing.T) {
	if got := renderWarnings(&statusResp{}); got != "" {
		t.Errorf("empty warnings should render nothing, got %q", got)
	}
	out := renderWarnings(&statusResp{Warnings: []string{
		`model "foo" served by 2 logged-in providers (aqp, zhipu); auto-routing to aqp`,
	}})
	for _, want := range []string{"implicit-route warnings", "foo", "aqp", "zhipu"} {
		if !strings.Contains(out, want) {
			t.Errorf("warnings render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTokens(t *testing.T) {
	tok := &tokensResp{Usage: []tokenEntry{
		{Provider: "zhipu", Model: "glm-4.6", Input: 1000, Output: 500, Requests: 1},
		{Provider: "aqp", Model: "claude-sonnet", Input: 1200000, Output: 450000, CacheCreation: 200000, CacheRead: 1100000, Requests: 1234},
	}}
	out := renderTokens(tok)
	// sorted by provider then model: aqp before zhipu
	if i, j := strings.Index(out, "aqp"), strings.Index(out, "zhipu"); !(i >= 0 && j > i) {
		t.Errorf("want aqp before zhipu, got aqp@%d zhipu@%d", i, j)
	}
	for _, want := range []string{"INPUT", "OUTPUT", "CACHE-CR", "CACHE-RD", "REQUESTS", "1.2M", "2 models"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderTokensEmpty(t *testing.T) {
	if got := renderTokens(&tokensResp{}); got != "" {
		t.Errorf("empty tokens should render nothing, got %q", got)
	}
}

func TestRenderLogs(t *testing.T) {
	out := renderLogs(&logsResp{Lines: []string{"line one", "line two"}})
	if !strings.Contains(out, "Logs (last 2)") {
		t.Errorf("want header, got:\n%s", out)
	}
	for _, want := range []string{"line one", "line two"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderLogsEmpty(t *testing.T) {
	if got := renderLogs(&logsResp{}); got != "" {
		t.Errorf("empty logs should render nothing, got %q", got)
	}
}

func TestRenderStatusIntegration(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{
		  "uptime":"2h15m3s","version":"0.4.2","listen":"127.0.0.1:15721",
		  "health":{"aqp":{"circuit_state":"closed","available":true}},
		  "counters":{"aqp":{"requests":1234,"failovers":0,"rate_limited_429":0,"failures":0,"last_request_at":1700000000}},
		  "quota":{"aqp":{"Account":"work","Plan":"plan","RemainingPct":0.62,"Windows":[{"Label":"Monthly","RemainingPct":0.62,"Ultimate":true,"ResetsAt":"2099-01-01T09:00:00Z"}]}},
		  "schedule":{"models":{"claude-sonnet":{"first":"aqp","ordered":[{"provider":"aqp","priority":1,"tier":"plan","surplus":12.3,"available":true}]}}}
		}`)
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":[{"provider":"aqp","model":"claude-sonnet","input":1200000,"output":450000,"cache_creation":200000,"cache_read":1100000,"requests":1234}]}`)
	})
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"lines":["line one","line two"]}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	listen := ts.Listener.Addr().String()

	out, err := renderStatus(listen, statusOpts{})
	if err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	for _, want := range []string{"model-proxy", "0.4.2", "2h15m3s", "aqp", "available", "claude-sonnet", "Monthly (ultimate)", "62%", "1.2M"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}

	// default: no logs section
	if strings.Contains(out, "line one") {
		t.Errorf("logs should be hidden by default")
	}

	// --logs includes a log line
	outLogs, err := renderStatus(listen, statusOpts{Logs: true, LogsN: 20})
	if err != nil {
		t.Fatalf("renderStatus logs: %v", err)
	}
	if !strings.Contains(outLogs, "line one") {
		t.Errorf("logs missing 'line one'")
	}

	// --json: valid merged object with status + tokens (+ logs when requested)
	outJSON, err := renderStatus(listen, statusOpts{JSON: true, Logs: true, LogsN: 2})
	if err != nil {
		t.Fatalf("renderStatus json: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal([]byte(outJSON), &merged); err != nil {
		t.Fatalf("json output invalid: %v\n%s", err, outJSON)
	}
	for _, k := range []string{"status", "tokens", "logs"} {
		if _, ok := merged[k]; !ok {
			t.Errorf("json missing key %q", k)
		}
	}
}

func TestRenderStatusDaemonDown(t *testing.T) {
	// bind then close → guaranteed connection refused
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	_, err = renderStatus(addr, statusOpts{})
	if err == nil {
		t.Fatal("want error for unreachable daemon")
	}
	if !strings.Contains(err.Error(), "cannot reach daemon") {
		t.Errorf("want 'cannot reach daemon', got %v", err)
	}
}

func TestRenderStatusWebDisabled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()
	_, err := renderStatus(ts.Listener.Addr().String(), statusOpts{})
	if err == nil || !strings.Contains(err.Error(), "web.enabled") {
		t.Fatalf("want web.enabled error, got %v", err)
	}
}

func TestRenderStatusJSONLogsError(t *testing.T) {
	// /api/logs returns non-200 with a JSON error body (e.g. no log_file configured).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"uptime":"1s","version":"x","listen":"127.0.0.1:1","health":{},"schedule":{"models":{}}}`)
	})
	mux.HandleFunc("/api/tokens", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"usage":[]}`)
	})
	mux.HandleFunc("/api/logs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":"no log_file configured"}`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// --json: the 404 error body must NOT be embedded as the "logs" field.
	outJSON, err := renderStatus(ts.Listener.Addr().String(), statusOpts{JSON: true, Logs: true, LogsN: 5})
	if err != nil {
		t.Fatalf("renderStatus json: %v", err)
	}
	var merged map[string]json.RawMessage
	if err := json.Unmarshal([]byte(outJSON), &merged); err != nil {
		t.Fatalf("json invalid: %v\n%s", err, outJSON)
	}
	if logs, ok := merged["logs"]; ok {
		t.Errorf("--json must omit logs when /api/logs is non-200, got logs=%s", logs)
	}
	if _, ok := merged["status"]; !ok {
		t.Errorf("status key missing from --json output")
	}

	// rendered path: the error body must not leak and the Logs section is omitted.
	out, err := renderStatus(ts.Listener.Addr().String(), statusOpts{Logs: true, LogsN: 5})
	if err != nil {
		t.Fatalf("renderStatus: %v", err)
	}
	if strings.Contains(out, "no log_file configured") {
		t.Errorf("error body leaked into rendered output:\n%s", out)
	}
	if strings.Contains(out, "Logs (last") {
		t.Errorf("Logs section should be omitted on non-200 /api/logs:\n%s", out)
	}
}

func TestParseStatusFlags(t *testing.T) {
	o := parseStatusFlags([]string{})
	if o.Logs || o.JSON || o.LogsN != 20 {
		t.Errorf("defaults wrong: %+v", o)
	}
	o = parseStatusFlags([]string{"--logs"})
	if !o.Logs || o.LogsN != 20 {
		t.Errorf("--logs default N wrong: %+v", o)
	}
	o = parseStatusFlags([]string{"--logs", "50"})
	if !o.Logs || o.LogsN != 50 {
		t.Errorf("--logs 50 wrong: %+v", o)
	}
	o = parseStatusFlags([]string{"--json"})
	if !o.JSON {
		t.Errorf("--json not set")
	}
	o = parseStatusFlags([]string{"--logs=50"})
	if !o.Logs || o.LogsN != 50 {
		t.Errorf("--logs=50 wrong: %+v", o)
	}
	o = parseStatusFlags([]string{"--logs=0"})
	if !o.Logs || o.LogsN != 20 { // non-positive value falls back to the default
		t.Errorf("--logs=0 should keep default N=20: %+v", o)
	}
	// --config must be skipped (configPath handles it) and not swallow --logs
	o = parseStatusFlags([]string{"--config", "x.yaml", "--logs"})
	if !o.Logs {
		t.Errorf("--config swallowed --logs: %+v", o)
	}
}
