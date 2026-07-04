# Quota-Aware Scheduling Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the proxy schedule providers by polled remaining quota (balanced, cache-preserving, peak-aware) and use pay-as-you-go only as a strict last resort.

**Architecture:** A background poller periodically calls each provider's existing usage endpoint, normalizes the result into a `QuotaSnapshot`, and persists it to `~/.model-proxy/quota_state.json`. `schedule()` reads the snapshot and ranks available providers by `(billing tier, effective_remaining desc, priority asc)`, where `effective_remaining = RemainingPct / peak_multiplier`. Sticky routing keeps a conversation on one provider for `sticky_dwell`, then switches only if another plan provider is ahead by ≥ `quota_switch_margin`. Circuit-breaker / rate-limit / half-open / failover mechanics are unchanged.

**Tech Stack:** Go (stdlib only), `gopkg.in/yaml.v3`, white-box tests with `net/http/httptest` + `testing` (no testify). Module root: `model-proxy/` subdirectory — run all `go` commands from there.

## Global Constraints

- All tests white-box (`package main`), stdlib `testing` + `httptest` only — **no testify**.
- Run `go` commands from `/Users/zhiyong.liu/model-proxy/model-proxy`.
- Build: `go build -o model-proxy .` · Lint: `go vet ./...` · Test all: `go test ./...` (~24s).
- Logging hygiene: mask cookies with `mask()`; log auth bodies by length only; color off when logging to a file (existing rules — preserve them).
- Lock ordering (must be consistent to avoid deadlock): `healthMu` → `quotaMu`. The poller takes only `quotaMu`. `schedule` takes `healthMu`, then snapshots quota under a brief `quotaMu` RLock at the top (released before sorting) — **never** holds both during the sort.
- `provider/` package must not import package `main` (existing boundary — preserve it).
- The `usage` CLI display output stays **byte-identical** to today (regression-tested).

**Spec:** `docs/superpowers/specs/2026-07-05-quota-aware-scheduling-design.md` — read it before starting.

---

## File Map

| File | Responsibility | Action |
|---|---|---|
| `provider/provider.go` | `BillingClass`, `QuotaWindow`, `QuotaSnapshot`, `QuotaDetail` types; `Quota()` on interface; `QuotaFn` + `QuotaOrUnknown()` on `Config`; `BindingRemaining` helper. | Modify |
| `provider/{compass,codex,zhipu,deepseek,volcengine,static}.go` | One-liner `Quota()` delegating to `cfg.QuotaOrUnknown()`. | Modify |
| `config.go` | `PeakSegment`/`PeakConfig` (custom unmarshal); `Provider.PeakHours` → `PeakConfig`; `Provider.Billing string`; `Provider.peakMultiplier(now)`; `Scheduling.QuotaPollInterval`/`QuotaSwitchMargin` + accessors; `validate()`. | Modify |
| `main.go` | `parse{Zhipu,Codex,Volcengine,Deepseek,Compass}Quota` pure parsers; `fetch{...}Quota` HTTP wrappers; refactor `show*Usage` to consume them. | Modify |
| `provider_wire.go` | Wire `pcfg.QuotaFn` per provider in `buildProviders`. | Modify |
| `quota.go` | `quotaTracker`: in-memory state, poll loop, atomic persistence, boot load, staleness, `refreshOne`. | Create |
| `proxy.go` | `Proxy.quota` field; `NewProxy` starts tracker; `reload` keeps it; `recordRateLimit` triggers `refreshOne`; rewrite `schedule()`; add `billingClass`/`effectiveRemaining` helpers. | Modify |
| `defaults.go` | Update embedded template (deepseek `billing`, multi-segment `peak_hours`, scheduling fields). | Modify |
| `config.yaml` | Live config: add deepseek `billing`, multi-segment zhipu `peak_hours`, scheduling fields. | Modify |
| `provider/quota_test.go` | `BindingRemaining` tests. | Create |
| `config_test.go` | `PeakConfig` unmarshal (3 shapes), `peakMultiplier`, billing, defaults. | Modify |
| `quota_test.go` | Parsers, tracker persistence/staleness/pollOne, display output. | Create |
| `proxy_quota_test.go` | `schedule()` quota ranking + sticky switch + peak + payg. | Create |
| `proxy_routing_test.go` | Add `Quota()` to `testProv`. | Modify |
| `AGENTS.md`, `README.md`, `CLAUDE.md` | Document quota-aware scheduling. | Modify |

---

## Task 1: Quota model types + interface method + binding helper

**Files:**
- Modify: `provider/provider.go`
- Modify: `provider/compass.go`, `provider/codex.go`, `provider/zhipu.go`, `provider/deepseek.go`, `provider/volcengine.go`, `provider/static.go`
- Modify: `proxy_routing_test.go` (add `Quota()` to `testProv`)
- Test: `provider/quota_test.go` (Create)

**Interfaces:**
- Produces (used by later tasks): `provider.BillingClass` (`BillingUnknown|BillingPlan|BillingPayG`); `provider.QuotaSnapshot{Billing, RemainingPct, Account, Plan, Level, Windows, Notes, AsOf, Err}`; `provider.QuotaWindow{Label, Kind, Used, Total, RemainingPct, ResetsAt, Details}`; `provider.QuotaDetail{Label, Used}`; `provider.BindingRemaining(windows) float64`; method `Provider.Quota() (*QuotaSnapshot, error)`; `provider.Config.QuotaFn func() (*QuotaSnapshot, error)`; `provider.Config.QuotaOrUnknown() (*QuotaSnapshot, error)`.

- [ ] **Step 1: Write the failing test** — `provider/quota_test.go`

```go
package provider

import "testing"

func TestBindingRemaining(t *testing.T) {
	cases := []struct {
		name    string
		windows []QuotaWindow
		want    float64
	}{
		{"empty", nil, -1},
		{"single", []QuotaWindow{{RemainingPct: 0.8}}, 0.8},
		{"min wins", []QuotaWindow{{RemainingPct: 0.8}, {RemainingPct: 0.3}, {RemainingPct: 0.9}}, 0.3},
		{"skip unmeasured", []QuotaWindow{{RemainingPct: -1}, {RemainingPct: 0.5}}, 0.5},
		{"all unmeasured", []QuotaWindow{{RemainingPct: -1}, {RemainingPct: -1}}, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BindingRemaining(tc.windows); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestQuotaOrUnknown(t *testing.T) {
	cfg := &Config{}
	got, err := cfg.QuotaOrUnknown()
	if err != nil {
		t.Fatalf("nil QuotaFn should not error, got %v", err)
	}
	if got.Billing != BillingUnknown {
		t.Errorf("nil QuotaFn → Billing %v, want BillingUnknown", got.Billing)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test ./provider/ -run TestBindingRemaining -v`
Expected: FAIL — `BindingRemaining undefined` / `QuotaSnapshot undefined`.

- [ ] **Step 3: Implement types + helpers in `provider/provider.go`**

Add to the top of `provider/provider.go` (after the imports, before `Authenticator`):

```go
// BillingClass tiers providers for scheduling: plan providers are ranked by
// remaining quota, unknown ones by priority, pay-as-you-go is strict last resort.
type BillingClass int

const (
	BillingUnknown BillingClass = iota // can't measure (no AK/SK, not logged in, poll failed, stale)
	BillingPlan                        // Coding/Agent Plan: windowed quotas
	BillingPayG                        // pay-as-you-go: strict last-resort
)

// QuotaDetail is one line of a per-window breakdown (per-model tokens, per-tool time).
type QuotaDetail struct {
	Label string
	Used  float64
}

// QuotaWindow is one normalized quota window (5h / weekly / monthly / spend / balance).
type QuotaWindow struct {
	Label        string        // "5h tokens", "Weekly tokens", "Monthly time", "Spend", "Balance"
	Kind         string        // "tokens" | "time" | "money"
	Used         float64
	Total        float64
	RemainingPct float64       // 0..1; -1 if unmeasured (e.g. balance-only)
	ResetsAt     time.Time     // zero if unknown
	Details      []QuotaDetail
	DetailLabel  string        // breakdown header for display ("By model", "By MCP tool"); "" omits
}

// QuotaSnapshot is the normalized, polled quota for one provider. It carries
// enough detail for both scheduling (Billing, RemainingPct) and the `usage`
// display (Account, Plan, Level, Windows, Notes).
type QuotaSnapshot struct {
	Billing      BillingClass
	RemainingPct float64 // binding min over windows; -1 if unknown
	Account      string
	Plan         string
	Level        string
	Windows      []QuotaWindow
	Notes        []string // provider-specific status lines for display
	AsOf         time.Time
	Err          string
}

// BindingRemaining returns the minimum RemainingPct across windows whose
// RemainingPct >= 0 (the binding constraint). Returns -1 if none measured.
func BindingRemaining(windows []QuotaWindow) float64 {
	min := -1.0
	for _, w := range windows {
		if w.RemainingPct < 0 {
			continue
		}
		if min < 0 || w.RemainingPct < min {
			min = w.RemainingPct
		}
	}
	return min
}
```

Add `time` to the import block of `provider/provider.go`:
```go
import (
	"fmt"
	"net/http"
	"time"
)
```

Add `Quota()` to the `Provider` interface (after `FetchModels`):
```go
	Quota() (*QuotaSnapshot, error)
```

