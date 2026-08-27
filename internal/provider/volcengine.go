package provider

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"model-proxy/internal/credstore"
)

// VolcengineProvider implements the Volcengine Ark (火山方舟) provider, including
// the "Agent Plan" subscription tier. A single Ark API key authenticates both
// protocols from one config entry:
//   - openai_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan/v3) →
//     /chat/completions, /responses, /models — uses Authorization: Bearer.
//   - anthropic_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan)
//     → /v1/messages (Anthropic-compatible, for Claude Code) — uses x-api-key.
//
// The proxy selects the upstream base by protocol (see proxy.forward). For
// anthropic, the proxy keeps the client's /v1 path (base_url + /v1/messages),
// so anthropic_base_url should NOT include /v1. Both auth headers are set on
// every request so one config serves both protocols (the OpenAI endpoint
// ignores x-api-key; the Anthropic-compatible endpoint reads x-api-key).
type VolcengineProvider struct {
	*ApiKeyBase
	baseProbe
	cfg *Config
}

func init() {
	Register("volcengine", func(cfg *Config, providerName string) (Provider, error) {
		return &VolcengineProvider{
			ApiKeyBase: newApiKeyBaseBound(cfg, providerName),
			cfg:        cfg,
		}, nil
	})
}

// AuthHeaders injects the API key as BOTH Authorization: Bearer (OpenAI endpoint)
// and x-api-key (Anthropic-compatible endpoint), so one provider config serves
// both protocols. Overrides ApiKeyBase.AuthHeaders (Bearer only).
func (p *VolcengineProvider) AuthHeaders(req *http.Request) error {
	key, err := p.LoadKey()
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("x-api-key", key)
	return nil
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by protocol.
func (p *VolcengineProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *VolcengineProvider) Logout() error { return p.DeleteKey() }
func (p *VolcengineProvider) FetchModels() ([]string, error) {
	if p.cfg.FetchModelsFn != nil {
		return p.cfg.FetchModelsFn()
	}
	return nil, fmt.Errorf("FetchModelsFn not configured")
}

// ProbeRequest returns the ANTHROPIC probe shape (/v1/messages), not baseProbe's
// OpenAI /chat/completions. probeModelCallable selects the anthropic base URL
// when anthropic_base_url is set (volcengine's primary path for Claude Code), so
// the probe path MUST be anthropic — /chat/completions on the anthropic base
// (.../api/plan/chat/completions) 404s for every model.
func (p *VolcengineProvider) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   anthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets anthropic-version on every upstream request (forward + probe).
// The probe has no client request to inherit it from, and the anthropic-compatible
// endpoint rejects requests without it.
func (p *VolcengineProvider) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

// volcengineModelFilterRegexps are the model-id exclusion rules applied to the
// ListArkAgentPlanModel result. Each is a compiled regexp; a model id is dropped
// when ANY rule matches. Rules are case-insensitive. Add a line here to extend
// coverage - this array is the single place to grow the filter.
//
// The Agent Plan endpoint (…/api/plan/v3) speaks family aliases; these ids are
// dropped because they are redundant or uncallable on that endpoint:
//   - *-latest            : rolling aliases redundant with the concrete family name
//   - doubao-seed-1-*     : standard-Ark "1.x + date-version" ids the plan endpoint
//     rejects (it only takes plan-scope aliases like 2.0)
//   - doubao-seed-*-lite  : small / low-latency "lite" tier (suffix or mid-segment)
//   - doubao-seed-*-mini  : small / low-latency "mini" tier
//
// The doubao-seed- rules do not match doubao-seedance-* / doubao-seedream-*
// (the char after "doubao-seed" is "a", not "-"); those are filtered separately
// by the endpoint probe since they are non-chat models.
var volcengineModelFilterRegexps = []*regexp.Regexp{
	regexp.MustCompile(`(?i)-latest$`),
	regexp.MustCompile(`(?i)^doubao-seed-1-`),
	regexp.MustCompile(`(?i)^doubao-seed-.*-lite($|-)`),
	regexp.MustCompile(`(?i)^doubao-seed-.*-mini$`),
}

// FilterModelIDs applies volcengine's static policy rules: drops ids matching
// volcengineModelFilterRegexps (*-latest / doubao-seed-1-* / lite / mini). This
// is the "policy" pass of `models refresh`; the endpoint probe is the separate
// callability pass. Overrides baseProbe's passthrough.
func (p *VolcengineProvider) FilterModelIDs(ids []string) (kept, dropped []string) {
	for _, id := range ids {
		if isVolcengineModelFiltered(id) {
			dropped = append(dropped, id)
		} else {
			kept = append(kept, id)
		}
	}
	return kept, dropped
}

// isVolcengineModelFiltered reports whether a model id should be excluded by the
// static regex rules.
func isVolcengineModelFiltered(id string) bool {
	for _, re := range volcengineModelFilterRegexps {
		if re.MatchString(id) {
			return true
		}
	}
	return false
}

// AfpWindow is one Agent Plan AFP quota window (5h/daily/weekly/monthly).
type AfpWindow struct {
	Quota     float64 `json:"Quota"`
	Used      float64 `json:"Used"`
	ResetTime int64   `json:"ResetTime"` // epoch ms
}

// AfpUsage is the Result payload of Volcengine GetAFPUsage: the 5h/daily/weekly/
// monthly AFP quota windows + the plan type.
type AfpUsage struct {
	PlanType    string    `json:"PlanType"`
	AFPFiveHour AfpWindow `json:"AFPFiveHour"`
	AFPDaily    AfpWindow `json:"AFPDaily"`
	AFPWeekly   AfpWindow `json:"AFPWeekly"`
	AFPMonthly  AfpWindow `json:"AFPMonthly"`
}

// ParseVolcengineQuota converts a GetAFPUsage result into a QuotaSnapshot. The
// monthly window is Ultimate (total budget); the 5h window is Short (rate cap);
// daily/weekly are intermediate display-only windows. Over-quota windows clamp
// to 0 remaining (not a negative that'd read as "unmeasured").
func ParseVolcengineQuota(u *AfpUsage) *QuotaSnapshot {
	s := &QuotaSnapshot{Billing: BillingPlan, Plan: u.PlanType, AsOf: time.Now()}
	add := func(label string, w AfpWindow, ultimate, short bool, dur time.Duration) {
		rem := -1.0
		if w.Quota > 0 {
			rem = (w.Quota - w.Used) / w.Quota
			if rem < 0 {
				rem = 0 // over-quota -> exhausted (0), not a negative that'd read as "unmeasured"
			}
		}
		var reset time.Time
		if w.ResetTime > 0 {
			reset = time.UnixMilli(w.ResetTime)
		}
		s.Windows = append(s.Windows, QuotaWindow{
			Label: label, Kind: "tokens",
			Used: w.Used, Total: w.Quota, RemainingPct: rem, ResetsAt: reset,
			Ultimate: ultimate, Short: short, Duration: dur,
		})
	}
	add("5h", u.AFPFiveHour, false, true, 5*time.Hour)
	add("daily", u.AFPDaily, false, false, 24*time.Hour)
	add("weekly", u.AFPWeekly, false, false, 7*24*time.Hour)
	add("monthly", u.AFPMonthly, true, false, 30*24*time.Hour)
	s.RemainingPct = ultimateRemaining(s.Windows)
	return s
}

// volcengineCredsFile mirrors the legacy <name>_apikey.json contents: the Ark
// API Key (chat) plus the Volcengine AK/SK (GetAFPUsage).
type volcengineCredsFile struct {
	APIKey    string `json:"api_key"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
}

// getAFPUsage calls the Volcengine signed OpenAPI GetAFPUsage and returns the
// 5h/daily/weekly/monthly AFP quota windows.
func getAFPUsage(ak, sk string) (*AfpUsage, error) {
	req, err := volcengineGet("GetAFPUsage", "2024-01-01", ak, sk, time.Now(), "")
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetAFPUsage: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GetAFPUsage HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 300))
	}
	var wrap struct {
		ResponseMetadata struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"ResponseMetadata"`
		Result AfpUsage `json:"Result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("parse GetAFPUsage: %w", err)
	}
	// Volcengine OpenAPI reports business errors as HTTP 200 + a non-empty
	// ResponseMetadata.Error (e.g. invalid AK/SK). Only Code/Message from the
	// response body enter the error string — never the signing SecretKey.
	if e := wrap.ResponseMetadata.Error; e.Code != "" || e.Message != "" {
		return nil, fmt.Errorf("GetAFPUsage: %s: %s", e.Code, e.Message)
	}
	return &wrap.Result, nil
}

// ValidateVolcengineAKSK verifies a Volcengine AccessKey/SecretKey pair by calling
// the signed GetAFPUsage control-plane API. nil means the pair signs correctly and
// the control plane accepts them; a non-nil error means they are invalid (or the
// endpoint unreachable). Used by `login volcengine` to validate the AK/SK half of
// the triple before saving (the Ark API Key is validated separately via /models).
func ValidateVolcengineAKSK(ak, sk string) error {
	_, err := getAFPUsage(ak, sk)
	return err
}

// resolveVolcengineAKSK picks the AccessKey/SecretKey to sign GetAFPUsage with.
// Bound keys (cfg.AccessKey/SecretKey, the pool-bound path) are used EXCLUSIVELY
// - the on-disk store is never consulted, preserving per-account isolation (a
// sibling virtual's file must not leak into this account's quota call). When
// unbound (the single-account / pre-pool path), the legacy store at
// cfg.VolcengineCredFile is read for backward compatibility — through
// credstore, so keychain mode covers this path too (lazy migration applies).
func (p *VolcengineProvider) resolveAKSK() (ak, sk string, err error) {
	if p.cfg.AccessKey != "" && p.cfg.SecretKey != "" {
		return p.cfg.AccessKey, p.cfg.SecretKey, nil
	}
	if p.cfg.VolcengineCredFile == "" {
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	b, err := credstore.NewRef(p.cfg.VolcengineCredFile).Load()
	if err != nil {
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	var c volcengineCredsFile
	if err := json.Unmarshal(b, &c); err != nil || c.AccessKey == "" || c.SecretKey == "" {
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	return c.AccessKey, c.SecretKey, nil
}

// Quota calls GetAFPUsage (signed, AK/SK) and parses the AFP windows. When
// bound (pool virtual) the virtual's own AK/SK are used; otherwise the legacy
// file is read. Returns BillingUnknown if AK/SK aren't configured or the call
// fails (never a non-nil error).
func (p *VolcengineProvider) Quota() (*QuotaSnapshot, error) {
	ak, sk, err := p.resolveAKSK()
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: "AK/SK not configured"}, nil
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		return &QuotaSnapshot{Billing: BillingUnknown, Err: err.Error()}, nil
	}
	return ParseVolcengineQuota(u), nil
}
