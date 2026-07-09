# `serve status` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `model-proxy serve status` one-shot CLI command that fetches the same data the Web UI's Status tab shows and renders it optimized for the terminal.

**Architecture:** Reuse the daemon's existing `/api/status`, `/api/tokens`, `/api/logs` HTTP endpoints (no server-side changes). A thin CLI wrapper `cmdServeStatus` parses flags and exits; a pure core `renderStatus(listen, opts)` does the GETs + rendering (testable via `httptest`). Section renderers are pure functions of decoded structs.

**Tech Stack:** Go 1.26 (stdlib only — `net/http`, `encoding/json`, `testing` + `httptest`). Hand-rolled CLI dispatch (no cobra). Existing helpers reused: `pad`, `truncate`, `progressBar`, `cBold`/`cDim`/`cGreen`/`cRed`/`cYellow`/`cGray`.

## Global Constraints

- All `go` commands run from `model-proxy/` (the Go module subdir): `cd model-proxy && go ...`.
- Tests are white-box (`package main`), stdlib `testing` + `httptest` only — **no testify**.
- **Coverage baseline 80%** per package, enforced by `scripts/cover.sh` — run it before finishing. The thin CLI wrapper (`cmdServeStatus`) is intentionally not unit-tested (matches the codebase convention for CLI entry points like `cmdSchedule`); the pure core + every renderer IS tested.
- `go test ./...`, `go test -race ./...`, `go vet ./...`, `gofmt -l .` must all be clean.
- Color in tests: when stdout is not a tty, `cGreen("x")` returns `"x"` (color auto-disabled). **Assert visible text, not ANSI codes.**
- **Pad-before-color rule:** ANSI escape codes break width alignment. When a cell is both colored and column-aligned, `pad`/`%-Ns` the raw text first, then wrap in color (e.g. `cGray(pad(t.Tier, 13))`).
- Working branch: `serve-status` (already created; spec committed).

---

## File Structure

- **Create** `model-proxy/serve_status.go` — one focused file: response structs, `statusOpts`, pure helpers (`compactNum`, `formatClock`, `formatClockTime`, `plural`), section renderers (`renderProviders`, `renderSchedule`, `renderQuota`, `renderTokens`, `renderLogs`), `appendSection`, `statusGet`, `renderStatus`, `parseStatusFlags`, `cmdServeStatus`.
- **Create** `model-proxy/serve_status_test.go` — white-box tests for every pure/renderer function + `renderStatus` integration (httptest) + error paths + flag parsing.
- **Modify** `model-proxy/daemon.go` — one `case "status":` in the `cmdServe` switch.
- **Modify** `model-proxy/main.go` — `usage` const + `cmdHelp["serve"]` text.
- **Modify** `model-proxy/README.md` — one line under the serve commands block.

---

## Task 1: Foundation — response structs + pure helpers

**Files:**
- Create: `model-proxy/serve_status.go`
- Create: `model-proxy/serve_status_test.go`

**Interfaces:**
- Produces: `statusOpts`, `statusHealth`, `statusCounters`, `statusWindow`, `statusQuota`, `statusOrdered`, `statusPool`, `statusRoute`, `statusSchedule`, `statusResp`, `tokenEntry`, `tokensResp`, `logsResp` types; helpers `compactNum(uint64) string`, `formatClock(int64) string`, `formatClockTime(time.Time) string`, `plural(int, string, string) string`.

- [ ] **Step 1: Write the failing tests** (`serve_status_test.go`)

```go
package main

import (
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestCompactNum|TestFormatClock|TestPlural' .`
Expected: FAIL — `undefined: compactNum` (and the others).

- [ ] **Step 3: Write the structs + helpers** (`serve_status.go`)

```go
package main

import (
	"fmt"
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
	CircuitState     string `json:"circuit_state"`               // closed | open | half_open
	Available        bool   `json:"available"`
	CircuitUntil     string `json:"circuit_until,omitempty"`     // RFC3339, only when in the future
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestCompactNum|TestFormatClock|TestPlural' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): add response structs + pure number/time helpers"
```

---

