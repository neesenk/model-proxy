package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"model-proxy/internal/display"
	"model-proxy/internal/protocol"
)

// OpenCodeGoProvider implements OpenCode Go (opencode.ai/zen/go) — the OpenCode
// team's $10/month SUBSCRIPTION plan for curated open coding models. NOT the
// same product as OpenCode Zen (the pay-as-you-go gateway at /zen): Go has its
// own base path, its own (smaller) catalog, and windowed per-model dollar
// limits instead of prepaid credits.
//
// One API key (from the console, opencode.ai/auth → subscribe to Go) serves
// BOTH protocols on two per-protocol bases (the deepseek pattern), but the
// legs read DIFFERENT auth headers (verified live 2026-09 against
// https://opencode.ai/zen/go):
//   - openai_base_url (https://opencode.ai/zen/go/v1) → /chat/completions,
//     /responses, /models — Authorization: Bearer (Bearer-only 401 "Invalid
//     API key."; no auth 401 "Missing API key.").
//   - anthropic_base_url (https://opencode.ai/zen/go) → /v1/messages —
//     x-api-key ONLY (Bearer-only 401 "Missing API key."; x-api-key 401
//     "Invalid API key.").
//
// → AuthHeaders dual-writes Bearer + x-api-key so one config serves both legs.
//
// Billing (docs "Usage limits"): each model carries a monthly dollar limit
// ($15/$30/$60 tiers) split into windows — 5h = 20%, weekly = 50%, monthly =
// 100% of it. Quota is polled via GET <openai_base_url>/usage — an
// undocumented endpoint (discovered via farion1231/cc-switch#6433) returning
// the used percent + reset time per window; see Quota / ParseOpenCodeGoUsage.
// Window exhaustion ALSO surfaces reactively as upstream 402/429 → cooldown +
// failover. /models is PUBLIC and auth-ignoring (200 with an invalid Bearer)
// and usage_url is deliberately left unconfigured, so login key validation
// cannot use either endpoint: login sends one minimal real request instead
// (this provider's ProbeRequest = POST chat/completions, first config model —
// see internal/login validateKeyByRealProbe), rejecting on 401/403 / envelope
// auth failure / network error. (Setting usage_url would flip login onto the
// Bearer-GET path; the undocumented /usage endpoint is a quota poller, not a
// verified key-validation signal.)
type OpenCodeGoProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

// opencodeGoConsoleURL is the OpenCode console (sign-in, Go subscription,
// usage tracking, API keys).
const opencodeGoConsoleURL = "https://opencode.ai/auth"