Add `QuotaFn` to `Config` and the `QuotaOrUnknown` helper (after `FetchModelsFn`):
```go
	QuotaFn       func() (*QuotaSnapshot, error) // for Quota (structured quota for scheduling + display)
}

// QuotaOrUnknown returns cfg.QuotaFn()'s snapshot, or a BillingUnknown snapshot
// when QuotaFn is unset (static / not-yet-wired providers) — never panics.
func (c *Config) QuotaOrUnknown() (*QuotaSnapshot, error) {
	if c.QuotaFn == nil {
		return &QuotaSnapshot{Billing: BillingUnknown}, nil
	}
	return c.QuotaFn()
}
```
(Note: the `}` after `QuotaFn` closes the `Config` struct — insert `QuotaFn` as the last field before the existing closing brace.)

- [ ] **Step 4: Add `Quota()` to every provider implementation**

In each of `provider/compass.go`, `provider/codex.go`, `provider/zhipu.go`, `provider/deepseek.go`, `provider/volcengine.go`, `provider/static.go`, add this method (next to the existing `Usage()`):

```go
func (p *XxxProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
```
(replace `XxxProvider` with the actual receiver type: `CompassProvider`, `CodexProvider`, `ZhipuProvider`, `DeepSeekProvider`, `VolcengineProvider`, `StaticProvider`). For `StaticProvider` the receiver field is `cfg` already.

- [ ] **Step 5: Add `Quota()` to `testProv` in `proxy_routing_test.go`**

After the existing `FetchModels` method (around `proxy_routing_test.go:158`):
```go
func (t *testProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
```
Add `"model-proxy/provider"` to that file's imports if not present.

- [ ] **Step 6: Run all tests to verify they pass**

Run: `cd model-proxy && go test ./... `
Expected: PASS (the interface now satisfies all implementations + `testProv`; `BindingRemaining`/`QuotaOrUnknown` pass).

- [ ] **Step 7: Commit**

```bash
cd model-proxy
git add provider/provider.go provider/compass.go provider/codex.go provider/zhipu.go provider/deepseek.go provider/volcengine.go provider/static.go provider/quota_test.go proxy_routing_test.go
git commit -m "feat(quota): add QuotaSnapshot model + Provider.Quota() interface"
```

---

## Task 2: Config — multi-segment peak + billing + scheduling fields

**Files:**
- Modify: `config.go`
- Modify: `config_test.go`
- (defaults.go + config.yaml updated in Step 9)

**Interfaces:**
- Produces: `PeakSegment{Window, Multiplier}`; `PeakConfig []PeakSegment` with custom YAML unmarshaling (accepts `string`, `[]string`, `[]map`); `Provider.PeakHours` is now `PeakConfig`; `Provider.Billing string`; `Provider.peakMultiplier(now time.Time) float64`; `Provider.inPeak(now) bool` (kept, = `peakMultiplier(now) > 1`); `Scheduling.QuotaPollInterval string`; `Scheduling.QuotaSwitchMargin int`; `Scheduling.pollInterval() time.Duration`; `Scheduling.switchMargin() float64`.

- [ ] **Step 1: Write the failing test** — append to `config_test.go`

```go
func TestPeakConfig_Unmarshal(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want []PeakSegment
	}{
		{"single string", `peak_hours: "09:00-18:00"`, []PeakSegment{{Window: "09:00-18:00"}}},
		{"list of strings", "peak_hours:\n  - \"09:00-12:00\"\n  - \"14:00-18:00\"", []PeakSegment{{Window: "09:00-12:00"}, {Window: "14:00-18:00"}}},
		{"list of maps", "peak_hours:\n  - {window: \"09:00-12:00\", multiplier: 2}\n  - {window: \"14:00-18:00\", multiplier: 3}", []PeakSegment{{Window: "09:00-12:00", Multiplier: 2}, {Window: "14:00-18:00", Multiplier: 3}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var wrap struct {
				PeakHours PeakConfig `yaml:"peak_hours"`
			}
			if err := yaml.Unmarshal([]byte(tc.yaml), &wrap); err != nil {
				t.Fatalf("yaml.Unmarshal: %v", err)
			}
			if len(wrap.PeakHours) != len(tc.want) {
				t.Fatalf("got %d segments, want %d", len(wrap.PeakHours), len(tc.want))
			}
			for i, want := range tc.want {
				got := wrap.PeakHours[i]
				if got.Window != want.Window {
					t.Errorf("seg %d Window: got %q, want %q", i, got.Window, want.Window)
				}
				if want.Multiplier != 0 && got.Multiplier != want.Multiplier {
					t.Errorf("seg %d Multiplier: got %v, want %v", i, got.Multiplier, want.Multiplier)
				}
			}
		})
	}
}

func TestProvider_PeakMultiplier(t *testing.T) {
	p := Provider{PeakHours: PeakConfig{
		{Window: "09:00-12:00", Multiplier: 2},
		{Window: "14:00-18:00", Multiplier: 3},
	}}
	parseHHMMRange("09:00-12:00") // ensure parser initialized
	at := func(h, m int) time.Time { return time.Date(2026, 7, 5, h, m, 0, 0, time.Local) }
	if got := p.peakMultiplier(at(10, 0)); got != 2 {
		t.Errorf("10:00 (in 09-12) mult=%v, want 2", got)
	}
	if got := p.peakMultiplier(at(15, 0)); got != 3 {
		t.Errorf("15:00 (in 14-18) mult=%v, want 3", got)
	}
	if got := p.peakMultiplier(at(13, 0)); got != 1 {
		t.Errorf("13:00 (no segment) mult=%v, want 1", got)
	}
}

func TestScheduling_QuotaDefaults(t *testing.T) {
	var s Scheduling
	if s.pollInterval() != 5*time.Minute {
		t.Errorf("default pollInterval=%v, want 5m", s.pollInterval())
	}
	if s.switchMargin() != 0.15 {
		t.Errorf("default switchMargin=%v, want 0.15", s.switchMargin())
	}
	s.QuotaSwitchMargin = 20
	if s.switchMargin() != 0.20 {
		t.Errorf("switchMargin(20)=%v, want 0.20", s.switchMargin())
	}
}
```
Add `"time"` and `"gopkg.in/yaml.v3"` to `config_test.go` imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test . -run 'TestPeakConfig_Unmarshal|TestProvider_PeakMultiplier|TestScheduling_QuotaDefaults' -v`
Expected: FAIL — `PeakSegment undefined`, `peakMultiplier undefined`, `pollInterval undefined`.

- [ ] **Step 3: Implement `PeakSegment` / `PeakConfig` in `config.go`**

Add after the `Provider` struct:
```go
// PeakSegment is one peak-hours window with its consumption multiplier.
type PeakSegment struct {
	Window     string  `yaml:"window"`
	Multiplier float64 `yaml:"multiplier"` // 0 → default (2.0) at validate time
}

// PeakConfig is a provider's set of peak segments. It unmarshals from three
// YAML shapes: a single string ("09:00-18:00"), a list of strings, or a list
// of {window, multiplier} maps.
type PeakConfig []PeakSegment

func (p *PeakConfig) UnmarshalYAML(value *yaml.Node) error {
	// Case 1: single string.
	var single string
	if value.Decode(&single) == nil && single != "" {
		*p = PeakConfig{{Window: single}}
		return nil
	}
	// Case 2/3: a sequence.
	var seq []yaml.Node
	if err := value.Decode(&seq); err != nil {
		return err
	}
	out := make(PeakConfig, 0, len(seq))
	for _, el := range seq {
		var s string
		if el.Decode(&s) == nil && s != "" {
			out = append(out, PeakSegment{Window: s})
			continue
		}
		var seg PeakSegment
		if err := el.Decode(&seg); err != nil {
			return err
		}
		out = append(out, seg)
	}
	*p = out
	return nil
}
```

- [ ] **Step 4: Change `Provider.PeakHours` type and add `Billing` + accessors**

In the `Provider` struct, replace:
```go
	PeakHours string `yaml:"peak_hours"`
```
with:
```go
	PeakHours PeakConfig `yaml:"peak_hours"`
	Billing   string     `yaml:"billing"` // "pay-as-you-go" | ""(plan, default)
```

Replace the `Provider.inPeak` method (the `func (p Provider) inPeak(now time.Time) bool` block) with:
```go
// peakMultiplier returns the multiplier of whichever peak segment `now` falls
// into (1.0 if none / no segments). Used to discount effective remaining quota.
func (p Provider) peakMultiplier(now time.Time) float64 {
	now = now.Local()
	m := now.Hour()*60 + now.Minute()
	for _, seg := range p.PeakHours {
		start, end, ok := parseHHMMRange(seg.Window)
		if !ok {
			continue
		}
		var inside bool
		if start <= end {
			inside = m >= start && m < end
		} else {
			inside = m >= start || m < end // wrap-around
		}
		if inside {
			if seg.Multiplier > 0 {
				return seg.Multiplier
			}
			return defaultPeakMultiplier
		}
	}
	return 1.0
}

// inPeak reports whether the provider is currently in any peak segment.
func (p Provider) inPeak(now time.Time) bool { return p.peakMultiplier(now) > 1.0 }

const defaultPeakMultiplier = 2.0
```

- [ ] **Step 5: Add scheduling fields + accessors**

In the `Scheduling` struct, add two fields (after `StickyDwell`):
```go
	QuotaPollInterval string `yaml:"quota_poll_interval"` // background poll cadence (default 5m)
	QuotaSwitchMargin int    `yaml:"quota_switch_margin"`  // switch if another plan provider's effective remaining beats current by ≥ this many pct points (default 15)
