package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/display"
	"net/http"
	"strings"
	"time"
)

// KimiCodeProvider implements the Kimi Code provider — Moonshot's membership-
// based coding platform at https://api.kimi.com/coding. An API key (from the
// Kimi Code console, https://www.kimi.com/code/console) authenticates BOTH
// protocols; the two endpoints are per-protocol:
//   - openai_base_url (e.g. https://api.kimi.com/coding/v1) → /chat/completions,
//     /responses, /models, /usages — uses Authorization: Bearer.
//   - anthropic_base_url (e.g. https://api.kimi.com/coding, no /v1) → /v1/messages
//     — uses x-api-key (the Anthropic SDK convention).
//
// The proxy selects the upstream base by protocol (see proxy.forward): anthropic
// requests go to anthropic_base_url (proxy keeps the client's /v1/messages path),
// openai requests to openai_base_url. Both auth headers are set on every request
// so one provider config serves both protocols (the OpenAI endpoint ignores
// x-api-key; the Anthropic endpoint reads x-api-key and ignores Bearer).
//
// Kimi Code is a membership (coding plan) product with a windowed quota
// (weekly total + 5h rolling rate-cap + extra-usage wallet), distinct from the
// pay-as-you-go Moonshot 开放平台 (api.moonshot.cn). Quota is polled via
// GET <openai_base_url>/usages (see Quota / ParseKimiCodeQuota).
type KimiCodeProvider struct {
	*ApiKeyBase
	baseProbe
	cfg *Config
}

func init() {
	Register("kimi-code", func(cfg *Config, providerName string) (Provider, error) {
		return &KimiCodeProvider{
			ApiKeyBase: newApiKeyBaseBound(cfg, providerName),
			cfg:        cfg,
		}, nil
	})
}

// AuthHeaders injects the API key as BOTH Authorization: Bearer (OpenAI +
// /usages + /models endpoints) and x-api-key (Anthropic /v1/messages endpoint),
// so one provider config serves both protocols. Overrides ApiKeyBase.AuthHeaders
// (which only sets Bearer + deletes x-api-key). Mirrors DeepSeekProvider.
func (p *KimiCodeProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by protocol
// (anthropic_base_url for /messages, openai_base_url otherwise). Kimi Code
// customizes only auth (see AuthHeaders).
func (p *KimiCodeProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *KimiCodeProvider) Logout() error { return p.DeleteKey() }

func (p *KimiCodeProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ProbeRequest overrides the OpenAI default: Kimi Code's anthropic_base_url speaks
// the Anthropic messages API, so the probe goes to /v1/messages (base does NOT
// include /v1; the SDK appends it) with an anthropic body. Mirrors forward's
// anthropic path. When anthropic_base_url is unset, the probe falls back to the
// OpenAI base (selected by probeModelCallable) and the /v1/messages path won't
// match — but a Kimi Code config always sets anthropic_base_url.
func (p *KimiCodeProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   AnthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets Kimi Code's per-request anthropic-version header. Applied on
// EVERY upstream request (forward + probe) so the probe (which has no client
// request to copy from) is accepted by the Anthropic endpoint; harmless on the
// OpenAI path (ignored). Mirrors AqpProvider (minus the compass request id).
func (p *KimiCodeProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// usagesURL derives the quota endpoint from openai_base_url + "/usages" (the
// shape used by the official Kimi Code CLI: baseUrl + "/usages"). Falls back to
// cfg.UsageURL when set (test/mirror override). Returns "" when neither is set
// (not configured → Quota treats it as unmeasured).
func (p *KimiCodeProvider) usagesURL() string {
	if p.cfg.UsageURL != "" {
		return p.cfg.UsageURL
	}
	if p.cfg.OpenAIBaseURL == "" {
		return ""
	}
	return strings.TrimRight(p.cfg.OpenAIBaseURL, "/") + "/usages"
}

// Quota GETs /usages and parses the membership quota envelope. On any failure
// (auth, HTTP, non-usages body) returns a BillingUnknown snapshot carrying the
// error (never a non-nil error) so the scheduler treats Kimi Code as unmeasured
// rather than crashing the poll. 404 specifically means "membership not active
// / usage endpoint unavailable" — surfaced as a hint.
func (p *KimiCodeProvider) Quota() (*QuotaSnapshot, error) {
	url := p.usagesURL()
	if url == "" {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "openai_base_url not set"}, nil
	}
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/json")
	if err := p.AuthHeaders(req); err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	for k, v := range p.cfg.Headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		hint := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if resp.StatusCode == 404 {
			hint = "HTTP 404 — usage endpoint unavailable (no Kimi Code membership?)"
		} else if resp.StatusCode == 401 || resp.StatusCode == 403 {
			hint = fmt.Sprintf("HTTP %d — check API key", resp.StatusCode)
		}
		return &QuotaSnapshot{Billing: BillingUnknown, Err: hint}, nil
	}
	s, _ := ParseKimiCodeQuota(body, "")
	if s == nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "not kimi-code usages format"}, nil
	}
	return s, nil
}

