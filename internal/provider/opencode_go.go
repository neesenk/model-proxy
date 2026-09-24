package provider

import (
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/display"
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
// 100% of it. There is NO public usage API ("You can track your current usage
// in the console"), so Quota() returns a BillingUnknown snapshot carrying the
// console URL + the window structure (the qwen-plan pattern); window
// exhaustion surfaces reactively as upstream 402/429 → cooldown + failover.
// /models is PUBLIC and auth-ignoring (200 with an invalid Bearer), so login
// key validation cannot reject bad keys there — no usage_url is set and a
// wrong key surfaces at first request instead (401 → auth cooldown +
// failover).
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
//   - x-opencode-session: Go's docs ask clients to "send a stable session ID
//     in x-opencode-session for each conversation so we can optimize routing
//     and prompt caching". The proxy fronts many client families, and the
//     forward whitelist already passes their native per-conversation session
//     headers through (x-claude-code-session-id, x-session-id, user_id) — Go
//     recognizes some natively, but not every family. Mirroring the first
//     present native session header into x-opencode-session gives every
//     client the stable conversation id Go wants, without inventing one
//     (the value stays the client's own opaque id). No session header →
//     nothing is set (Go falls back to its own heuristics).
func (p *OpenCodeGoProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
	if req.Header.Get("x-opencode-session") == "" {
		for _, h := range []string{"x-claude-code-session-id", "x-session-id", "user_id"} {
			if v := req.Header.Get(h); v != "" {
				req.Header.Set("x-opencode-session", v)
				break
			}
		}
	}
}

// Quota returns an unmeasured snapshot: Go's usage windows (per-model monthly
// dollar limit split 5h/weekly/monthly) are console-only — there is no
// API-key billing endpoint. The console URL rides in Notes so both the CLI
// `usage` command and the Web UI (app.js renders snap.Notes) can link to the
// real numbers (qwen-plan pattern). BillingUnknown → the surplus scheduler
// ranks opencode-go by priority (neutral); window exhaustion is handled
// reactively: the upstream blocks with 402/429, which targetexec already
// classifies (quota-denied marker table / rate-limit parsing) → cooldown +
// failover, no opencode-go-specific code. When the console's "Use balance"
// option is on, over-limit traffic falls back to the separate Zen prepaid
// balance instead of blocking.
func (p *OpenCodeGoProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing:      BillingUnknown,
		RemainingPct: -1, // the documented "unknown" sentinel — 0 would read as "exhausted"
		Notes: []string{
			"OpenCode Go usage is viewable only in the console (no API-key usage endpoint)",
			"Console & usage: " + opencodeGoConsoleURL,
		},
		AsOf: time.Now(),
	}, nil
}

// Usage prints the console pointer, the documented limit structure, and the
// config model list (qwen-plan pattern) so `usage opencode-go` is never mute.
func (p *OpenCodeGoProvider) Usage() error {
	fmt.Printf("%s %s\n", display.Dim("Provider:  "), display.Bold(display.Blue(p.providerName)))
	fmt.Printf("%s $10/month subscription (per-model monthly dollar limit; 5h=20%% weekly=50%% monthly=100%%)\n", display.Dim("Billing:    "))
	fmt.Printf("%s %s\n", display.Dim("Usage:      "), display.Gray("(console-only; no public usage API)"))
	fmt.Printf("%s %s\n", display.Dim("Details:    "), display.Cyan(opencodeGoConsoleURL))
	listConfigModels(p.cfg.Models)
	return nil
}