```
Add accessors next to the existing `dwell()`:
```go
func (s Scheduling) pollInterval() time.Duration {
	if d, err := time.ParseDuration(s.QuotaPollInterval); err == nil {
		return d
	}
	return 5 * time.Minute
}
func (s Scheduling) switchMargin() float64 {
	if s.QuotaSwitchMargin > 0 {
		return float64(s.QuotaSwitchMargin) / 100.0
	}
	return 0.15
}
```

- [ ] **Step 6: Update `validate()` for new shapes**

Replace the existing `peak_hours` validation block (the `if p.PeakHours != "" { ... }` inside the providers loop) with:
```go
		// peak_hours: each segment window must be a valid HH:MM-HH:MM range.
		for i, seg := range p.PeakHours {
			start, end, ok := parseHHMMRange(seg.Window)
			if !ok {
				return fmt.Errorf("provider %q: peak_hours segment %d %q is malformed — expected \"HH:MM-HH:MM\"", name, i, seg.Window)
			}
			if start == end {
				return fmt.Errorf("provider %q: peak_hours segment %d %q has zero-width window", name, i, seg.Window)
			}
			if seg.Multiplier < 0 {
				return fmt.Errorf("provider %q: peak_hours segment %d multiplier %v must be > 0", name, i, seg.Multiplier)
			}
		}
		// billing: only known values.
		if p.Billing != "" && p.Billing != "plan" && p.Billing != "pay-as-you-go" {
			return fmt.Errorf("provider %q: billing %q invalid — use \"plan\" or \"pay-as-you-go\"", name, p.Billing)
		}
```

- [ ] **Step 7: Run tests to verify they pass**

Run: `cd model-proxy && go test . -run 'TestPeakConfig_Unmarshal|TestProvider_PeakMultiplier|TestScheduling_QuotaDefaults' -v && go test . -run TestConfig`
Expected: PASS (new tests pass; existing config tests still pass — `peak_hours: "14:00-18:00"` in config.yaml still parses as 1 segment).

- [ ] **Step 8: Verify the live config still loads**

Run: `cd model-proxy && go test . -run 'TestConfig_ProviderBaseURLs|TestConfig_RouteTargets' -v`
Expected: PASS (config.yaml unchanged so far still validates).

- [ ] **Step 9: Update `defaults.go` template + `config.yaml`**

In `defaults.go`, in the `deepseek:` block add `billing: pay-as-you-go` under `provider_id: deepseek`. In the `zhipu:` block replace `peak_hours: "14:00-18:00"` (if present in the template) — the template currently omits it; add a commented multi-segment example:
```yaml
    # peak_hours:                       # multi-segment, per-segment multiplier
    #   - {window: "09:00-12:00", multiplier: 2}
    #   - {window: "14:00-18:00", multiplier: 2}
```
In the `scheduling:` block of the template add:
```yaml
  quota_poll_interval: 5m     # background quota poll cadence
  quota_switch_margin: 15     # switch provider if another's effective remaining beats current by ≥ this many pct points
```

In `config.yaml` (the live config): under `deepseek:` add `billing: pay-as-you-go` (indented under the provider, next to `provider_id`). Change zhipu's `peak_hours: "14:00-18:00"` to:
```yaml
    peak_hours:
      - {window: "09:00-12:00", multiplier: 2}
      - {window: "14:00-18:00", multiplier: 2}
```
In the `scheduling:` block add the same `quota_poll_interval` / `quota_switch_margin` lines.

- [ ] **Step 10: Run full test suite + vet**

Run: `cd model-proxy && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 11: Commit**

```bash
cd model-proxy
git add config.go config_test.go defaults.go config.yaml
git commit -m "feat(quota): multi-segment peak_hours + billing + scheduling quota fields"
```

---

## Task 3: zhipu quota parser + display refactor (establishes the pattern)

**Files:**
- Modify: `main.go` (extract zhipu parse from `showGenericUsage`; add `parseZhipuQuota`, `fetchZhipuQuota`)
- Modify: `provider_wire.go` (wire `QuotaFn` for zhipu — done here as part of the pattern)
- Test: `quota_test.go` (Create)

**Interfaces:**
- Produces: `parseZhipuQuota(body []byte, account string) (*provider.QuotaSnapshot, error)`; `fetchZhipuQuota(cfg *Config, name string, prov Provider) (*provider.QuotaSnapshot, error)`.
- Consumes: `provider.QuotaSnapshot`, `provider.BindingRemaining` (Task 1).

- [ ] **Step 1: Write the failing test** — `quota_test.go`

```go
package main

import (
	"strings"
	"testing"
)

func TestParseZhipuQuota(t *testing.T) {
	body := []byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[
		{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000,"usageDetails":[{"modelCode":"glm-5.2","usage":30000},{"modelCode":"glm-5.1","usage":10000}]},
		{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000},
		{"type":"TIME_LIMIT","unit":5,"percentage":10,"nextResetTime":1750000000000,"usage":3600,"currentValue":360,"remaining":3240}
	]}}`)
	s, err := parseZhipuQuota(body, "alice@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Level != "GLM Coding Plan" {
		t.Errorf("Level=%q", s.Level)
	}
	// binding = min(5h rem 0.6, weekly rem 0.3) — TIME_LIMIT excluded.
	if s.RemainingPct != 0.3 {
		t.Errorf("RemainingPct=%v, want 0.3 (weekly is binding; TIME_LIMIT excluded)", s.RemainingPct)
	}
	if len(s.Windows) != 3 {
		t.Fatalf("got %d windows, want 3", len(s.Windows))
	}
	// 5h window has per-model details.
	var w5h *provider.QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Label == "5h tokens" {
			w5h = &s.Windows[i]
		}
	}
	if w5h == nil || len(w5h.Details) != 2 {
		t.Errorf("5h window details: %+v", w5h)
	}
}

func TestParseZhipuQuota_NotZhipu(t *testing.T) {
	// Non-zhipu JSON → returns nil snapshot (caller falls back to model list).
	s, err := parseZhipuQuota([]byte(`{"object":"list","data":[]}`), "")
	if err == nil && s != nil {
		t.Fatalf("expected nil snapshot for non-zhipu body, got %+v", s)
	}
}
```
Add `"model-proxy/provider"` to `main.go`'s imports if not present (it already imports it via proxy.go's package — `main` package shares imports per-file; add to `main.go` import block).

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test . -run TestParseZhipuQuota -v`
Expected: FAIL — `parseZhipuQuota undefined`.

- [ ] **Step 3: Implement `parseZhipuQuota` + `fetchZhipuQuota` in `main.go`**

Add (place near `showGenericUsage`):
```go
// parseZhipuQuota parses Zhipu BigModel's /api/monitor/usage/quota/limit body into
// a QuotaSnapshot. Returns (nil, nil) if the body isn't the zhipu quota format
// (caller falls back to the OpenAI model-list display). TIME_LIMIT windows are
// included for display but EXCLUDED from the binding RemainingPct (they're MCP
// tool quota, not LLM tokens).
func parseZhipuQuota(body []byte, account string) (*provider.QuotaSnapshot, error) {
	var z struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Limits []struct {
				Type          string `json:"type"`
				Unit          int    `json:"unit"`
				Number        int    `json:"number"`
				Percentage    int    `json:"percentage"`
				NextResetTime int64  `json:"nextResetTime"`
				Usage         *int   `json:"usage"`
				CurrentValue  *int   `json:"currentValue"`
				Remaining     *int   `json:"remaining"`
				UsageDetails  []struct {
					ModelCode string `json:"modelCode"`
					Usage     int    `json:"usage"`
				} `json:"usageDetails"`
			} `json:"limits"`
			Level string `json:"level"`
		} `json:"data"`
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(body, &z); err != nil {
		return nil, nil
	}
	if !z.Success || len(z.Data.Limits) == 0 {
		return nil, nil
	}
	s := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Account: account,
		Level:   z.Data.Level,
		Plan:    z.Data.Level,
		AsOf:    time.Now(),
	}
	var binding []provider.QuotaWindow // only TOKENS_LIMIT contribute to the binding min
	for _, l := range z.Data.Limits {
		w := provider.QuotaWindow{
			Label:        zhipuLimitLabel(l.Type, l.Unit),
			RemainingPct: float64(l.Percentage) / 100.0,
		}
		if l.Type == "TIME_LIMIT" {
			w.Kind = "time"
			w.DetailLabel = "By MCP tool"
		} else {
			w.Kind = "tokens"
			w.DetailLabel = "By model"
		}
		if l.CurrentValue != nil && l.Remaining != nil {
			w.Used = float64(*l.CurrentValue)
			w.Total = float64(*l.CurrentValue + *l.Remaining)
		}
		if l.NextResetTime > 0 {
			w.ResetsAt = time.UnixMilli(l.NextResetTime)
		}
		for _, ud := range l.UsageDetails {
			w.Details = append(w.Details, provider.QuotaDetail{Label: ud.ModelCode, Used: float64(ud.Usage)})
		}
		s.Windows = append(s.Windows, w)
		if l.Type == "TOKENS_LIMIT" {
			binding = append(binding, w)
		}
	}
	s.RemainingPct = provider.BindingRemaining(binding)
	return s, nil
}

// fetchZhipuQuota GETs the zhipu usage_url and returns the parsed snapshot.
func fetchZhipuQuota(cfg *Config, name string, prov Provider) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	for k, v := range prov.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	s, _ := parseZhipuQuota(body, "")
	if s == nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "not zhipu quota format"}, nil
	}
	return s, nil
}
```

- [ ] **Step 4: Refactor `showGenericUsage`'s zhipu branch to use the parser**