// Usage prints the Kimi Code membership quota. On fetch failure falls back to
// listing config models (volcengine pattern) so `usage kimi-code` is never mute.
func (p *KimiCodeProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.cfg.ProviderName)))
	s, err := p.Quota()
	if err != nil || s == nil || s.Billing != BillingPlan {
		why := "unavailable"
		if s != nil && s.Err != "" {
			why = s.Err
		}
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("("+why+")"))
		fmt.Printf("%s check quota at %s or via the Kimi Code CLI /usage command\n", display.Dim("            "), display.Cyan("https://www.kimi.com/code/console"))
		listConfigModels(p.cfg.Models)
		return nil
	}
	fmt.Printf("%s %s\n", display.Dim("Plan:       "), display.Magenta(display.Or(s.Plan, "Kimi Code membership")))
	DecorateExhaustionEta(p.cfg.ProviderName, s)
	for _, w := range s.Windows {
		line := formatQuotaWindowLine(w)
		if w.Ultimate {
			line += exhaustionHint(s.ExhaustionEta, time.Now())
		}
		fmt.Println(line)
		// Kimi Code token windows use an abstract 0-100 scale — the absolute
		// used/total IS the percentage already shown by the bar, so it's
		// redundant. Show the absolute line only for money windows (extra-usage
		// wallet / monthly cap), where the amount is meaningful.
		if w.Kind == "money" && w.Total > 0 {
			fmt.Printf("%s %.0f used / %.0f total (%.0f remaining)\n",
				display.Dim(display.Pad("Usage:", 18)), w.Used, w.Total, w.Total-w.Used)
		}
	}
	return nil
}

