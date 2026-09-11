package counters

import (
	"net/http"
	"strings"
	"sync"
)

// agent.go adds an AGENT dimension to call statistics: which client ("claude-
// code", "codex", "opencode", "pi", …) is making requests. The per-(provider,
// model) stats pipeline can't carry a categorical dimension without re-keying
// everything, so agent stats live in a PARALLEL pipeline — a separate in-memory
// counter keyed (agent, provider, model) flushed to a separate agent_buckets
// table — leaving the hot (provider, model) counters untouched.

// DetectAgent maps a request's User-Agent / known agent headers to a short,
// stable lowercase label (no spaces). Order matters: the most specific signals
// first. "unknown" = no UA at all; unrecognized UAs fall back to a label
// derived from the UA (uaLabel) so distinct unknown clients remain
// distinguishable; "other" survives only for a UA that is entirely
// whitespace (no signal whatsoever).
func DetectAgent(r *http.Request) string {
	ua := strings.ToLower(r.Header.Get("user-agent"))
	// Claude Code sends x-claude-code-session-id AND a "claude-cli/<ver>" UA;
	// either is a strong, unambiguous signal.
	if r.Header.Get("x-claude-code-session-id") != "" ||
		strings.Contains(ua, "claude-cli") || strings.Contains(ua, "claude-code") {
		return "claude-code"
	}
	if strings.Contains(ua, "codex") { // codex_cli_rs/<ver>
		return "codex"
	}
	if strings.Contains(ua, "opencode") {
		return "opencode"
	}
	// Pi CLI's LLM calls send "pi (<platform> <release>; <arch>)" (pi-ai
	// library, no version), while older/other pi surfaces send "pi/<ver> (...)",
	// possibly after other product tokens. Match the "pi" product token followed
	// by "/" or " (" so substrings like "pinecone/1.0" don't collide.
	if strings.HasPrefix(ua, "pi/") || strings.Contains(ua, " pi/") ||
		strings.HasPrefix(ua, "pi (") || strings.Contains(ua, " pi (") {
		return "pi"
	}
	if ua == "" {
		return "unknown"
	}
	return uaLabel(ua)
}

// uaLabel derives a fallback agent label from an unrecognized UA: normally the
// product token — the prefix up to the first '/', whitespace, '(' or ';',
// with anything outside [a-z0-9._-] dropped (versions stripped, truncated), so
// "curl/8.4.0" → "curl", "Go-http-client/2.0" → "go-http-client".
// When no usable token remains (e.g. "()/*"), the raw (lowercased) UA is used
// instead — truncated, with runs of whitespace collapsed to '-' — so the
// label still identifies the client; "other" only when even that is empty
// (whitespace-only UA).
const (
	uaLabelMaxLen    = 24
	uaFallbackMaxLen = 64
)

func uaLabel(ua string) string {
	cut := len(ua)
	for i := 0; i < len(ua); i++ {
		switch ua[i] {
		case '/', ' ', '(', ';':
			cut = i
			goto token
		}
	}
token:
	var b []byte
	for i := 0; i < cut && len(b) < uaLabelMaxLen; i++ {
		c := ua[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
			b = append(b, c)
		}
	}
	if len(b) == 0 {
		return uaFallback(ua)
	}
	return string(b)
}

// uaFallback labels a UA whose product token is unusable with the raw
// (already lowercased) UA itself: runs of whitespace collapsed to '-',
// control bytes dropped, truncated to uaFallbackMaxLen bytes, trimmed of
// trailing '-'. "other" only when nothing remains.
func uaFallback(ua string) string {
	var b []byte
	prevSpace := false
	for i := 0; i < len(ua) && len(b) < uaFallbackMaxLen; i++ {
		c := ua[i]
		switch {
		case c == ' ' || c == '\t':
			if !prevSpace && len(b) > 0 {
				b = append(b, '-')
			}
			prevSpace = true
		case c < 0x20 || c == 0x7f: // control chars carry no label value
			prevSpace = false
		default:
			b = append(b, c)
			prevSpace = false
		}
	}
	for len(b) > 0 && b[len(b)-1] == '-' {
		b = b[:len(b)-1]
	}
	if len(b) == 0 {
		return "other"
	}
	return string(b)
}