In `showGenericUsage`, replace the inline zhipu parse+display block (the `var zhipu struct{...}` declaration through the `return` at the end of the `if json.Unmarshal(body, &zhipu) == nil && zhipu.Success ...` block) with:
```go
	if s, _ := parseZhipuQuota(body, ""); s != nil {
		printQuotaSnapshot(s)
		return
	}
```
Then add a generic renderer (used by all providers' display):
```go
// printQuotaSnapshot renders a QuotaSnapshot for the `usage` CLI. Output mirrors
// the pre-refactor per-provider formatters.
func printQuotaSnapshot(s *provider.QuotaSnapshot) {
	for _, w := range s.Windows {
		pct := int(w.RemainingPct * 100)
		usedPct := 100 - pct
		bar := progressBar(usedPct, 16)
		pctStr := usageRatioColor(w.RemainingPct, 1, fmt.Sprintf("%d%% used", usedPct))
		resetStr := ""
		if !w.ResetsAt.IsZero() {
			dur := formatDuration(int(time.Until(w.ResetsAt) / time.Second))
			resetStr = cGray(" · resets " + dur + "(at " + formatResetAt(w.ResetsAt.UnixMilli()) + ")")
		}
		fmt.Printf("%s %s  %s%s\n", cDim(pad(w.Label+":", 18)), bar, pctStr, resetStr)
		if w.Total > 0 {
			fmt.Printf("%s %.0f used / %.0f total (%.0f remaining)\n",
				cDim(pad("Usage:", 18)), w.Used, w.Total, w.Total-w.Used)
		}
		if len(w.Details) > 0 && w.DetailLabel != "" {
			parts := make([]string, 0, len(w.Details))
			for _, d := range w.Details {
				parts = append(parts, fmt.Sprintf("%s: %.0f", d.Label, d.Used))
			}
			fmt.Printf("%s %s\n", cDim(pad(w.DetailLabel+":", 18)), cGray(strings.Join(parts, " · ")))
		}
	}
	for _, n := range s.Notes {
		fmt.Println(cDim(pad("", 18)) + n)
	}
}
```
(The header `Provider:` line is printed by the caller — see Step 6.) Note `progressBar(usedPct, 16)` matches today's `progressBar(pct, 16)` where the old `pct` was the *used* percentage.

- [ ] **Step 5: Run parser test to verify it passes**

Run: `cd model-proxy && go test . -run TestParseZhipuQuota -v`
Expected: PASS.

- [ ] **Step 6: Wire `QuotaFn` for zhipu in `provider_wire.go` / `buildProviders`**

In `proxy.go:buildProviders`, in the `case "zhipu":` block, add after the existing `UsageFn` line:
```go
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchZhipuQuota(cfg, name, prov) }
```
(`name` and `prov` are the loop variables in `buildProviders`; capture is correct because each iteration has its own `prov` via `prov := cfg.Providers[name]`.)

- [ ] **Step 7: Commit**

```bash
cd model-proxy
git add main.go quota_test.go proxy.go
git commit -m "feat(quota): zhipu parser + unified display (parseZhipuQuota)"
```

---

## Task 4: codex quota parser + display

**Files:**
- Modify: `main.go`, `proxy.go` (`buildProviders`).
- Test: `quota_test.go`.

**Interfaces:**
- Produces: `parseCodexQuota(body []byte, account, plan string) (*provider.QuotaSnapshot, error)`; `fetchCodexQuota(cfg *Config, prov Provider) (*provider.QuotaSnapshot, error)`.

- [ ] **Step 1: Write the failing test** — append to `quota_test.go`

```go
func TestParseCodexQuota(t *testing.T) {
	body := []byte(`{"email":"a@b.com","plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
		"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},
		"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},
		"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`)
	s, err := parseCodexQuota(body, "a@b.com", "pro")
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.Account != "a@b.com" || s.Plan != "pro" {
		t.Errorf("Account/Plan=%q/%q", s.Account, s.Plan)
	}
	// binding = min(primary rem 0.7, weekly rem 0.4, spend rem 0.75) = 0.4
	if s.RemainingPct != 0.4 {
		t.Errorf("RemainingPct=%v, want 0.4 (weekly binding)", s.RemainingPct)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test . -run TestParseCodexQuota -v`
Expected: FAIL — `parseCodexQuota undefined`.

- [ ] **Step 3: Implement `parseCodexQuota` + `fetchCodexQuota` in `main.go`**

```go
// parseCodexQuota parses codex /backend-api/wham/usage into a QuotaSnapshot.
// Windows: primary(5h) + secondary(weekly) + spend(monthly $). Credits/rate-limit
// status go to Notes for display.
func parseCodexQuota(body []byte, account, plan string) (*provider.QuotaSnapshot, error) {
	var u struct {
		Email    string `json:"email"`
		PlanType string `json:"plan_type"`
		Credits  *struct {
			HasCredits bool    `json:"has_credits"`
			Unlimited  bool    `json:"unlimited"`
			Balance    *string `json:"balance"`
		} `json:"credits"`
		RateLimit *struct {
			Allowed      bool `json:"allowed"`
			LimitReached bool `json:"limit_reached"`
			PrimaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"primary_window"`
			SecondaryWindow *struct {
				UsedPercent     int `json:"used_percent"`
				LimitWindowSecs int `json:"limit_window_seconds"`
				ResetAfterSecs  int `json:"reset_after_seconds"`
			} `json:"secondary_window"`
		} `json:"rate_limit"`
		SpendControl *struct {
			Reached         bool `json:"reached"`
			IndividualLimit *struct {
				Used        string `json:"used"`
				Limit       string `json:"limit"`
				Remaining   string `json:"remaining"`
				UsedPercent int    `json:"used_percent"`
				ResetAfter  int    `json:"reset_after_seconds"`
			} `json:"individual_limit"`
		} `json:"spend_control"`
	}
	if err := json.Unmarshal(body, &u); err != nil {
		return nil, err
	}
	s := &provider.QuotaSnapshot{
		Billing: provider.BillingPlan,
		Account: or(u.Email, account),
		Plan:    or(u.PlanType, plan),
		AsOf:    time.Now(),
	}
	now := time.Now()
	if u.RateLimit != nil {
		status := "allowed"
		if u.RateLimit.LimitReached {
			status = "limit reached"
		} else if !u.RateLimit.Allowed {
			status = "not allowed"
		}
		s.Notes = append(s.Notes, "Rate Limit: "+status)
		if pw := u.RateLimit.PrimaryWindow; pw != nil {
			s.Windows = append(s.Windows, provider.QuotaWindow{
				Label: "primary (5h)", Kind: "tokens",
				RemainingPct: float64(100-pw.UsedPercent) / 100.0,
				ResetsAt:     now.Add(time.Duration(pw.ResetAfterSecs) * time.Second),
			})
		}
		if sw := u.RateLimit.SecondaryWindow; sw != nil {
			s.Windows = append(s.Windows, provider.QuotaWindow{
				Label: "weekly", Kind: "tokens",
				RemainingPct: float64(100-sw.UsedPercent) / 100.0,
				ResetsAt:     now.Add(time.Duration(sw.ResetAfterSecs) * time.Second),
			})
		}
	}
	if sc := u.SpendControl; sc != nil && sc.IndividualLimit != nil {
		il := sc.IndividualLimit
		s.Windows = append(s.Windows, provider.QuotaWindow{
			Label: "Spend", Kind: "money",
			RemainingPct: float64(100-il.UsedPercent) / 100.0,
			ResetsAt:     now.Add(time.Duration(il.ResetAfter) * time.Second),
		})
	}
	s.RemainingPct = provider.BindingRemaining(s.Windows)
	return s, nil
}

// fetchCodexQuota GETs /backend-api/wham/usage with Bearer + originator.
func fetchCodexQuota(cfg *Config, prov Provider) (*provider.QuotaSnapshot, error) {
	authFile := authFilePath("codex", "oauth_auth")
	p := newCodexOAuthProvider(authFile)
	tok, acct, err := p.token()
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	usageURL := strings.TrimSuffix(prov.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	return parseCodexQuota(body, acct, "")
}
```

- [ ] **Step 4: Refactor `showCodexUsage` to use the parser**

Replace the body of `showCodexUsage` (from the `var u struct{...}` declaration through the end) with:
```go
	authFile := authFilePath("codex", "oauth_auth")
	p := newCodexOAuthProvider(authFile)
	tok, acct, err := p.token()
	if err != nil {
		fmt.Println(cYellow("Not logged in.") + " Run: " + cCyan("model-proxy login codex"))
		return
	}
	usageURL := strings.TrimSuffix(prov.OpenAIBaseURL, "/codex") + "/wham/usage"
	req, _ := http.NewRequest("GET", usageURL, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("originator", "codex_cli_rs")
	if acct != "" {
		req.Header.Set("ChatGPT-Account-Id", acct)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		fmt.Println(cRed("Error: usage request: " + err.Error()))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		fmt.Printf("%s HTTP %d: %s\n", cRed("Error:"), resp.StatusCode, truncate(string(body), 200))
		return
	}
	s, err := parseCodexQuota(body, acct, "")
	if err != nil {
		fmt.Println(cRed("Error: parse: " + err.Error()))
		return
	}
	fmt.Printf("%s %s\n", cDim("Account:   "), cBold(cCyan(or(s.Account, "(unknown)"))))
	fmt.Printf("%s %s\n", cDim("Plan:      "), cMagenta(or(s.Plan, "(unknown)")))
	printQuotaSnapshot(s)
```
(Keep the `func showCodexUsage(cfg *Config, prov Provider) {` signature and the `fmt.Printf("Provider: codex")` line at the top unchanged.)

- [ ] **Step 5: Wire `QuotaFn` for codex**

In `proxy.go:buildProviders`, `case "codex":` block, add:
```go
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCodexQuota(cfg, prov) }
```

- [ ] **Step 6: Run tests**

Run: `cd model-proxy && go test . -run TestParseCodexQuota -v && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd model-proxy && git add main.go quota_test.go proxy.go && git commit -m "feat(quota): codex parser + unified display (parseCodexQuota)"
```

---

## Task 5: volcengine quota parser + display

**Files:**
- Modify: `main.go`, `proxy.go`.
- Test: `quota_test.go`.

**Interfaces:**
- Produces: `parseVolcengineQuota(u *afpUsage) *provider.QuotaSnapshot`; `fetchVolcengineQuota(name string) (*provider.QuotaSnapshot, error)`.

- [ ] **Step 1: Write the failing test** — append to `quota_test.go`

```go
func TestParseVolcengineQuota(t *testing.T) {
	u := &afpUsage{
		PlanType:    "agent-plan",
		AFPFiveHour: afpWindow{Quota: 100, Used: 80, ResetTime: 1750000000000},
		AFPWeekly:   afpWindow{Quota: 100, Used: 30, ResetTime: 1750000000000},
		AFPMonthly:  afpWindow{Quota: 100, Used: 10, ResetTime: 1750000000000},
	}
	s := parseVolcengineQuota(u)
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	// binding = min(5h rem 0.2, weekly rem 0.7, monthly rem 0.9) = 0.2
	if s.RemainingPct != 0.2 {
		t.Errorf("RemainingPct=%v, want 0.2 (5h binding)", s.RemainingPct)
	}
	if s.Plan != "agent-plan" {
		t.Errorf("Plan=%q", s.Plan)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test . -run TestParseVolcengineQuota -v`
Expected: FAIL — `parseVolcengineQuota undefined`.

- [ ] **Step 3: Implement the parser + fetcher in `main.go`** (near `showVolcengineUsage`)

```go
// parseVolcengineQuota converts the GetAFPUsage result into a QuotaSnapshot.
func parseVolcengineQuota(u *afpUsage) *provider.QuotaSnapshot {
	s := &provider.QuotaSnapshot{Billing: provider.BillingPlan, Plan: u.PlanType, AsOf: time.Now()}
	add := func(label string, w afpWindow) {
		rem := -1.0
		if w.Quota > 0 {
			rem = (w.Quota - w.Used) / w.Quota
		}
		var reset time.Time
		if w.ResetTime > 0 {
			reset = time.UnixMilli(w.ResetTime)
		}
		s.Windows = append(s.Windows, provider.QuotaWindow{
			Label: label, Kind: "tokens",
			Used: w.Used, Total: w.Quota, RemainingPct: rem, ResetsAt: reset,
		})
	}
	add("5h", u.AFPFiveHour)
	add("daily", u.AFPDaily)
	add("weekly", u.AFPWeekly)
	add("monthly", u.AFPMonthly)
	s.RemainingPct = provider.BindingRemaining(s.Windows)
	return s
}

// fetchVolcengineQuota calls GetAFPUsage (signed, AK/SK). Returns BillingUnknown
// if AK/SK aren't configured or the call fails.
func fetchVolcengineQuota(name string) (*provider.QuotaSnapshot, error) {
	creds, err := loadVolcengineCreds(name)
	if err != nil || creds.AccessKey == "" || creds.SecretKey == "" {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "AK/SK not configured"}, nil
	}
	u, err := getAFPUsage(creds.AccessKey, creds.SecretKey)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	return parseVolcengineQuota(u), nil
}
```

- [ ] **Step 4: Refactor `showVolcengineUsage` to use the parser**

Replace the body after the AK/SK check (the `u, err := getAFPUsage(...)` block and the `printAFPWindow` calls) with:
```go
	s := fetchVolcengineQuota(provName)
	if s.Billing == provider.BillingUnknown {
		fmt.Printf("%s %s\n", cDim("Note:       "), s.Err)
		listConfigModels(prov)
		return
	}
	if s.Plan != "" {
		fmt.Printf("%s %s\n", cDim("Plan:      "), cMagenta(s.Plan))
	}
	printQuotaSnapshot(s)
```
Keep the AK/SK-absent branch that prints the Agent Plan note + `listConfigModels` (now unified into the `BillingUnknown` path above — the prior `if err != nil || creds.AccessKey == "" ...` block can stay as the guard, then call `fetchVolcengineQuota`).

- [ ] **Step 5: Wire `QuotaFn` for volcengine**

In `proxy.go:buildProviders`, `case "volcengine":` block, add:
```go
			pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchVolcengineQuota(name) }
```

- [ ] **Step 6: Run tests + vet**

Run: `cd model-proxy && go test . -run TestParseVolcengineQuota -v && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd model-proxy && git add main.go quota_test.go proxy.go && git commit -m "feat(quota): volcengine parser + unified display (parseVolcengineQuota)"
```

---

## Task 6: deepseek + compass parsers + display

**Files:**
- Modify: `main.go`, `proxy.go`.
- Test: `quota_test.go`.

**Interfaces:**
- Produces: `parseDeepseekQuota(body []byte) *provider.QuotaSnapshot`; `fetchDeepseekQuota(cfg, name, prov)`; `parseCompassQuota(mu *MonthlyProjectUsage, account string) *provider.QuotaSnapshot`; `fetchCompassQuota(cfg)`.

- [ ] **Step 1: Write the failing tests** — append to `quota_test.go`

```go
func TestParseDeepseekQuota(t *testing.T) {
	body := []byte(`{"is_available":true,"balance_infos":[
		{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`)
	s := parseDeepseekQuota(body)
	if s.Billing != provider.BillingPayG {
		t.Errorf("Billing=%v, want PayG", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct=%v, want -1 (balance has no window)", s.RemainingPct)
	}
	if len(s.Windows) != 1 || s.Windows[0].Total != 10.5 {
		t.Errorf("balance window: %+v", s.Windows)
	}
}

func TestParseCompassQuota(t *testing.T) {
	mu := &MonthlyProjectUsage{TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}
	s := parseCompassQuota(mu, "alice@example.com")
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v, want Plan", s.Billing)
	}
	if s.RemainingPct != 0.7 {
		t.Errorf("RemainingPct=%v, want 0.7", s.RemainingPct)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test . -run 'TestParseDeepseekQuota|TestParseCompassQuota' -v`
Expected: FAIL — `parseDeepseekQuota`/`parseCompassQuota undefined`.

- [ ] **Step 3: Implement the four functions in `main.go`**

```go
// parseDeepseekQuota parses /user/balance. deepseek is pay-as-you-go: no window,
// RemainingPct unmeasured (-1). Balance kept as a single window for display.
func parseDeepseekQuota(body []byte) *provider.QuotaSnapshot {
	var u struct {
		IsAvailable  bool `json:"is_available"`
		BalanceInfos []struct {
			Currency        string `json:"currency"`
			TotalBalance    string `json:"total_balance"`
			GrantedBalance  string `json:"granted_balance"`
			ToppedUpBalance string `json:"topped_up_balance"`
		} `json:"balance_infos"`
	}
	s := &provider.QuotaSnapshot{Billing: provider.BillingPayG, RemainingPct: -1, AsOf: time.Now()}
	if err := json.Unmarshal(body, &u); err != nil {
		s.Err = err.Error()
		return s
	}
	if !u.IsAvailable {
		s.Notes = append(s.Notes, "insufficient balance")
	}
	for _, b := range u.BalanceInfos {
		total, _ := strconv.ParseFloat(b.TotalBalance, 64)
		s.Windows = append(s.Windows, provider.QuotaWindow{
			Label: or(b.Currency, "Balance"), Kind: "money",
			Total: total, RemainingPct: -1,
			Details: []provider.QuotaDetail{
				{Label: "granted", Used: atof(b.GrantedBalance)},
				{Label: "topped-up", Used: atof(b.ToppedUpBalance)},
			},
		})
	}
	return s
}

func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

// fetchDeepseekQuota GETs /user/balance.
func fetchDeepseekQuota(cfg *Config, name string, prov Provider) (*provider.QuotaSnapshot, error) {
	auth := newAuthProvider(prov.Provider, name, cfg)
	req, _ := http.NewRequest("GET", prov.UsageURL, nil)
	if err := auth.Inject(req); err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: fmt.Sprintf("HTTP %d", resp.StatusCode)}, nil
	}
	return parseDeepseekQuota(body), nil
}

// parseCompassQuota converts monthly_usage into a single-window plan snapshot.
func parseCompassQuota(mu *MonthlyProjectUsage, account string) *provider.QuotaSnapshot {
	s := &provider.QuotaSnapshot{Billing: provider.BillingPlan, Account: account, Plan: mu.Plan, AsOf: time.Now()}
	rem := -1.0
	if mu.TotalAmount > 0 {
		rem = mu.Balance / mu.TotalAmount
	}
	s.Windows = append(s.Windows, provider.QuotaWindow{
		Label: "Monthly", Kind: "money",
		Used: mu.Usage, Total: mu.TotalAmount, RemainingPct: rem,
	})
	s.RemainingPct = rem
	return s
}

// fetchCompassQuota mints the CQP key + POSTs monthly_usage.
func fetchCompassQuota(cfg *Config) (*provider.QuotaSnapshot, error) {
	path := authFilePath("compass", "oauth_auth")
	a, _ := loadAccount(path)
	c := newCompassClient(path)
	mu, err := c.MonthlyUsage()
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	acct := ""
	if a != nil {
		acct = a.Email
	}
	return parseCompassQuota(mu, acct), nil
}
```
Add `"strconv"` to `main.go` imports if missing.

- [ ] **Step 4: Refactor `showDeepseekUsage` and `showCompassUsage`**

`showDeepseekUsage`: after the existing HTTP fetch (keep the request-build + `auth.Inject` + "Not logged in" handling), replace the inline parse+display with:
```go
	s := parseDeepseekQuota(body)
	if s.Err != "" {
		fmt.Println(cRed("Error: parse: " + s.Err))
		return
	}
	for _, n := range s.Notes {
		fmt.Printf("%s %s\n", cDim("Available:  "), cRed(n))
	}
	printQuotaSnapshot(s)
```
(Keep the `Provider:` header line.)

`showCompassUsage`: keep the account/project_id prints; replace the `mu, err := c.MonthlyUsage()` display block with:
```go
	mu, err := c.MonthlyUsage()
	if err != nil {
		fmt.Printf("%s %s\n", cDim("Usage:      "), cRed("(unavailable: "+err.Error()+")"))
	} else {
		s := parseCompassQuota(mu, a.Email)
		printQuotaSnapshot(s)
	}
```
Note: this changes compass's exact display format (it previously printed `Usage: balance / total (balance, plan, year-month)` on one line). If byte-identical compass output matters, keep the old line and also call `parseCompassQuota` only for the scheduler via `fetchCompassQuota`. **Decision:** keep the existing compass one-liner display (do not refactor `showCompassUsage`'s Usage line) to avoid changing its output; only add `fetchCompassQuota`/`parseCompassQuota` for scheduling. So skip the `showCompassUsage` edit above; just add the two functions.

- [ ] **Step 5: Wire `QuotaFn` for deepseek + compass**

In `proxy.go:buildProviders`:
- `case "deepseek":` add `pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchDeepseekQuota(cfg, name, prov) }`
- `case "compass":` add `pcfg.QuotaFn = func() (*provider.QuotaSnapshot, error) { return fetchCompassQuota(cfg) }`

- [ ] **Step 6: Run tests + vet**

Run: `cd model-proxy && go test . -run 'TestParseDeepseekQuota|TestParseCompassQuota' -v && go vet ./...`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
cd model-proxy && git add main.go quota_test.go proxy.go && git commit -m "feat(quota): deepseek + compass parsers; wire QuotaFn for all providers"
```

---

## Task 7: quotaTracker — poll loop + persistence

**Files:**
- Create: `quota.go`
- Test: `quota_test.go`.

**Interfaces:**
- Produces: `quotaTracker` type; `newQuotaTracker(path string, cfg func()*Config, provs func()map[string]provider.Provider) *quotaTracker`; methods `(t *quotaTracker) start()`, `(t *quotaTracker) stop()`, `(t *quotaTracker) snapshot(name string) *provider.QuotaSnapshot`, `(t *quotaTracker) allSnapshots() map[string]*provider.QuotaSnapshot`, `(t *quotaTracker) refreshOne(name string)`, `(t *quotaTracker) pollAll(now time.Time)`.

- [ ] **Step 1: Write the failing tests** — append to `quota_test.go`

```go
func TestQuotaTracker_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota_state.json")
	cfg := func() *Config { return &Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newQuotaTracker(path, cfg, provs)
	tr.setSnapshot("zhipu", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.42, AsOf: time.Now()})
	tr.persist()

	tr2 := newQuotaTracker(path, cfg, provs)
	tr2.load()
	if s := tr2.snapshot("zhipu"); s == nil || s.RemainingPct != 0.42 {
		t.Fatalf("after reload: %+v", s)
	}
}

func TestQuotaTracker_StaleIsUnknown(t *testing.T) {
	tr := newQuotaTracker(filepath.Join(t.TempDir(), "q.json"), func() *Config { return &Config{} }, func() map[string]provider.Provider { return nil })
	tr.setSnapshot("zhipu", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now().Add(-30 * time.Minute)})
	if c := tr.effectiveBilling("zhipu", 5*time.Minute); c != provider.BillingUnknown {
		t.Errorf("stale snapshot billing=%v, want Unknown", c)
	}
}

func TestQuotaTracker_PollAllCallsQuota(t *testing.T) {
	dir := t.TempDir()
	tr := newQuotaTracker(filepath.Join(dir, "q.json"),
		func() *Config { return &Config{Providers: map[string]Provider{"x": {Provider: "zhipu"}}} },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": &snapshotProv{rem: 0.77}} },
	)
	tr.pollAll(time.Now())
	if s := tr.snapshot("x"); s == nil || s.RemainingPct != 0.77 {
		t.Fatalf("pollAll did not populate: %+v", s)
	}
}

// snapshotProv is a test Provider returning a fixed snapshot.
type snapshotProv struct{ rem float64 }

func (s *snapshotProv) AuthHeaders(*http.Request) error                  { return nil }
func (s *snapshotProv) Refresh() error                                   { return nil }
func (s *snapshotProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (s *snapshotProv) Login() error                                     { return nil }
func (s *snapshotProv) Logout() error                                    { return nil }
func (s *snapshotProv) Usage() (any, error)                              { return nil, nil }
func (s *snapshotProv) FetchModels() ([]string, error)                   { return nil, nil }
func (s *snapshotProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: s.rem, AsOf: time.Now()}, nil
}
```
Add `"path/filepath"` to imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test . -run TestQuotaTracker -v`
Expected: FAIL — `newQuotaTracker undefined`.

- [ ] **Step 3: Implement `quota.go`**

```go
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"model-proxy/provider"
)

// quotaTracker polls providers' Quota() periodically, caches the results in
// memory + a file (~/.model-proxy/quota_state.json), and serves them to the
// scheduler. It has its own mutex (quotaMu), independent of healthMu / reload mu.
type quotaTracker struct {
	mu    sync.RWMutex
	state map[string]*provider.QuotaSnapshot
	path  string
	cfg   func() *Config
	provs func() map[string]provider.Provider
	stop  chan struct{}
}

func newQuotaTracker(path string, cfg func() *Config, provs func() map[string]provider.Provider) *quotaTracker {
	return &quotaTracker{
		state: map[string]*provider.QuotaSnapshot{},
		path:  path,
		cfg:   cfg,
		provs: provs,
		stop:  make(chan struct{}),
	}
}

func (t *quotaTracker) start() {
	t.load()                          // baseline before first poll
	go func() {
		// bootstrap poll shortly after start
		t.pollAfter(10 * time.Second)
		interval := t.cfg().Scheduling.pollInterval()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		staleCheck := 3 * interval
		for {
			select {
			case <-ticker.C:
				t.pollAll(time.Now())
				_ = staleCheck
			case <-t.stop:
				return
			}
		}
	}()
}

func (t *quotaTracker) stop() { close(t.stop) }

func (t *quotaTracker) pollAfter(d time.Duration) {
	go func() {
		time.Sleep(d)
		t.pollAll(time.Now())
	}()
}

// pollAll polls every configured provider in parallel (bounded by the runtime's
// goroutine scheduling; provider count is small) and persists once at the end.
func (t *quotaTracker) pollAll(now time.Time) {
	cfg := t.cfg()
	provs := t.provs()
	var wg sync.WaitGroup
	var mu sync.Mutex
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	for _, name := range names {
		provImpl := provs[name]
		if provImpl == nil {
			continue
		}
		wg.Add(1)
		go func(n string, p provider.Provider) {
			defer wg.Done()
			s, err := p.Quota()
			mu.Lock()
			defer mu.Unlock()
			if err != nil || s == nil {
				s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: now}
				if err != nil {
					s.Err = err.Error()
				}
			}
			s.AsOf = now
			t.setSnapshot(n, s)
		}(name, provImpl)
	}
	wg.Wait()
	t.persist()
}

// refreshOne re-polls a single provider (called after a 429).
func (t *quotaTracker) refreshOne(name string) {
	provs := t.provs()
	p := provs[name]
	if p == nil {
		return
	}
	s, err := p.Quota()
	if err != nil || s == nil {
		s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: time.Now()}
		if err != nil {
			s.Err = err.Error()
		}
	}
	s.AsOf = time.Now()
	t.setSnapshot(name, s)
	t.persist()
}

func (t *quotaTracker) setSnapshot(name string, s *provider.QuotaSnapshot) {
	t.mu.Lock()
	t.state[name] = s
	t.mu.Unlock()
}

func (t *quotaTracker) snapshot(name string) *provider.QuotaSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state[name]
}

func (t *quotaTracker) allSnapshots() map[string]*provider.QuotaSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]*provider.QuotaSnapshot, len(t.state))
	for k, v := range t.state {
		out[k] = v
	}
	return out
}

// effectiveBilling applies the staleness guard: a snapshot older than 3× the poll
// interval is treated as Unknown. Pay-as-you-go config intent is honored here too.
func (t *quotaTracker) effectiveBilling(name string, interval time.Duration) provider.BillingClass {
	cfg := t.cfg()
	if cfg.Providers[name].Billing == "pay-as-you-go" {
		return provider.BillingPayG
	}
	s := t.snapshot(name)
	if s == nil {
		return provider.BillingUnknown
	}
	if s.Billing == provider.BillingUnknown || s.Err != "" {
		return provider.BillingUnknown
	}
	if time.Since(s.AsOf) > 3*interval {
		return provider.BillingUnknown
	}
	return s.Billing
}

type persistedSnapshot struct {
	Billing      provider.BillingClass `json:"billing"`
	RemainingPct float64               `json:"remaining_pct"`
	Windows      []provider.QuotaWindow `json:"windows"`
	AsOf         time.Time             `json:"as_of"`
	Err          string                `json:"err,omitempty"`
}

func (t *quotaTracker) persist() {
	t.mu.RLock()
	out := make(map[string]persistedSnapshot, len(t.state))
	for k, v := range t.state {
		out[k] = persistedSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
	t.mu.RUnlock()
	data, err := json.MarshalIndent(map[string]any{"providers": out}, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[quota] persist rename failed: %v", err)
	}
}

func (t *quotaTracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var wrap struct {
		Providers map[string]persistedSnapshot `json:"providers"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range wrap.Providers {
		t.state[k] = &provider.QuotaSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd model-proxy && go test . -run TestQuotaTracker -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && git add quota.go quota_test.go && git commit -m "feat(quota): quotaTracker (poll loop + persistence + staleness)"
```

---

## Task 8: Proxy wiring — start tracker, keep on reload, refresh on 429

**Files:**
- Modify: `proxy.go`.
- Test: `proxy_quota_test.go` (Create, minimal — full schedule tests are Task 9).

**Interfaces:**
- Produces: `Proxy.quota *quotaTracker`; `NewProxy` starts it; `reload` keeps it; `recordRateLimit` calls `p.quota.refreshOne`.

- [ ] **Step 1: Write the failing test** — `proxy_quota_test.go`

```go
package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestProxy_QuotaRefreshOnRateLimit: a 429 on a provider triggers an async
// quota refresh of that provider.
func TestProxy_QuotaRefreshOnRateLimit(t *testing.T) {
	primary, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 429, `{}`, intHdr("Retry-After", "30"), 0
	})
	defer primary.Close()
	fallback, _ := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 200, `{"ok":true}`, nil, 0
	})
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"m1": {
			{Provider: "primary", Model: "m1", Priority: 1},
			{Provider: "fallback", Model: "m1", Priority: 2},
		}},
		Scheduling: schedCfg(3, "50ms", "10s", "5s", "0s"),
	}
	p := NewProxy(cfg)
	p.providers["primary"] = &quotaCountProv{}
	p.providers["fallback"] = &testProv{key: "f"}
	var refreshes atomic.Int32
	p.quota = &quotaTracker{
		state: map[string]*provider.QuotaSnapshot{},
		cfg:   func() *Config { return cfg },
		provs: func() map[string]provider.Provider { return p.providers },
	}
	// refreshHook lets the test count refreshes without running a real poll.
	p.quota.refreshHook = func(name string) { refreshes.Add(1) }
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if got := refreshes.Load(); got != 1 {
		t.Errorf("expected 1 quota refresh after 429, got %d", got)
	}
}

// quotaCountProv is a testProv whose Quota() is callable.
type quotaCountProv struct{ testProv }

func (q *quotaCountProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5}, nil
}
```
(This test defines a `p.quotaRefresh` hook so the assertion is deterministic without goroutine timing. The implementation calls `p.quotaRefresh(provider)` from `recordRateLimit`.)

- [ ] **Step 2: Run test to verify it fails**

Run: `cd model-proxy && go test . -run TestProxy_QuotaRefreshOnRateLimit -v`
Expected: FAIL — `p.quotaRefresh undefined`.

- [ ] **Step 3: Wire the tracker into `Proxy`**

In `proxy.go`, add a field to the `Proxy` struct (after `sticky`):
```go
	quota *quotaTracker
