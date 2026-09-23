package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/display"
)

// MiMoProvider implements the Xiaomi MiMo provider (小米 MiMo 开放平台,
// https://mimo.mi.com), PAY-AS-YOU-GO mode only. One API key serves BOTH
// protocols on two per-protocol bases (the deepseek pattern):
//   - openai_base_url (https://api.xiaomimimo.com/v1) → /chat/completions,
//     /responses, /models — uses Authorization: Bearer (or the vendor's
//     api-key header; both are documented auth methods).
//   - anthropic_base_url (https://api.xiaomimimo.com/anthropic, no /v1) →
//     /v1/messages — Claude Code reaches it via the Anthropic SDK convention
//     (x-api-key / Bearer).
//
// The proxy selects the upstream base by protocol (see proxy.forward); requests
// are byte-level passthrough (RewriteRequest no-op).
//
// Billing: pay-as-you-go only. usage_url points at the community-documented
// balance endpoint GET /api/v1/balance (Bearer auth), which doubles as the
// login-time key-validation probe. The balance is NOT a windowed budget, so
// Quota() reports BillingPayG with unmeasured money windows — the surplus
// scheduler ranks MiMo as a strict last resort, exactly like deepseek.
// MiMo's Token Plan (monthly package) is intentionally NOT supported: its
// usage endpoint is undocumented, and a subscription account would surface
// here as a plain balance (exhaustion still fails over reactively via the
// upstream 402, which targetexec's quota-denied policy already classifies).
//
// ProbeRequest / FilterModelIDs keep the baseProbe defaults, like deepseek:
// probe.Callable forces /v1/messages + the anthropic body when
// anthropic_base_url is set and ProbeModelProtocols probes each leg on its own
// base, so an override would be dead weight. ExtraHeaders IS overridden — see
// the method comment (the forward path drops the client's anthropic-version).
type MiMoProvider struct {
	*ApiKeyBase
	baseProbe
	cfg          *Config
	providerName string
}

func init() {
	Register("mimo", func(cfg *Config, providerName string) (Provider, error) {
		return &MiMoProvider{
			ApiKeyBase:   newApiKeyBaseBound(cfg, providerName),
			cfg:          cfg,
			providerName: providerName,
		}, nil
	})
}

// AuthHeaders injects the key as ALL documented MiMo auth shapes:
// Authorization: Bearer (OpenAI endpoint + the /api/v1/balance GET — the shape
// the community usage query uses), x-api-key (Anthropic SDK convention on the
// anthropic base), and api-key (the vendor's OWN primary documented header —
// MiMo documents `api-key: $MIMO_API_KEY` first on BOTH the OpenAI and the
// Anthropic compatibility pages, unlike deepseek which only reads
// Bearer/x-api-key). Endpoints ignore the shapes they don't read, so one
// config serves every MiMo surface (deepseek dual-write pattern plus the
// vendor's api-key spelling, since a gateway that reads only its own documented
// header would otherwise reject the anthropic path).
func (p *MiMoProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	req.Header.Set("api-key", key)
	return nil
}

// RewriteRequest is a no-op: pure passthrough. The proxy selects the upstream
// base URL by protocol in proxy.forward (openai_base_url for OpenAI paths,
// anthropic_base_url for /v1/messages).
func (p *MiMoProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *MiMoProvider) Logout() error { return p.DeleteKey() }

// FetchModels lists models via the OpenAI-compatible /models endpoint. If the
// gateway hides the list, this errors and the caller (models refresh / usage
// display) falls back to the config models: list.
func (p *MiMoProvider) FetchModels() ([]string, error) {
	return fetchModelsBearer(p.cfg, p.AuthHeaders)
}