## Task 2: `renderProviders` — health + counters table

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`

**Interfaces:**
- Consumes: `statusResp`, `statusHealth`, `statusCounters` (Task 1); `pad(s string, n int) string` (`models.go`); `cDim`/`cGreen`/`cRed`/`cYellow` (`color.go`); `compactNum`, `formatClock` (Task 1).
- Produces: `func renderProviders(st *statusResp) string`.

- [ ] **Step 1: Write the failing test** (append to `serve_status_test.go`)

> Add `"strings"` to `serve_status_test.go`'s import block (now uses `strings.Contains`/`Index`). Imports are now: `strings, testing, time`.

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestRenderProviders .`
Expected: FAIL — `undefined: renderProviders`.

- [ ] **Step 3: Implement** (append to `serve_status.go`)

> Add `"sort"` to `serve_status.go`'s import block (`renderProviders` uses `sort.Strings`). Imports are now: `fmt, sort, strconv, strings, time`.

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestRenderProviders .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): render providers health + counters table"
```

---

## Task 3: `renderSchedule` — per-route provider chains

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`

**Interfaces:**
- Consumes: `statusResp`, `statusRoute` (Task 1); `pad`, `cBold`/`cGreen`/`cRed`/`cYellow`/`cGray`/`cDim`; `plural` (Task 1).
- Produces: `func renderSchedule(st *statusResp) string`.

- [ ] **Step 1: Write the failing test** (append to `serve_status_test.go`)

```go
func TestRenderSchedule(t *testing.T) {
	st := &statusResp{
		Schedule: statusSchedule{
			Models: map[string]statusRoute{
				"claude-sonnet": {
					First: "aqp",
					Ordered: []statusOrdered{
						{Provider: "aqp", Priority: 1, Tier: "plan", Surplus: 12.3, Available: true},
						{Provider: "codex", Priority: 1, Tier: "plan", Surplus: 8.1, Available: false},
					},
					Sticky:   "aqp",
					DwellRem: 320,
				},
			},
		},
	}
	out := renderSchedule(st)
	for _, want := range []string{"Schedule", "claude-sonnet", "aqp", "codex", "surplus +12.30", "p1", "(unavailable)", "sticky:", "320s dwell left"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestRenderScheduleEmpty(t *testing.T) {
	if got := renderSchedule(&statusResp{}); got != "" {
		t.Errorf("empty schedule should render nothing, got %q", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run 'TestRenderSchedule' .`
Expected: FAIL — `undefined: renderSchedule`.

- [ ] **Step 3: Implement** (append to `serve_status.go`)

```go
// renderSchedule renders the per-route provider chains. Mirrors the standalone
// `schedule` command's output (first-choice, ordered list, sticky, pools).
func renderSchedule(st *statusResp) string {
	models := st.Schedule.Models
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%d %s)\n", cBold("Schedule"), len(names), plural(len(names), "route", "routes"))
	for _, m := range names {
		ri := models[m]
		fmt.Fprintf(&b, "  %s → %s\n", cBold(m), cGreen(ri.First))
		for _, pool := range ri.Pools {
			fmt.Fprintf(&b, "      %s %s (%d accounts, %d available)\n",
				cDim("pool:"), cBold(pool.Parent), pool.Accounts, pool.Available)
		}
		for _, t := range ri.Ordered {
			extra := ""
			if !t.Available {
				extra += " " + cRed("(unavailable)")
			}
			if t.Peak {
				extra += " " + cYellow("peak")
			}
			fmt.Fprintf(&b, "      %s %s  surplus %+.2f  p%d%s\n",
				pad(t.Provider, 14), cGray(pad(t.Tier, 13)), t.Surplus, t.Priority, extra)
		}
		if ri.Sticky != "" {
			dwell := ""
			if ri.DwellRem > 0 {
				dwell = fmt.Sprintf(", %.0fs dwell left", ri.DwellRem)
			}
			fmt.Fprintf(&b, "      %s%s%s\n", cDim("sticky: "), ri.Sticky, cDim(dwell))
		}
		b.WriteString("\n")
	}
	return b.String()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run 'TestRenderSchedule' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): render schedule provider chains"
```

---