// ParseKimiCodeQuota parses the Kimi Code /usages body into a QuotaSnapshot.
// Returns (nil, nil) if the body isn't the usages format (caller falls back to
// BillingUnknown). The response shape (field spelling drifted across CLI
// versions — `used` vs `remaining`, `resetAt` vs `reset_at`):
//
//	{
//	  "usage":  { "name":"Weekly limit", "used":40, "limit":1000, "resetAt":"ISO" },
//	  "limits": [ { "detail":{...}, "window":{"duration":300,"timeUnit":"MINUTE"} } ],
//	  "boosterWallet": { "balance":{"type":"BOOSTER","amount":..,"amountLeft":..}, ... }
//	}
//
// Multi-window consensus rule (codebase-wide): the LONGEST-duration window is
// the hard limit → Ultimate (scheduling base + pace source); every shorter
// window is a soft rate-cap → Short. For Kimi Code that means the weekly
// summary → Ultimate (Duration 7d) and the 5h limit → Short (Duration 5h) —
// matching zhipu, so Kimi's surplus paces against the weekly budget and stays
// comparable across providers. `boosterWallet.balance`
// (type BOOSTER) is the extra-usage pay-as-you-go wallet → a money window with
// RemainingPct=-1 (amount/amountLeft are fixed-point ÷1,000,000 → cents).
func ParseKimiCodeQuota(body []byte, account string) (*QuotaSnapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // the API emits numbers as quoted strings ("100", "10000") for
	// many fields; json.Number accepts both shapes so the loose int/float helpers
	// below work regardless.
	var u struct {
		Usage  json.RawMessage `json:"usage"`
		Limits []struct {
			Detail map[string]any `json:"detail"`
			Window struct {
				Duration json.Number `json:"duration"`
				TimeUnit string      `json:"timeUnit"`
			} `json:"window"`
		} `json:"limits"`
		BoosterWallet *struct {
			Balance struct {
				Type       string      `json:"type"`
				Amount     json.Number `json:"amount"`
				AmountLeft json.Number `json:"amountLeft"`
			} `json:"balance"`
			MonthlyChargeLimit *struct {
				PriceInCents json.Number `json:"priceInCents"`
				Currency     string      `json:"currency"`
			} `json:"monthlyChargeLimit"`
			MonthlyUsed *struct {
				PriceInCents json.Number `json:"priceInCents"`
				Currency     string      `json:"currency"`
			} `json:"monthlyUsed"`
			MonthlyChargeLimitEnabled bool `json:"monthlyChargeLimitEnabled"`
		} `json:"boosterWallet"`
	}
	if err := dec.Decode(&u); err != nil {
		return nil, nil
	}
	// Require at least a summary or limits to call this a usages payload;
	// otherwise the body isn't ours (return nil → caller falls back).
	if len(u.Usage) == 0 && len(u.Limits) == 0 {
		return nil, nil
	}
	s := &QuotaSnapshot{
		Billing: BillingPlan,
		Account: account,
		Plan:    "Kimi Code membership",
		AsOf:    time.Now(),
	}

	// Build the token windows (the summary `usage` + each `limits[]` entry),
	// then apply the multi-window consensus rule: the LONGEST-duration window
	// is the hard limit (Ultimate = scheduling base + pace source); every
	// shorter window is a soft rate-cap (Short). This matches the codebase-wide
	// convention (zhipu: weekly=Ultimate, 5h=Short) and keeps Kimi's surplus on
	// the same weekly horizon as comparable plan providers.
	type tw struct {
		w     QuotaWindow
		dur   time.Duration
		order int // stable sort key: limits[] first (5h before weekly summary)
	}
	var tws []tw
	order := 0
	for _, l := range u.Limits {
		detail := l.Detail
		if detail == nil {
			detail = map[string]any{} // window-only label still useful
		}
		secs := kimiWindowSeconds(numToInt(l.Window.Duration), l.Window.TimeUnit)
		label := kimiLimitLabel(detail, numToInt(l.Window.Duration), l.Window.TimeUnit, secs)
		w, ok := kimiWindowFromDetail(detail, label, secs)
		if !ok {
			continue
		}
		w.Kind = "tokens"
		dur := time.Duration(secs) * time.Second
		if dur <= 0 && kimiIs5hWindow(detail, secs) {
			dur = 5 * time.Hour // name says 5h but window block omitted the duration
		}
		w.Duration = dur
		tws = append(tws, tw{w: w, dur: dur, order: order})
		order++
	}
	if summary := asMap(u.Usage); summary != nil {
		if w, ok := kimiWindowFromDetail(summary, "Weekly limit", 0); ok {
			w.Kind = "tokens"
			w.Duration = 7 * 24 * time.Hour
			tws = append(tws, tw{w: w, dur: 7 * 24 * time.Hour, order: order})
			order++
		}
	}
	// Longest duration → Ultimate (hard limit); the rest → Short (soft rate-cap).
	// Display order: Short windows first (limits[] order, so 5h leads), then the
	// Ultimate window — matching the requested "5h before weekly" layout.
	var maxDur time.Duration
	for i := range tws {
		if tws[i].dur > maxDur {
			maxDur = tws[i].dur
		}
	}
	// Mark in place (index access — range copies wouldn't persist). Two passes
	// keep a stable, deterministic order (Short first, Ultimate last).
	for i := range tws {
		if tws[i].dur == maxDur && maxDur > 0 {
			tws[i].w.Ultimate = true
		} else {
			tws[i].w.Short = true
		}
	}
	for i := range tws {
		if tws[i].w.Short {
			s.Windows = append(s.Windows, tws[i].w)
		}
	}
	for i := range tws {
		if tws[i].w.Ultimate {
			s.Windows = append(s.Windows, tws[i].w)
		}
	}

	// Booster wallet (extra-usage pay-as-you-go balance) → money, unmeasured.
	if u.BoosterWallet != nil && u.BoosterWallet.Balance.Type == "BOOSTER" {
		amount := numToFloat(u.BoosterWallet.Balance.Amount)
		if amount > 0 {
			const fixedPoint = 1_000_000
			total := amount / fixedPoint / 100.0
			left := numToFloat(u.BoosterWallet.Balance.AmountLeft) / fixedPoint / 100.0
			s.Windows = append(s.Windows, QuotaWindow{
				Label:        "Extra usage",
				Kind:         "money",
				Total:        total,
				Used:         total - left,
				RemainingPct: -1,
			})
		}
		if u.BoosterWallet.MonthlyChargeLimitEnabled && u.BoosterWallet.MonthlyChargeLimit != nil {
			limit := numToFloat(u.BoosterWallet.MonthlyChargeLimit.PriceInCents) / 100.0
			var used float64
			if u.BoosterWallet.MonthlyUsed != nil {
				used = numToFloat(u.BoosterWallet.MonthlyUsed.PriceInCents) / 100.0
			}
			cur := ""
			if u.BoosterWallet.MonthlyChargeLimit.Currency != "" {
				cur = u.BoosterWallet.MonthlyChargeLimit.Currency + " "
			}
			rem := -1.0
			if limit > 0 {
				rem = (limit - used) / limit
			}
			s.Windows = append(s.Windows, QuotaWindow{
				Label: "Monthly cap " + cur, Kind: "money",
				Total: limit, Used: used, RemainingPct: rem,
			})
		}
	}

	// If we couldn't find an Ultimate (weekly) window, the membership state is
	// unmeasured — degrade to BillingUnknown so the scheduler falls back to
	// priority-based ordering rather than a bogus 0 surplus.
	if !hasUltimate(s.Windows) {
		s.Billing = BillingUnknown
		s.RemainingPct = -1
		return s, nil
	}
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}