// AgentKey is the (agent, provider, model) key for the parallel agent pipeline.
// Provider is the virtual id for pooled providers (same as PMKey); Model is the
// rewrite-target upstream model.
type AgentKey struct {
	Agent    string
	Provider string
	Model    string
}

// AgentCount is one in-memory cumulative counter cell for the agent pipeline:
// request count + observed token usage (input/output plus the cache buckets,
// mirroring TokenUsage). All fields are mutated under AgentCounter.mu
// (get-or-create and read-modify-write are one critical section, mirroring
// TokenCounter so two concurrent scanners can't lose an increment).
type AgentCount struct {
	Requests      uint64
	Input         uint64
	Output        uint64
	CacheCreation uint64
	CacheRead     uint64
	LatencySum    uint64 // cumulative upstream latency ms (commit-only)
	TTFTSum       uint64 // cumulative upstream first-byte ms (commit-only; streaming responses)
	DurationSum   uint64 // cumulative full call wall-clock ms, send → end of body (commit-only)
	Failures      uint64 // all-targets-failed 502 count
}

type AgentCounter struct {
	mu sync.Mutex
	m  map[AgentKey]*AgentCount
}

func NewAgentCounter() *AgentCounter {
	return &AgentCounter{m: map[AgentKey]*AgentCount{}}
}

// Seed sets one (agent, provider, model) entry to a baseline (boot restore
// from SQLite, mirroring TokenCounter.Seed). Only Bootstrap calls it — before
// any request traffic — so it does not need to be a read-modify-write.
func (a *AgentCounter) Seed(k AgentKey, c AgentCount) {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := c
	a.m[k] = &v
}

// incRequests bumps the request count for one (agent, provider, model) — called
// once per committed (served) target on the forward hot path.
func (a *AgentCounter) IncRequests(agent, provider, model string) {
	if a == nil || agent == "" {
		return
	}
	k := AgentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &AgentCount{}
		a.m[k] = c
	}
	c.Requests++
}

// addTokens accrues observed usage to (agent, provider, model). Called from the
// SSE usageScanner's commit path (the same bytes that feed the (provider, model)
// token counter), so agent token attribution matches the per-model totals.
func (a *AgentCounter) AddTokens(agent, provider, model string, u TokenUsage) {
	if a == nil || agent == "" {
		return
	}
	k := AgentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &AgentCount{}
		a.m[k] = c
	}
	c.Input += u.Input
	c.Output += u.Output
	c.CacheCreation += u.CacheCreation
	c.CacheRead += u.CacheRead
}

// snapshot returns a detached copy of all agent cells. Callers may read it
// without holding the lock (the flusher diffs it once per minute).
func (a *AgentCounter) Snapshot() map[AgentKey]AgentCount {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[AgentKey]AgentCount, len(a.m))
	for k, v := range a.m {
		out[k] = *v
	}
	return out
}

// addLatency records the upstream response latency (ms) and time-to-first-
// byte (ms) for one (agent, provider, model) — called on commit alongside
// the metrics AddLatency (same commit-only semantics; ttft is 0 for
// non-streaming responses).
func (a *AgentCounter) AddLatency(agent, provider, model string, ms, ttftMs uint64) {
	if a == nil || agent == "" {
		return
	}
	k := AgentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &AgentCount{}
		a.m[k] = c
	}
	c.LatencySum += ms
	c.TTFTSum += ttftMs
}

// addDuration records the FULL call wall-clock (send → end of the streamed
// body) for one (agent, provider, model) — the tok/s denominator (see
// MetricsStore.AddDuration).
func (a *AgentCounter) AddDuration(agent, provider, model string, totalMs uint64) {
	if a == nil || agent == "" {
		return
	}
	k := AgentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &AgentCount{}
		a.m[k] = c
	}
	c.DurationSum += totalMs
}

// incFailure bumps the failure count (all-targets-failed 502) for an agent.
func (a *AgentCounter) IncFailure(agent, provider, model string) {
	if a == nil || agent == "" {
		return
	}
	k := AgentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &AgentCount{}
		a.m[k] = c
	}
	c.Failures++
}

func (a *AgentCounter) Reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m = map[AgentKey]*AgentCount{}
}
