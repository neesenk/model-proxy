package probe

import (
	"context"
	"net/http"
	"sync"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// Leg identifies one wire protocol probed by ProbeModelProtocols.
type Leg string

const (
	LegChat      Leg = "chat"
	LegAnthropic Leg = "anthropic"
	LegResponses Leg = "responses"
)

// legPath returns the canonical upstream path of a protocol leg.
func legPath(leg Leg) string {
	switch leg {
	case LegChat:
		return "/chat/completions"
	case LegAnthropic:
		return "/v1/messages"
	case LegResponses:
		return "/responses"
	}
	return ""
}

// Legs is every protocol leg in stable display/probe order.
var Legs = []Leg{LegChat, LegAnthropic, LegResponses}

// LegResult is one protocol leg's probe outcome. Probed=false means the leg
// was NOT probed because its base URL isn't configured (chat/responses need
// openai_base_url, anthropic needs anthropic_base_url) — classification treats
// it as definitionally unsupported. Status 0 with Err set means the request
// never reached an HTTP exchange (build/auth/network).
type LegResult struct {
	Leg     Leg
	Probed  bool
	Status  int
	Body    []byte
	Err     error
	Latency time.Duration
}

// ProbeModelProtocols probes the chat / anthropic / responses callability of
// ONE model on the provider's configured bases, concurrently, and returns one
// LegResult per leg (Legs order). Base/body rules:
//   - chat + responses legs target OpenAIBaseURL (unprobed when unset);
//     the anthropic leg targets AnthropicBaseURL (unprobed when unset) — a
//     provider declares anthropic support by configuring the base, so the
//     protocol is never fabricated on the openai base.
//   - when the impl's own ProbeRequest path matches the leg's canonical path
//     (e.g. codex's /responses dialect: input list + stream:true), the impl's
//     body wins over the generic minimal body — provider wire knowledge stays
//     in the provider package.
//
// Each leg goes through doCallability (so the max_completion_tokens retry
// applies per leg). Classification of the outcomes into yes/no/unknown
// verdicts is the caller's business (internal/runtime/wirecap).
func ProbeModelProtocols(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, model string) []LegResult {
	pr := impl.ProbeRequest(model)
	results := make([]LegResult, len(Legs))
	var wg sync.WaitGroup
	for i, leg := range Legs {
		wg.Add(1)
		go func(i int, leg Leg) {
			defer wg.Done()
			results[i] = probeLeg(ctx, client, prov, impl, pr, model, leg)
		}(i, leg)
	}
	wg.Wait()
	return results
}

func probeLeg(ctx context.Context, client *http.Client, prov configdomain.Provider, impl provider.Provider, pr provider.ProbeRequest, model string, leg Leg) LegResult {
	res := LegResult{Leg: leg}
	base := prov.OpenAIBaseURL
	if leg == LegAnthropic {
		base = prov.AnthropicBaseURL
	}
	if base == "" {
		return res // Probed=false: base not configured → definitionally unsupported
	}
	res.Probed = true

	path := legPath(leg)
	body := legBody(leg, model)
	if pr.Path == path && len(pr.Body) > 0 {
		body = pr.Body // provider-owned dialect wins (codex /responses)
	}
	rep, err := doCallability(ctx, client, prov, impl, Request{
		BaseURL: base,
		Method:  pr.Method,
		Path:    path,
		Body:    body,
	})
	res.Status, res.Body, res.Err, res.Latency = rep.Status, rep.Body, err, rep.Latency
	return res
}

// legBody returns the generic minimal probe body for a protocol leg.
func legBody(leg Leg, model string) []byte {
	switch leg {
	case LegChat:
		return provider.OpenAIProbeBody(model)
	case LegAnthropic:
		return provider.AnthropicProbeBody(model)
	case LegResponses:
		return provider.ResponsesProbeBody(model)
	}
	return nil
}

// PickModel picks the model id used in probe bodies: the provider's first
// configured model, else the first explicit route target pointing at it, else
// a derived route target for it. Shared by the daemon's capability probing and
// `wire record`.
func PickModel(cfg *configdomain.Config, derived map[string][]configdomain.RouteTarget, provName string) string {
	if ms := cfg.Providers[provName].Models; len(ms) > 0 {
		return ms[0]
	}
	for _, targets := range cfg.Routes {
		for _, t := range targets {
			if t.Provider == provName {
				return t.Model
			}
		}
	}
	for _, targets := range derived {
		for _, t := range targets {
			if t.Provider == provName {
				return t.Model
			}
		}
	}
	return ""
}