// hasUltimate reports whether any window is marked Ultimate.
func hasUltimate(windows []QuotaWindow) bool {
	for i := range windows {
		if windows[i].Ultimate {
			return true
		}
	}
	return false
}

// asMap unmarshals a RawMessage into a map[string]any, returning nil on failure
// or when the value is null/absent.
func asMap(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// kimiWindowFromDetail builds a QuotaWindow from a usages `detail` (or summary)
// record. used is derived from `remaining` when `used` is absent (the CLI parser
// does the same — field spelling drifts). Returns ok=false when neither used nor
// limit is present (nothing to display).
func kimiWindowFromDetail(detail map[string]any, label string, _ int64) (QuotaWindow, bool) {
	limit := intFromKeys(detail, "limit")
	used := intFromKeys(detail, "used")
	if used == nil {
		if rem := intFromKeys(detail, "remaining"); rem != nil && limit != nil {
			u := *limit - *rem
			used = &u
		}
	}
	if used == nil && limit == nil {
		return QuotaWindow{}, false
	}
	u, lim := 0.0, 0.0
	if used != nil {
		u = float64(*used)
	}
	if limit != nil {
		lim = float64(*limit)
	}
	name := strFromKeys(detail, "name", "title")
	if name != "" {
		label = name
	}
	rem := -1.0
	if lim > 0 {
		rem = (lim - u) / lim
	}
	w := QuotaWindow{
		Label:        label,
		Used:         u,
		Total:        lim,
		RemainingPct: rem,
	}
	if r := resetAtFromKeys(detail); !r.IsZero() {
		w.ResetsAt = r
	}
	return w, true
}

// kimiIs5hWindow reports whether a limits[] entry is the 5h rolling window: true
// when the window duration is 5h, or when the name contains "5h".
func kimiIs5hWindow(detail map[string]any, secs int64) bool {
	if secs == int64(5*time.Hour/time.Second) {
		return true
	}
	name := strings.ToLower(strFromKeys(detail, "name", "title"))
	return strings.Contains(name, "5h")
}

// kimiWindowSeconds converts a {duration, timeUnit} window to seconds. Returns 0
// when unknown. Mirrors the CLI's limitLabel duration math.
func kimiWindowSeconds(duration int, timeUnit string) int64 {
	if duration <= 0 {
		return 0
	}
	unit := strings.ToUpper(timeUnit)
	switch {
	case strings.Contains(unit, "MINUTE"):
		return int64(duration) * 60
	case strings.Contains(unit, "HOUR"):
		return int64(duration) * 3600
	case strings.Contains(unit, "DAY"):
		return int64(duration) * 86400
	case strings.Contains(unit, "SECOND"):
		return int64(duration)
	}
	return 0
}

// kimiLimitLabel builds a display label for a limits[] entry, preferring the
// detail's name/title; else a duration-based label ("5h limit", "7d limit").
func kimiLimitLabel(detail map[string]any, duration int, timeUnit string, secs int64) string {
	if name := strFromKeys(detail, "name", "title", "scope"); name != "" {
		return name
	}
	if secs > 0 {
		d := time.Duration(secs) * time.Second
		switch {
		case d >= 24*time.Hour && d%24*time.Hour == 0:
			return fmt.Sprintf("%dd limit", int(d/(24*time.Hour)))
		case d >= time.Hour && d%time.Hour == 0:
			return fmt.Sprintf("%dh limit", int(d/time.Hour))
		case d >= time.Minute && d%time.Minute == 0:
			return fmt.Sprintf("%dm limit", int(d/time.Minute))
		default:
			return fmt.Sprintf("%ds limit", secs)
		}
	}
	_ = duration
	_ = timeUnit
	return "Limit"
}

// intFromKeys returns the first present integer-valued field among keys (checks
// numeric, json.Number, and numeric-string values, matching the CLI's loose
// toInt). Returns nil when none match.
func intFromKeys(m map[string]any, keys ...string) *int64 {
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue
		}
		switch n := v.(type) {
		case float64:
			i := int64(n)
			return &i
		case int:
			i := int64(n)
			return &i
		case int64:
			return &n
		case json.Number:
			if i, err := n.Int64(); err == nil {
				return &i
			}
		case string:
			var i int64
			if _, err := fmt.Sscanf(n, "%d", &i); err == nil {
				return &i
			}
		}
	}
	return nil
}