// ExtraHeaders sets anthropic-version on every upstream request (forward +
// probe), like every other anthropic-compatible provider here (kimi-code,
// qwen-plan, step-plan, volcengine, aqp, zcode) — deepseek is the documented
// exception because its gateway ignores the header. The forward path's
// client→upstream header whitelist (targetexec.upstreamHeaderWhitelist) does
// NOT carry anthropic-version, so without this the proxy would strip the
// version header the Anthropic SDK (Claude Code's MiMo path) always sends, and
// a strict gateway would 400. Harmless where the endpoint ignores it.
func (p *MiMoProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// Quota GETs usage_url (/api/v1/balance) and parses the pay-as-you-go balance
// into unmeasured money windows — the deepseek contract: BillingPayG with
// RemainingPct=-1 (a balance is not a windowed budget, so the surplus
// scheduler ranks MiMo as a strict last resort). On ANY failure (auth, HTTP,
// non-balance body) returns a BillingUnknown snapshot carrying the error,
// never a non-nil error, so the scheduler poll stays alive; an unset usage_url
// is BillingUnknown with the console link in Notes (no endpoint to poll).
func (p *MiMoProvider) Quota() (*QuotaSnapshot, error) {
	if p.cfg.UsageURL == "" {
		return &QuotaSnapshot{
			Billing: BillingUnknown,
			Notes:   []string{"usage_url not set — cannot query the balance", mimoConsoleNote},
			AsOf:    time.Now(),
		}, nil
	}
	headers := map[string]string{"Accept": "application/json"}
	for k, v := range p.cfg.Headers {
		headers[k] = v
	}
	body, fail, ok := usageGet(p.cfg.UsageURL, p.AuthHeaders, headers, func(code int) string {
		switch code {
		case 401, 403:
			return fmt.Sprintf("HTTP %d — check API key", code)
		}
		return fmt.Sprintf("HTTP %d", code)
	})
	if !ok {
		return fail, nil
	}
	s := &QuotaSnapshot{Billing: BillingPayG, RemainingPct: -1, AsOf: time.Now()}
	s.Windows = append(s.Windows, ParseMiMoBalance(body)...)
	return s, nil
}

// Usage prints the MiMo balance. On an unmeasured snapshot it links the console
// and falls back to listing config models (qwen-plan/step-plan pattern) so
// `usage mimo` is never mute.
func (p *MiMoProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	s, err := p.Quota()
	if err != nil || s == nil {
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("(unavailable)"))
		listConfigModels(p.cfg.Models)
		return nil
	}
	if s.Billing == BillingUnknown {
		why := "unavailable"
		if s.Err != "" {
			why = s.Err
		}
		fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Red("("+why+")"))
		for _, n := range s.Notes {
			fmt.Println(display.Dim(display.Pad("", 18)) + n)
		}
		listConfigModels(p.cfg.Models)
		return nil
	}
	printQuotaSnapshot(s)
	return nil
}

// mimoConsoleNote is the shared "numbers live in the console" line appended to
// unmeasured snapshots (both the CLI and the Web UI render
// QuotaSnapshot.Notes).
const mimoConsoleNote = "Balance & recharge: " + mimoConsoleURL

// mimoConsoleURL is the MiMo console balance page.
const mimoConsoleURL = "https://platform.xiaomimimo.com/#/console/balance"

// ParseMiMoBalance parses the GET /api/v1/balance body into money windows.
// Returns nil when the body carries no recognizable balance field (the caller
// then shows an empty PayG snapshot — never a fabricated zero balance). The
// endpoint's envelope/field spelling is community-documented rather than
// vendor-published, so the parser is deliberately tolerant: a `data` (or
// `result`) envelope is unwrapped and the amount is read from the first present
// of several spellings, as a JSON number or a numeric string. Every window is
// money with RemainingPct=-1 (a balance is not a windowed budget), carrying the
// granted/topped-up split as details when the endpoint reports it.
func ParseMiMoBalance(body []byte) []QuotaWindow {
	root := decodeTolerant(body)
	if root == nil {
		return nil
	}
	data := unwrapEnvelope(root)
	total, ok := firstFloat(data, "balance", "total_balance", "available_balance", "remaining_balance", "credit_balance", "amount")
	if !ok {
		return nil
	}
	w := QuotaWindow{
		Label:        display.Or(firstString(data, "currency"), "Balance"),
		Kind:         "money",
		Total:        total,
		Used:         0,
		RemainingPct: -1,
	}
	var details []QuotaDetail
	if granted, ok := firstFloat(data, "granted_balance", "free_quota", "gift_balance", "complimentary_balance"); ok {
		details = append(details, QuotaDetail{Label: "granted", Used: granted})
	}
	if topped, ok := firstFloat(data, "topped_up_balance", "recharged_balance", "paid_balance"); ok {
		details = append(details, QuotaDetail{Label: "topped-up", Used: topped})
	}
	if len(details) > 0 {
		w.Details = details
		w.DetailLabel = "Balance split"
	}
	return []QuotaWindow{w}
}

// decodeTolerant decodes a JSON object with numbers as json.Number (accepts
// both JSON numbers and numeric strings). Returns nil for non-objects.
func decodeTolerant(body []byte) map[string]any {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil
	}
	return m
}

// unwrapEnvelope returns data when the object is a {data: {...}} (or
// {result: {...}}) envelope, else the object itself.
func unwrapEnvelope(m map[string]any) map[string]any {
	for _, key := range []string{"data", "result"} {
		if inner, ok := m[key].(map[string]any); ok {
			return inner
		}
	}
	return m
}

// firstFloat returns the first present numeric value among keys. Values arrive
// from decodeTolerant (UseNumber), so a JSON number is a json.Number and a
// quoted number is a string — both are accepted. ok=false when none match.
func firstFloat(m map[string]any, keys ...string) (float64, bool) {
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		switch n := v.(type) {
		case json.Number:
			if f, err := n.Float64(); err == nil {
				return f, true
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
				return f, true
			}
		}
	}
	return 0, false
}

// firstString returns the first present non-empty string value among keys.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}
