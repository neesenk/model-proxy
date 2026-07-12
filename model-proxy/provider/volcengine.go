package provider

import (
	"fmt"
	"net/http"
	"regexp"
	"time"
)

// VolcengineProvider implements the Volcengine Ark (火山方舟) provider, including
// the "Agent Plan" subscription tier. A single Ark API key authenticates both
// protocols from one config entry:
//   - openai_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan/v3) →
//     /chat/completions, /responses, /models — uses Authorization: Bearer.
//   - anthropic_base_url (e.g. https://ark.cn-beijing.volces.com/api/plan/compatible)
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

func (p *VolcengineProvider) Login() error                   { return p.cfg.LoginFn() }
func (p *VolcengineProvider) Logout() error                  { return p.cfg.LogoutFn() }
func (p *VolcengineProvider) Usage() (any, error)            { return p.cfg.UsageFn() }
func (p *VolcengineProvider) Quota() (*QuotaSnapshot, error) { return p.cfg.QuotaOrUnknown() }
func (p *VolcengineProvider) Surplus(snap *QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}
func (p *VolcengineProvider) FetchModels() ([]string, error) {
	if p.cfg.FetchModelsFn != nil {
		return p.cfg.FetchModelsFn()
	}
	return nil, fmt.Errorf("FetchModelsFn not configured")
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