// numToInt parses a json.Number (which may be a quoted string like "300") to an
// int; 0 on any error.
func numToInt(n json.Number) int {
	i, err := n.Int64()
	if err != nil {
		return 0
	}
	return int(i)
}

// numToFloat parses a json.Number to a float64; 0 on any error.
func numToFloat(n json.Number) float64 {
	f, err := n.Float64()
	if err != nil {
		return 0
	}
	return f
}

// strFromKeys returns the first present string-valued field among keys.
func strFromKeys(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// resetAtFromKeys returns the first parseable reset timestamp among the known
// field-name variants (reset_at / resetAt / reset_time / resetTime). The CLI
// notes these drift across versions.
func resetAtFromKeys(m map[string]any) time.Time {
	for _, k := range []string{"reset_at", "resetAt", "reset_time", "resetTime"} {
		s, ok := m[k].(string)
		if !ok || s == "" {
			continue
		}
		if t := parseKimiTime(s); !t.IsZero() {
			return t
		}
	}
	return time.Time{}
}

// parseKimiTime parses a Kimi Code reset timestamp. The API emits ISO8601
// (often with nano precision and a trailing Z); RFC3339Nano handles that, with
// RFC3339 and a couple of common layouts as fallbacks.
func parseKimiTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999Z", "2006-01-02T15:04:05Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