```
Add a `refreshHook` field to `quotaTracker` (in `quota.go`) so tests can intercept refreshes without running a real poll:
```go
	refreshHook func(name string) // test override; if nil, refreshOne runs the real poll
```
and make `refreshOne` check it first:
```go
func (t *quotaTracker) refreshOne(name string) {
	if t.refreshHook != nil {
		t.refreshHook(name)
		return
	}
	// …the real single-provider poll body from Task 7…
}
```

In `NewProxy`, after building `p`, start the tracker:
```go
func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		cfg:       cfg,
		providers: buildProviders(cfg),
		client:    &http.Client{Timeout: 0},
		health:    map[string]*providerHealth{},
		sticky:    map[string]routeSticky{},
	}
	home, _ := os.UserHomeDir()
	qpath := filepath.Join(home, ".model-proxy", "quota_state.json")
	p.quota = newQuotaTracker(qpath,
		func() *Config { return p.cfgSnapshot() },
		func() map[string]provider.Provider { return p.providerSnapshot() })
	p.quota.start()
	return p
}
```
Add the snapshot helpers (read under RLock — the tracker reads cfg asynchronously, so it must use the lock):
```go
func (p *Proxy) cfgSnapshot() *Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}
func (p *Proxy) providerSnapshot() map[string]provider.Provider {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.providers
}
```
Add `"os"` and `"path/filepath"` to `proxy.go` imports if missing.

In `reload`, **keep** the tracker running (it reads fresh cfg via the closures) — do not stop/recreate it. Add a refresh kick after rebuild:
```go
	// ... existing rebuild + health/sticky reset ...
	go p.quota.pollAll(time.Now()) // pick up added/removed providers immediately
	return nil
