package main

import (
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

func TestFormatClockTime(t *testing.T) {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, now.Location())
	if got := formatClockTime(today); got != "09:30" {
		t.Errorf("today = %q, want 09:30", got)
	}
	other := time.Date(2024, 1, 2, 9, 30, 0, 0, now.Location())
	if got := formatClockTime(other); got != "01-02 09:30" {
		t.Errorf("other-day = %q, want 01-02 09:30", got)
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
