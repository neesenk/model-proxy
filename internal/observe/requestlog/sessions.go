package requestlog

import (
	"bufio"
	"encoding/json"
	"sort"
	"strings"
)

// Usage is the token tuple extracted from one recorded response body, in the
// same buckets ComputeCost consumes (cache read/creation separate from input).
type Usage struct {
	Input         uint64
	Output        uint64
	CacheRead     uint64
	CacheCreation uint64
}

// usageProbe parses the usage object of any of the three wire protocols out
// of one JSON document — a whole non-streaming body, or one SSE data payload
// (anthropic message_start wraps its usage inside "message").
type usageProbe struct {
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
	Message *struct {
		Usage *struct {
			InputTokens   uint64 `json:"input_tokens"`
			OutputTokens  uint64 `json:"output_tokens"`
			CacheCreation uint64 `json:"cache_creation_input_tokens"`
			CacheRead     uint64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
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

// ExtractUsage parses one recorded response body in any of the three wire
// protocols: anthropic usage.input_tokens*, chat usage.prompt_tokens
// (prompt_tokens_details cached), responses response.usage.*. Streaming
// (SSE) bodies — the shape every streaming request records — are parsed
// frame by frame (data lines joined per the spec, dispatched on blank
// lines) and merged per field by maximum: usage frames repeat with
// cumulative counters (anthropic message_delta, per-chunk chat usage), so
// the maximum across frames is the stream's final usage. Bodies without a
// usage object (errors, truncation) yield the zero Usage.
//
// Cache accounting is normalized to anthropic buckets: openai's
// prompt_tokens/input_tokens INCLUDE their cached_tokens detail, so the
// cached count is subtracted from Input (clamped at zero) — ComputeCost
// prices the buckets independently and would otherwise bill cached input at
// both the full and the cache rate.
func ExtractUsage(body string) Usage {
	if p, ok := parseUsageJSON(body); ok {
		return usageFrom(p)
	}
	return extractSSEUsage(body)
}

func parseUsageJSON(doc string) (*usageProbe, bool) {
	var p usageProbe
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return nil, false
	}
	return &p, true
}

func usageFrom(p *usageProbe) Usage {
	switch {
	case p.Message != nil && p.Message.Usage != nil:
		// anthropic message_start frame: usage nested under "message".
		return Usage{
			Input:         p.Message.Usage.InputTokens,
			Output:        p.Message.Usage.OutputTokens,
			CacheCreation: p.Message.Usage.CacheCreation,
			CacheRead:     p.Message.Usage.CacheRead,
		}
	case p.Usage != nil && (p.Usage.InputTokens > 0 || p.Usage.OutputTokens > 0 || p.Usage.PromptTokens > 0):
		u := Usage{
			Input:         p.Usage.InputTokens,
			Output:        p.Usage.OutputTokens,
			CacheCreation: p.Usage.CacheCreation,
			CacheRead:     p.Usage.CacheRead,
		}
		if p.Usage.PromptTokens > 0 {
			cached := uint64(0)
			if p.Usage.PromptDetails != nil {
				cached = p.Usage.PromptDetails.CachedTokens
			}
			u.Input = subClamped(p.Usage.PromptTokens, cached)
			u.Output = p.Usage.CompletionToken
			u.CacheRead = cached
		}
		return u
	case p.Response != nil && p.Response.Usage != nil:
		cached := uint64(0)
		if p.Response.Usage.InputDetails != nil {
			cached = p.Response.Usage.InputDetails.CachedTokens
		}
		return Usage{
			Input:     subClamped(p.Response.Usage.InputTokens, cached),
			Output:    p.Response.Usage.OutputTokens,
			CacheRead: cached,
		}
	}
	return Usage{}
}

func subClamped(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}

// extractSSEUsage walks an SSE text body frame by frame and merges every
// frame's usage by per-field maximum (see ExtractUsage). Only reached when
// the whole body is not a JSON document, i.e. genuine stream text.
func extractSSEUsage(body string) Usage {
	var u Usage
	var data []string
	merge := func() {
		if len(data) == 0 {
			return
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		if p, ok := parseUsageJSON(payload); ok {
			frame := usageFrom(p)
			u.Input = max(u.Input, frame.Input)
			u.Output = max(u.Output, frame.Output)
			u.CacheRead = max(u.CacheRead, frame.CacheRead)
			u.CacheCreation = max(u.CacheCreation, frame.CacheCreation)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	// A single SSE data line can carry an entire content delta; the cap is
	// the body itself so no line can overflow.
	sc.Buffer(make([]byte, 0, 64<<10), len(body)+1)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			merge()
		case strings.HasPrefix(line, "data:"):
			if payload := strings.TrimPrefix(line[len("data:"):], " "); payload != "[DONE]" {
				data = append(data, payload)
			}
		}
	}
	merge() // EOF dispatches a trailing unterminated frame
	return u
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
	// UsageOnly: usage is parsed as each line is read and the bodies are
	// dropped before the top-K heap retains the record — scanning 2000
	// records must not pin 2000 full (up-to-5MiB-each) bodies in memory.
	records, err := query(dir, Filter{Limit: scanLimit, UsageOnly: true}, false, nil)
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
		u := r.ParsedUsage
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