```

In `recordRateLimit`, after setting `rateLimitedUntil`, trigger a quota refresh:
```go
func (p *Proxy) recordRateLimit(name string, until time.Time) {
	p.healthMu.Lock()
	// ... existing body ...
	p.healthMu.Unlock()
	if p.quota != nil {
		go p.quota.refreshOne(name)
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `cd model-proxy && go test . -run TestProxy_QuotaRefreshOnRateLimit -v`
Expected: PASS.

- [ ] **Step 5: Run full suite (existing tests must still pass)**

Run: `cd model-proxy && go test ./...`
Expected: PASS (NewProxy now starts a tracker; existing tests construct NewProxy and must remain green — the tracker's goroutine writes to ~/.model-proxy/quota_state.json, which is harmless; tests that 429 will fire an async refresh that's a no-op when providers lack QuotaFn).

- [ ] **Step 6: Commit**

```bash
cd model-proxy && git add proxy.go proxy_quota_test.go && git commit -m "feat(quota): wire quotaTracker into Proxy lifecycle + 429 refresh"
```

---

## Task 9: schedule() rewrite — quota ranking + sticky switch

**Files:**
- Modify: `proxy.go`.
- Test: `proxy_quota_test.go`.

**Interfaces:**
- Produces: rewritten `schedule()`; `billingClass(name, qs)`; `effectiveRemaining(name, qs, now)`.

- [ ] **Step 1: Write the failing tests** — append to `proxy_quota_test.go`

```go
// staticQuota sets the tracker's snapshot for a provider (plan, given remaining%).
func staticQuota(p *Proxy, name string, rem float64) {
	p.quota.setSnapshot(name, &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: rem, AsOf: time.Now()})
}

func newQuotaProxy(t *testing.T, provs map[string]Provider, routes map[string][]RouteTarget) *Proxy {
	t.Helper()
	for _, pr := range provs {
		pr.Provider = "static"
	}
	cfg := &Config{
		Providers:  provs,
		Routes:     routes,
		Scheduling: Scheduling{QuotaSwitchMargin: 15},
	}
	p := NewProxy(cfg)
	p.quota = &quotaTracker{state: map[string]*provider.QuotaSnapshot{}, cfg: func() *Config { return cfg }, provs: func() map[string]provider.Provider { return p.providers }}
	for name := range provs {
		p.providers[name] = &testProv{key: name}
	}
	return p
}

// firstProvider returns the provider the scheduler tries first for a model.
func firstProvider(p *Proxy, model string) string {
	ordered := p.schedule(model, p.cfg.Routes[model])
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].Provider
}

func TestSchedule_PicksMostRemaining(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}, "c": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}, {Provider: "c"}}})
	staticQuota(p, "a", 0.2)
	staticQuota(p, "b", 0.8)
	staticQuota(p, "c", 0.5)
	if got := firstProvider(p, "m"); got != "b" {
		t.Errorf("first=%q, want b (highest remaining)", got)
	}
}

func TestSchedule_StickyHoldsWithinDwell(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "10m"
	staticQuota(p, "a", 0.2) // current sticky (low)
	staticQuota(p, "b", 0.9) // much higher
	// seed sticky on 'a'
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now()}
	if got := firstProvider(p, "m"); got != "a" {
		t.Errorf("within dwell: first=%q, want a (sticky despite lower remaining)", got)
	}
}

func TestSchedule_SwitchesAfterDwellByMargin(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "1ms"
	staticQuota(p, "a", 0.4)
	staticQuota(p, "b", 0.8) // ahead by 0.4 ≥ 0.15 margin
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now().Add(-time.Second)}
	time.Sleep(2 * time.Millisecond) // dwell expired
	if got := firstProvider(p, "m"); got != "b" {
		t.Errorf("after dwell + margin: first=%q, want b", got)
	}
}

func TestSchedule_NoSwitchBelowMargin(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	p.cfg.Scheduling.StickyDwell = "1ms"
	staticQuota(p, "a", 0.5)
	staticQuota(p, "b", 0.6) // ahead by 0.1 < 0.15 margin
	p.sticky["m"] = routeSticky{provider: "a", since: time.Now().Add(-time.Second)}
	time.Sleep(2 * time.Millisecond)
	if got := firstProvider(p, "m"); got != "a" {
		t.Errorf("below margin: first=%q, want a (stay sticky)", got)
	}
}

func TestSchedule_PayGStrictLastResort(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"plan": {Billing: ""}, "payg": {Billing: "pay-as-you-go"}},
		map[string][]RouteTarget{"m": {{Provider: "plan"}, {Provider: "payg"}}})
	p.cfg.Providers["plan"].Provider = "static"
	p.cfg.Providers["payg"].Provider = "static"
	p.quota.setSnapshot("plan", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.05, AsOf: time.Now()})
	p.quota.setSnapshot("payg", &provider.QuotaSnapshot{Billing: provider.BillingPayG, RemainingPct: -1, AsOf: time.Now()})
	if got := firstProvider(p, "m"); got != "plan" {
		t.Errorf("plan@5%% still beats payg: first=%q, want plan", got)
	}
	// now mark plan unavailable (rate-limited) → payg used
	p.healthMu.Lock()
	p.health["plan"] = &providerHealth{rateLimitedUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()
	if got := firstProvider(p, "m"); got != "payg" {
		t.Errorf("when plan unavailable: first=%q, want payg (last resort)", got)
	}
}
```
Add `"time"` to imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd model-proxy && go test . -run TestSchedule_ -v`
Expected: FAIL — schedule still uses priority ordering.

- [ ] **Step 3: Rewrite `schedule()` and add helpers in `proxy.go`**

Replace the existing `schedule` function with:
```go
// schedule returns targets in try-order using quota-aware ranking:
//   tier: plan < unknown < payg (pay-as-you-go is strict last-resort)
//   within tier: effective_remaining desc (peak-discounted), then priority asc.
// Sticky routing keeps the current provider for sticky_dwell, then switches only
// if another *plan* provider's effective remaining beats it by ≥ switchMargin.
func (p *Proxy) schedule(exposed string, targets []RouteTarget) []RouteTarget {
	now := time.Now()
	sched := p.cfg.Scheduling

	// Snapshot quota once (brief RLock), to avoid holding quotaMu during the sort.
	qs := p.quota.allSnapshots()

	p.healthMu.Lock()
	defer p.healthMu.Unlock()

	avail := func(name string) bool {
		h := p.health[name]
		return h == nil || h.available(now)
	}

	var availTargets []RouteTarget
	for _, t := range targets {
		if avail(t.Provider) {
			availTargets = append(availTargets, t)
		}
	}

	billing := func(name string) provider.BillingClass { return p.billingClass(name, qs) }
	eff := func(name string) float64 { return p.effectiveRemaining(name, qs, now) }

	sort.SliceStable(availTargets, func(i, j int) bool {
		bi, bj := billing(availTargets[i].Provider), billing(availTargets[j].Provider)
		if bi != bj {
			return bi < bj
		}
		ri, rj := eff(availTargets[i].Provider), eff(availTargets[j].Provider)
		if ri != rj {
			return ri > rj
		}
		return availTargets[i].Priority < availTargets[j].Priority
	})

	margin := sched.switchMargin()
	cur := p.sticky[exposed]
	keepSticky := cur.provider != "" && avail(cur.provider)
	if keepSticky && now.Sub(cur.since) >= sched.dwell() {
		// dwell expired: keep unless another *plan* provider is ahead by ≥ margin.
		curEff := eff(cur.provider)
		for _, t := range availTargets {
			if t.Provider == cur.provider || billing(t.Provider) != provider.BillingPlan {
				continue
			}
			if eff(t.Provider)-curEff >= margin {
				keepSticky = false
				break
			}
		}
	}

	var ordered []RouteTarget
	if keepSticky {
		p.sticky[exposed] = cur // unchanged since
		for _, t := range availTargets {
			if t.Provider == cur.provider {
				ordered = append(ordered, t)
			}
		}
	} else if len(availTargets) > 0 {
		p.sticky[exposed] = routeSticky{provider: availTargets[0].Provider, since: now}
	}
	for _, t := range availTargets {
		if len(ordered) > 0 && t.Provider == ordered[0].Provider {
			continue
		}
		ordered = append(ordered, t)
	}
	return ordered
}

// billingClass returns the effective scheduling tier, applying the staleness
// guard and the pay-as-you-go config override.
func (p *Proxy) billingClass(name string, qs map[string]*provider.QuotaSnapshot) provider.BillingClass {
	if p.cfg.Providers[name].Billing == "pay-as-you-go" {
		return provider.BillingPayG
	}
	s := qs[name]
	if s == nil || s.Billing == provider.BillingUnknown || s.Err != "" {
		return provider.BillingUnknown
	}
	if time.Since(s.AsOf) > 3*p.cfg.Scheduling.pollInterval() {
		return provider.BillingUnknown
	}
	return s.Billing
}

// effectiveRemaining discounts remaining quota by the active peak multiplier.
// Only meaningful for plan providers; unknown/payg callers ignore the result.
func (p *Proxy) effectiveRemaining(name string, qs map[string]*provider.QuotaSnapshot, now time.Time) float64 {
	s := qs[name]
	if s == nil || s.RemainingPct < 0 {
		return 1.0
	}
	mult := p.cfg.Providers[name].peakMultiplier(now)
	if mult < 1 {
		mult = 1
	}
	return s.RemainingPct / mult
}
```
Remove the now-unused `RouteTarget.inPeak` method and the `Provider.inPeak` calls in `schedule` (the `inPeak` method itself can stay for any external caller, but `schedule` no longer references it). Leave `Provider.inPeak` and `Provider.peakMultiplier` defined (Task 2). Remove the old `inPeak`-based sort block entirely.

- [ ] **Step 4: Run the new tests**

Run: `cd model-proxy && go test . -run TestSchedule_ -v`
Expected: PASS.

- [ ] **Step 5: Run the full suite (existing sticky/circuit/rate-limit tests must still pass)**

Run: `cd model-proxy && go test ./...`
Expected: PASS. If `TestStickyDwell_HoldsThenReEvaluates` fails, it's because the new schedule re-ranks by quota when no quota data exists (all providers `BillingUnknown` → eff 1.0 → falls back to priority, which should reproduce old behavior). Verify by confirming all unknown → priority ordering. If it fails, ensure `billingClass` returns `BillingUnknown` for providers with no snapshot (it does) and the priority tiebreak matches the old `inPeak`+priority ordering.

- [ ] **Step 6: Commit**

```bash
cd model-proxy && git add proxy.go proxy_quota_test.go && git commit -m "feat(quota): quota-aware schedule() ranking + margin-based sticky switch"
```

---

## Task 10: Documentation

**Files:**
- Modify: `AGENTS.md`, `README.md`, `CLAUDE.md`.

- [ ] **Step 1: Update `AGENTS.md`**

In the "model-proxy 实现经验" → "架构：Provider 抽象 + 协议路由" section, add a new subsection documenting: `quotaTracker` (poll loop + `~/.model-proxy/quota_state.json`), `Provider.Quota()` + `QuotaFn`, the `schedule()` ranking `(billing tier, effective_remaining desc, priority asc)`, `effective_remaining = RemainingPct / peak_multiplier`, the margin-based sticky switch, per-provider quota source mapping (zhipu/codex/volcengine/compass/deepseek), the multi-segment `peak_hours` config + `billing: pay-as-you-go`, and `scheduling.quota_poll_interval`/`quota_switch_margin`. Add to the 踩过的坑 (gotchas) list: staleness guard (3× interval → unknown), volcengine needs AK/SK for quota, lock ordering healthMu→quotaMu.

- [ ] **Step 2: Update `README.md`**

Add a short "Quota-aware scheduling" section: one paragraph on behavior (polls remaining quota, balances plan providers, payg last-resort, peak discounts effective remaining), plus a config example showing `billing: pay-as-you-go`, multi-segment `peak_hours`, and the two scheduling fields.

- [ ] **Step 3: Update `CLAUDE.md`**

In the "Failover health & sticky routing" / `scheduling:` description, add a bullet on quota-aware scheduling: the `quotaTracker` polls `Provider.Quota()` every `quota_poll_interval`, persists `~/.model-proxy/quota_state.json`, `schedule()` ranks by `(billing, effective_remaining=RemainingPct/peak_multiplier, priority)`, switches sticky after `sticky_dwell` when ahead by `quota_switch_margin`, payg (`billing: pay-as-you-go`) is strict last-resort. Update the config-field list and the "Adding a new provider" note (a new plan provider should implement `Quota()`/wire `QuotaFn`).

- [ ] **Step 4: Build + vet + full test**

Run: `cd model-proxy && go build -o model-proxy . && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd model-proxy && git add AGENTS.md README.md CLAUDE.md && git commit -m "docs: quota-aware scheduling"
```

---

## Self-Review Notes

- **Spec coverage:** quota-aware sticky (Task 9), periodic polling + persistence (Tasks 7–8), margin switch after dwell (Task 9), peak folded into effective_remaining via per-segment multiplier (Tasks 2 + 9), payg strict last-resort (Task 9), unified parse (Tasks 3–6), no hard floor (Task 9 — no floor logic present), graceful degradation / staleness (Task 7), 429 re-poll (Task 8). All spec sections mapped.
- **Type consistency:** `provider.QuotaSnapshot` / `QuotaWindow` / `BillingClass` used uniformly; `Quota()` signature matches `testProv`/`snapshotProv`/`quotaCountProv`; `peakMultiplier`/`pollInterval`/`switchMargin` accessor names match across config + schedule; `quotaTracker.refreshOne` + `refreshHook` match the test.
- **Known risk:** Task 9 Step 5 — the pre-existing `TestStickyDwell_HoldsThenReEvaluates` relies on priority-based re-evaluation; with no quota data all providers are `BillingUnknown` (eff 1.0) so the sort falls back to priority — behavior should match. If it regresses, the fix is in `effectiveRemaining`/`billingClass` defaults, not the test.
