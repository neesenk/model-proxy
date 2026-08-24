package requestlog

import (
	"encoding/json"
	"sort"
)

// Usage is the token tuple extracted from one recorded response body, in the
// same buckets ComputeCost consumes (cache read/creation separate from input).
type Usage struct {
	Input         uint64
	Output        uint64
	CacheRead     uint64
	CacheCreation uint64
}

// ExtractUsage parses one recorded response body in any of the three wire
// protocols: anthropic usage.input_tokens*, chat usage.prompt_tokens
// (prompt_tokens_details cached), responses response.usage.*. Bodies without
// a usage object (errors, truncation) yield the zero Usage.
func ExtractUsage(body string) Usage {
	var probe struct {
		Usage *struct {
			InputTokens     uint64 `json:"input_tokens"`
			OutputTokens    uint64 `json:"output_tokens"`
			PromptTokens    uint64 `json:"prompt_tokens"`
			CompletionToken uint64 `json:"completion_tokens"`
			CacheCreation   uint64 `json:"cache_creation_input_tokens"`
			CacheRead       uint64 `json:"cache_read_input_tokens"`
			PromptDetails   *struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Response *struct {
			Usage *struct {
				InputTokens  uint64 `json:"input_tokens"`
				OutputTokens uint64 `json:"output_tokens"`
				InputDetails *struct {
					CachedTokens uint64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal([]byte(body), &probe) != nil {
		return Usage{}
	}
	switch {
	case probe.Usage != nil && (probe.Usage.InputTokens > 0 || probe.Usage.OutputTokens > 0 || probe.Usage.PromptTokens > 0):
		u := Usage{
			Input:         probe.Usage.InputTokens,
			Output:        probe.Usage.OutputTokens,
			CacheCreation: probe.Usage.CacheCreation,
			CacheRead:     probe.Usage.CacheRead,
		}
		if probe.Usage.PromptTokens > 0 {
			u.Input = probe.Usage.PromptTokens
			u.Output = probe.Usage.CompletionToken
			if probe.Usage.PromptDetails != nil {
				u.CacheRead = probe.Usage.PromptDetails.CachedTokens
			}
		}
		return u
	case probe.Response != nil && probe.Response.Usage != nil:
		u := Usage{
			Input:  probe.Response.Usage.InputTokens,
			Output: probe.Response.Usage.OutputTokens,
		}
		if probe.Response.Usage.InputDetails != nil {
			u.CacheRead = probe.Response.Usage.InputDetails.CachedTokens
		}
		return u
	}
	return Usage{}
}

// SessionSummary aggregates one client session's committed requests: time
// span, models/providers touched, token totals, and (via the caller's cost
// lookup) the equivalent USD cost. Shadow records count toward their session
// (they are real upstream spend) and are reported separately.
type SessionSummary struct {
	SessionID      string   `json:"session_id"`
	FirstTs        string   `json:"first_ts"`
	LastTs         string   `json:"last_ts"`
	Requests       int      `json:"requests"`
	ShadowRequests int      `json:"shadow_requests"`
	Errors         int      `json:"errors"`
	Providers      []string `json:"providers"`
	Models         []string `json:"models"`
	Usage          Usage    `json:"usage"`
	CostUSD        float64  `json:"cost_usd"`
}

// SessionSummaries groups the newest scanLimit records by session id and
// keeps the limit most-recently-active sessions. costOf (nil = CostUSD stays
// zero) receives the upstream model and per-session usage; mirrors the
// analytics cost path (pricing.Resolve + ComputeCost at the caller).
func SessionSummaries(dir string, scanLimit, limit int, costOf func(model string, usage Usage) float64) ([]SessionSummary, error) {
	records, err := QueryRecords(dir, Filter{Limit: scanLimit})
	if err != nil {
		return nil, err
	}
	type agg struct {
		summary  SessionSummary
		byModel  map[string]Usage
		provSeen map[string]bool
	}
	bySession := map[string]*agg{}
	// records arrive newest-first: the first record seen per session sets
	// LastTs; every later one refreshes FirstTs.
	for _, r := range records {
		if r.SessionID == "" {
			continue
		}
		a := bySession[r.SessionID]
		if a == nil {
			a = &agg{
				summary:  SessionSummary{SessionID: r.SessionID, LastTs: r.Ts},
				byModel:  map[string]Usage{},
				provSeen: map[string]bool{},
			}
			bySession[r.SessionID] = a
		}
		a.summary.FirstTs = r.Ts
		a.summary.Requests++
		if r.Shadow {
			a.summary.ShadowRequests++
		}
		if r.Status >= 400 {
			a.summary.Errors++
		}
		if !a.provSeen[r.Provider] {
			a.provSeen[r.Provider] = true
			// Records iterate newest-first: plain append keeps the most
			// recently active provider first.
			a.summary.Providers = append(a.summary.Providers, r.Provider)
		}
		model := r.UpstreamModel
		if model == "" {
			model = r.CalledModel
		}
		if _, ok := a.byModel[model]; !ok {
			a.byModel[model] = Usage{}
		}
		u := ExtractUsage(r.ResponseBody)
		m := a.byModel[model]
		m.Input += u.Input
		m.Output += u.Output
		m.CacheRead += u.CacheRead
		m.CacheCreation += u.CacheCreation
		a.byModel[model] = m
		a.summary.Usage.Input += u.Input
		a.summary.Usage.Output += u.Output
		a.summary.Usage.CacheRead += u.CacheRead
		a.summary.Usage.CacheCreation += u.CacheCreation
	}
	out := make([]SessionSummary, 0, len(bySession))
	for _, a := range bySession {
		for model, u := range a.byModel {
			a.summary.Models = append(a.summary.Models, model)
			if costOf != nil {
				a.summary.CostUSD += costOf(model, u)
			}
		}
		sort.Strings(a.summary.Models)
		out = append(out, a.summary)
	}
	// Most recently active first.
	sort.Slice(out, func(i, j int) bool { return out[i].LastTs > out[j].LastTs })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