## Task 4: `renderQuota` — per-provider quota windows with bars

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`

**Interfaces:**
- Consumes: `statusResp`, `statusQuota`, `statusWindow` (Task 1); `pad`, `cBold`/`cDim`; `progressBar(pct, width int) string` (`main.go` — `pct` is the **used** percentage 0-100); `formatClockTime` (Task 1).
- Produces: `func renderQuota(st *statusResp) string`.

- [ ] **Step 1: Write the failing test** (append to `serve_status_test.go`)

```go
func TestRenderQuota(t *testing.T) {
	st := &statusResp{
		Quota: map[string]statusQuota{
			"aqp": {
				Account: "work", Plan: "plan",
				Windows: []statusWindow{
					{Label: "Monthly", RemainingPct: 0.62, Ultimate: true},
					{Label: "5h tokens", RemainingPct: 0.88, Short: true},
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
	for _, want := range []string{"work", "plan", "Monthly (ultimate)", "62%", "5h tokens (short)", "88%", "resets"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run 'TestRenderQuota' .`
Expected: FAIL — `undefined: renderQuota`.

- [ ] **Step 3: Implement** (append to `serve_status.go`)

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run 'TestRenderQuota' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): render quota windows with progress bars"
```

---

## Task 5: `renderTokens` + `renderLogs`

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`

**Interfaces:**
- Consumes: `tokensResp`, `tokenEntry`, `logsResp` (Task 1); `cBold`/`cDim`; `compactNum`, `plural` (Task 1).
- Produces: `func renderTokens(t *tokensResp) string`, `func renderLogs(l *logsResp) string`.

- [ ] **Step 1: Write the failing tests** (append to `serve_status_test.go`)

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestRenderTokens|TestRenderLogs' .`
Expected: FAIL — `undefined: renderTokens` / `renderLogs`.

- [ ] **Step 3: Implement** (append to `serve_status.go`)

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestRenderTokens|TestRenderLogs' .`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): render tokens table + logs section"
```

---

## Task 6: `statusGet` + `renderStatus` — fetch, decode, assemble, JSON path

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`

**Interfaces:**
- Consumes: all Task 1-5 renderers + structs; `truncate(s string, n int) string` (`gateway.go`); `cBold`/`cDim`.
- Produces: `func statusGet(base, path string) (body []byte, status int, err error)`, `func renderStatus(listen string, opts statusOpts) (string, error)`, `func appendSection(b *strings.Builder, s string)`.

- [ ] **Step 1: Write the failing tests** (append to `serve_status_test.go`)

> Add `"encoding/json"`, `"fmt"`, `"net"`, `"net/http"`, `"net/http/httptest"` to `serve_status_test.go`'s import block. Imports are now: `encoding/json, fmt, net, net/http, net/http/httptest, strings, testing, time`.

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test -run 'TestRenderStatus' .`
Expected: FAIL — `undefined: renderStatus` / `statusGet`.

- [ ] **Step 3: Implement** (append to `serve_status.go`)

> Add `"encoding/json"`, `"io"`, `"net/http"` to `serve_status.go`'s import block. Imports are now: `encoding/json, fmt, io, net/http, sort, strconv, strings, time`.

```go
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test -run 'TestRenderStatus' .`
Expected: PASS (all three).

- [ ] **Step 5: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go
git commit -m "feat(serve-status): fetch + render status, json path, error handling"
```

---

## Task 7: CLI wrapper + dispatch wiring + help/usage + README

**Files:**
- Modify: `model-proxy/serve_status.go`
- Modify: `model-proxy/serve_status_test.go`
- Modify: `model-proxy/daemon.go`
- Modify: `model-proxy/main.go`
- Modify: `model-proxy/README.md`

**Interfaces:**
- Consumes: `renderStatus`, `statusOpts` (Task 6); `configPath(args) string`, `LoadConfig(path) (*Config, error)` (`main.go`); `cRed` (`color.go`).
- Produces: `func parseStatusFlags(args []string) statusOpts`, `func cmdServeStatus(args []string)`. Wires the `serve status` subcommand.

- [ ] **Step 1: Write the failing test** (append to `serve_status_test.go`)

```go
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
	// --config must be skipped (configPath handles it) and not swallow --logs
	o = parseStatusFlags([]string{"--config", "x.yaml", "--logs"})
	if !o.Logs {
		t.Errorf("--config swallowed --logs: %+v", o)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test -run TestParseStatusFlags .`
Expected: FAIL — `undefined: parseStatusFlags`.

- [ ] **Step 3: Implement the wrapper** (append to `serve_status.go`)

> Add `"log"`, `"os"` to `serve_status.go`'s import block. Final imports: `encoding/json, fmt, io, log, net/http, os, sort, strconv, strings, time`.

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd model-proxy && go test -run TestParseStatusFlags .`
Expected: PASS.

- [ ] **Step 5: Wire the subcommand** (`daemon.go`)

In `cmdServe`, change the comment and add the `status` case. Edit the block so it reads:

```go
	// Check for subcommand (daemon | stop | reload | status).
	sub := positional(args)
	switch sub {
	case "daemon":
		sa := parseServeArgs(args)
		if err := daemonize(sa); err != nil {
			log.Fatal(err)
		}
	case "stop":
		cmdStop(args)
	case "reload":
		cmdReload(args)
	case "status":
		cmdServeStatus(args)
	default:
		// No subcommand — foreground serve.
		sa := parseServeArgs(args)
		runProxy(sa)
	}
```

(The `case "status": cmdServeStatus(args)` line is the only addition; update the comment text from `(daemon | stop)` to `(daemon | stop | reload | status)`.)

- [ ] **Step 6: Update help + usage** (`main.go`)

In the `usage` const, after the `serve reload` line, add:

```go
  serve status         Show running daemon status (providers/schedule/quota/tokens)
```

In `cmdHelp["serve"]`, in the `Subcommands:` block, after the `reload` line add:

```go
  status    Show running daemon status (providers, schedule, quota, tokens).
```

- [ ] **Step 7: Update README** (`model-proxy/README.md`)

After line 91 (`model-proxy serve reload ...`), add:

```
model-proxy serve status           # 运行状态（providers/路由/配额/token）
```

- [ ] **Step 8: Build + full test + lint + coverage**

```bash
cd model-proxy && go build ./... \
  && go test ./... \
  && go vet ./... \
  && gofmt -l . \
  && bash scripts/cover.sh
```
Expected: build OK; all tests PASS; `gofmt -l .` prints nothing; `scripts/cover.sh` reports main ≥ 80% (and does not exit non-zero).

- [ ] **Step 9: Commit**

```bash
cd model-proxy && gofmt -w serve_status.go serve_status_test.go
git add serve_status.go serve_status_test.go daemon.go main.go README.md
git commit -m "feat(serve): add 'serve status' terminal status command"
```

---

## Verification (manual smoke, after Task 7)

With a daemon running (`model-proxy serve daemon`):

- `model-proxy serve status` → prints header + Providers + Schedule + Quota + Tokens (no logs).
- `model-proxy serve status --logs` → same plus a Logs (last 20) section.
- `model-proxy serve status --logs 5` → Logs (last 5).
- `model-proxy serve status --json | jq .status.version` → the version string.
- `model-proxy serve stop` (stop the daemon), then `model-proxy serve status` → `✗ cannot reach daemon at <listen>: …` exit 1.
- `model-proxy serve -h` → help lists `status`.

## Self-Review notes

- Spec coverage: header/Providers/Schedule/Quota/Tokens/Logs → Tasks 2-5 + 6. Flags `--logs [N]`/`--json`/`--config` → Task 7. Error paths (down / web-disabled / non-200 / parse) → Task 6. Dispatch + help + README → Task 7. No spec section unaddressed.
- Type consistency: `statusOpts{Logs, LogsN, JSON}` used identically in Tasks 6 & 7; `renderStatus(listen, statusOpts)` signature matches between Task 6 (defines) and Task 7 (calls via `cfg.Listen`); `appendSection` defined and used in Task 6 only.
- **Import blocks are per-task exact** (Go fails on unused imports): Task 1 = `fmt, strconv, strings, time`; Task 2 adds `sort` (go) + `strings` (test); Task 6 adds `encoding/json, io, net/http` (go) + `encoding/json, fmt, net, net/http, net/http/httptest` (test); Task 7 adds `log, os` (go). Each task's step states the resulting block.
- No placeholders; every code step shows complete code; every command shows expected output.