func init() {
	Register("opencode-go", func(cfg *Config, providerName string) (Provider, error) {
		return &OpenCodeGoProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as BOTH Authorization: Bearer (the chat /
// responses / models leg) and x-api-key (the /v1/messages anthropic leg,
// which does not read Bearer — verified live). Overrides ApiKeyBase.AuthHeaders
// (which only sets Bearer + deletes x-api-key).
func (p *OpenCodeGoProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *OpenCodeGoProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *OpenCodeGoProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists the Go catalog via the OpenAI-compatible /models endpoint
// (Bearer — the endpoint is public, but the auth call keeps the shared
// shape). If the gateway hides the list, this errors and the caller (models
// refresh / usage display) falls back to the config models: list.
func (p *OpenCodeGoProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ExtraHeaders layers the anthropic compatibility header and the session
// affinity header on every upstream request (forward + probe):
//
//   - anthropic-version: the forward path's client→upstream whitelist does
//     not carry it, and Go's /v1/messages follows the Anthropic Messages wire
//     (the official wiring is @ai-sdk/anthropic, whose transport always sends
//     the version). Harmless on the OpenAI paths.
//   - x-opencode-session: Go REQUIRES a session id on every request —
//     header-less requests are rejected 400 MissingSessionID ("cannot be
//     routed efficiently", verified live 2026-09 on /zen/go). The proxy
//     fronts many client families, and the forward whitelist already passes
//     their native per-conversation session headers through
//     (x-claude-code-session-id, x-session-id, user_id). The first present
//     one is mirrored into x-opencode-session — the client's own opaque id,
//     never invented. Agents implemented strictly against the OpenAI /
//     Anthropic specs send no session header at all and carry the stable id
//     in the BODY instead (prompt_cache_key / metadata.user_id /
//     client_metadata.session_id — Kimi Code does exactly this,
//     github.com/MoonshotAI/kimi-code/issues/3506). The forward path resolves
//     that identity ONCE from the ORIGINAL request and passes it as sessionID:
//     it is mirrored before any body-field fallback, because a cross-protocol
//     conversion may rewrite or drop the original field (chat→anthropic drops
//     prompt_cache_key; a→r even injects a HASHED prompt_cache_key derived
//     from metadata.user_id — re-reading the converted body would shard one
//     conversation into a different routing/prompt-cache lane per leg), so one
//     conversation keeps one Go routing/prompt-cache lane across requests.
//     Only when NEITHER a header, nor sessionID, nor a body identity exists
//     (the proxy's own probe/test traffic), a per-request synthesized id
//     ("mp-" + random UUID) keeps the request routable: there is no
//     conversation key to stay stable across, so a fresh id per request is
//     the honest representation. An explicit x-opencode-session from the
//     client always wins.
func (p *OpenCodeGoProvider) ExtraHeaders(req *http.Request, body []byte, sessionID string, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	// Session-id sources are guarded with printableASCII (same as zcode): a
	// control-byte value would make net/http reject the whole outbound request
	// and the failure would land on this provider's circuit breaker.
	if req.Header.Get("x-opencode-session") == "" {
		for _, h := range []string{"x-claude-code-session-id", "x-session-id", "user_id"} {
			if v := printableASCII(req.Header.Get(h)); v != "" {
				req.Header.Set("x-opencode-session", v)
				break
			}
		}
	}
	// The forward path's resolved client session (from the ORIGINAL request:
	// header allowlist first, then body spec fields) outranks whatever
	// identity the CONVERTED body happens to carry — see the method doc.
	// The body-derived half of that resolution is already bounded to a
	// printable token (protocol.SessionIDFromBody); the header half only got
	// TrimSpace'd, so it needs the same guard here.
	if req.Header.Get("x-opencode-session") == "" {
		if sid := printableASCII(sessionID); sid != "" {
			req.Header.Set("x-opencode-session", sid)
		}
	}
	// Spec-conforming header-less agents on unconverted legs: the stable
	// conversation id rides in the body (see the method doc).
	// protocol.SessionIDFromBody bounds it to a printable ≤256-byte token,
	// safe to echo into a header verbatim.
	if req.Header.Get("x-opencode-session") == "" {
		if sid := protocol.SessionIDFromBody(body); sid != "" {
			req.Header.Set("x-opencode-session", sid)
		}
	}
	// Nothing to mirror (no header, no body identity — the proxy's own
	// probe): Go hard-rejects a missing x-opencode-session with 400
	// MissingSessionID, so synthesize one instead of sending none.
	if req.Header.Get("x-opencode-session") == "" {
		req.Header.Set("x-opencode-session", "mp-"+newRequestID())
	}
}

// usageURL derives the quota endpoint from openai_base_url + "/usage" (the
// endpoint verified live 2026-10; it is undocumented publicly — discovered
// via farion1231/cc-switch#6433). cfg.UsageURL overrides it (test/mirror
// seam). Returns "" when neither is set (not configured → Quota treats it as
// unmeasured). Deliberately NOT wired into the config templates: login's
// key-validation branch keys on ModelsAuthless && no usage_url (see the type
// doc).
func (p *OpenCodeGoProvider) usageURL() string {
	if p.cfg.UsageURL != "" {
		return p.cfg.UsageURL
	}
	if p.cfg.OpenAIBaseURL == "" {
		return ""
	}
	return strings.TrimRight(p.cfg.OpenAIBaseURL, "/") + "/usage"
}

// Quota GETs /usage and parses the subscription quota envelope. On any failure
// (auth, HTTP, non-usage body) returns a BillingUnknown snapshot carrying the
// error (never a non-nil error) so the scheduler treats opencode-go as
// unmeasured rather than crashing the poll. 401/403 → check-API-key hint;
// 404 → usage endpoint unavailable (no Go subscription, or the endpoint
// moved). Window exhaustion still surfaces reactively as upstream 402/429 →
// targetexec's existing classification → cooldown + failover; when the
// console's "Use balance" option is on, over-limit traffic falls back to the
// separate Zen prepaid balance instead of blocking.
func (p *OpenCodeGoProvider) Quota() (*QuotaSnapshot, error) {
	url := p.usageURL()
	if url == "" {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "openai_base_url not set"}, nil
	}
	headers := map[string]string{"Accept": "application/json"}
	for k, v := range p.cfg.Headers {
		headers[k] = v
	}
	body, fail, ok := usageGet(url, p.AuthHeaders, headers, func(code int) string {
		switch code {
		case 404:
			return "HTTP 404 — usage endpoint unavailable"
		case 401, 403:
			return fmt.Sprintf("HTTP %d — check API key", code)
		}
		return fmt.Sprintf("HTTP %d", code)
	})
	if !ok {
		return fail, nil
	}
	s, _ := ParseOpenCodeGoUsage(body, "")
	if s == nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "not opencode-go usage format"}, nil
	}
	return s, nil
}

// Usage prints the parsed Go subscription windows (5h / weekly / monthly used
// percent, the monthly Ultimate carrying the exhaustion ETA); on fetch/parse
// failure it falls back to the console pointer + config model list (qwen-plan
// pattern) so `usage opencode-go` is never mute.
func (p *OpenCodeGoProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	s, err := p.Quota()
	if err != nil || s == nil || s.Billing != BillingPlan {
		why := "unavailable"
		if s != nil && s.Err != "" {
			why = s.Err
		}
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("("+why+")"))
		fmt.Printf("%s check usage at %s\n", display.Dim("            "), display.Cyan(opencodeGoConsoleURL))
		listConfigModels(p.cfg.Models)
		return nil
	}
	fmt.Printf("%s %s\n", display.Dim("Plan:       "), display.Magenta(display.Or(s.Plan, "OpenCode Go subscription")))
	DecorateExhaustionEta(p.providerName, s)
	for _, w := range s.Windows {
		line := formatQuotaWindowLine(w)
		if w.Ultimate {
			line += exhaustionHint(s.ExhaustionEta, time.Now())
		}
		fmt.Println(line)
	}
	return nil
}

// openCodeGoUsageWindow is one window object of the /usage envelope.
type openCodeGoUsageWindow struct {
	Status   string      `json:"status"`
	Percent  json.Number `json:"percent"`
	ResetsAt string      `json:"resetsAt"`
}

// ParseOpenCodeGoUsage parses the OpenCode Go /usage body into a
// QuotaSnapshot. Returns (nil, nil) if the body isn't the Go usage format
// (caller falls back to BillingUnknown). The response shape (undocumented
// endpoint, discovered via farion1231/cc-switch#6433; verified live 2026-10):
//
//	{"usage":{
//	  "rolling":{"status":"ok","percent":1,"resetsAt":"2026-10-01T10:10:01.964Z"},
//	  "weekly": {"status":"ok","percent":2,"resetsAt":"2026-10-05T00:00:00.000Z"},
//	  "monthly":{"status":"ok","percent":1,"resetsAt":"2026-10-27T17:35:19.000Z"}}}
//
// `percent` is the USED percent (observed 0 → nonzero after a request), so
// RemainingPct = (100 − percent)/100. Windows: rolling = 5h rate-cap → Short;
// weekly = week (UTC Monday 00:00 reset) → Short; monthly = subscription
// cycle (Duration = resetsAt − 1 month) → Ultimate (scheduling base). Every
// window rides an abstract 0-100 scale (Total 100, Used percent) so
// EstimateExhaustionEta's Δused/Δt works without the per-model dollar
// amounts. A window whose `status` isn't "ok" contributes a Note (label +
// status) instead of being silently trusted; a missing monthly window (no
// Ultimate) degrades the snapshot to BillingUnknown with the console link
// kept in Notes.
func ParseOpenCodeGoUsage(body []byte, account string) (*QuotaSnapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // percent arrives as a number or a quoted string
	var u struct {
		Usage struct {
			Rolling *openCodeGoUsageWindow `json:"rolling"`
			Weekly  *openCodeGoUsageWindow `json:"weekly"`
			Monthly *openCodeGoUsageWindow `json:"monthly"`
		} `json:"usage"`
	}
	if err := dec.Decode(&u); err != nil {
		return nil, nil
	}
	// All three windows absent → this isn't a Go usage payload (nil lets the
	// caller fall back instead of reporting a bogus empty plan).
	if u.Usage.Rolling == nil && u.Usage.Weekly == nil && u.Usage.Monthly == nil {
		return nil, nil
	}
	s := &QuotaSnapshot{
		Billing: BillingPlan,
		Account: account,
		Plan:    "OpenCode Go",
		AsOf:    time.Now(),
	}
	window := func(w *openCodeGoUsageWindow, label string, duration time.Duration) QuotaWindow {
		percent := numToFloat(w.Percent)
		qw := QuotaWindow{
			Label:        label,
			Total:        100,
			Used:         percent,
			RemainingPct: (100 - percent) / 100,
			Duration:     duration,
		}
		if t, err := time.Parse(time.RFC3339Nano, w.ResetsAt); err == nil {
			qw.ResetsAt = t
		}
		return qw
	}
	// statusNote records a non-ok window status for display instead of
	// silently trusting its percent.
	statusNote := func(label string, w *openCodeGoUsageWindow) {
		if w.Status != "" && w.Status != "ok" {
			s.Notes = append(s.Notes, label+": status "+w.Status)
		}
	}

	// Display order: Short rate-caps first (5h, weekly), Ultimate last
	// (monthly) — the kimi-code layout.
	if w := u.Usage.Rolling; w != nil {
		qw := window(w, "5h limit", 5*time.Hour)
		qw.Short = true
		s.Windows = append(s.Windows, qw)
		statusNote("5h limit", w)
	}
	if w := u.Usage.Weekly; w != nil {
		qw := window(w, "Weekly limit", 7*24*time.Hour)
		qw.Short = true
		s.Windows = append(s.Windows, qw)
		statusNote("Weekly limit", w)
	}
	if w := u.Usage.Monthly; w != nil {
		qw := window(w, "Monthly limit", 0)
		if !qw.ResetsAt.IsZero() {
			qw.Duration = qw.ResetsAt.Sub(qw.ResetsAt.AddDate(0, -1, 0))
		}
		qw.Ultimate = true
		s.Windows = append(s.Windows, qw)
		statusNote("Monthly limit", w)
	}

	// No monthly (Ultimate) window → the subscription state is unmeasured:
	// degrade to BillingUnknown so the scheduler falls back to priority
	// ordering rather than a bogus surplus; the console link keeps `usage`
	// actionable.
	if !hasUltimate(s.Windows) {
		s.Billing = BillingUnknown
		s.RemainingPct = -1
		s.Notes = append(s.Notes, "Console & usage: "+opencodeGoConsoleURL)
		return s, nil
	}
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s, nil
}
