package main

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

// detectAgent maps a request's User-Agent / known agent headers to a short,
// stable lowercase label (no spaces). Order matters: the most specific signals
// first. "unknown" = no UA at all; "other" = a UA we don't recognize (so an
// unrecognized client is still distinguishable from a headerless one).
func detectAgent(r *http.Request) string {
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
	// Pi CLI sends a "pi/<ver>" UA; match the prefix to avoid colliding with a
	// substring inside another product name.
	if strings.HasPrefix(ua, "pi/") || strings.Contains(ua, " pi/") {
		return "pi"
	}
	if ua == "" {
		return "unknown"
	}
	return "other"
}

// agentKey is the (agent, provider, model) key for the parallel agent pipeline.
// Provider is the virtual id for pooled providers (same as pmKey); Model is the
// rewrite-target upstream model.
type agentKey struct {
	Agent    string
	Provider string
	Model    string
}

// agentCount is one in-memory cumulative counter cell for the agent pipeline:
// request count + observed input/output tokens. All fields are mutated under
// agentCounter.mu (get-or-create and read-modify-write are one critical section,
// mirroring tokenCounter so two concurrent scanners can't lose an increment).
type agentCount struct {
	Requests uint64
	Input    uint64
	Output   uint64
}

type agentCounter struct {
	mu sync.Mutex
	m  map[agentKey]*agentCount
}

func newAgentCounter() *agentCounter {
	return &agentCounter{m: map[agentKey]*agentCount{}}
}

// incRequests bumps the request count for one (agent, provider, model) — called
// once per committed (served) target on the forward hot path.
func (a *agentCounter) incRequests(agent, provider, model string) {
	if a == nil || agent == "" {
		return
	}
	k := agentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &agentCount{}
		a.m[k] = c
	}
	c.Requests++
}

// addTokens accrues observed usage to (agent, provider, model). Called from the
// SSE usageScanner's commit path (the same bytes that feed the (provider, model)
// token counter), so agent token attribution matches the per-model totals.
func (a *agentCounter) addTokens(agent, provider, model string, u tokenUsage) {
	if a == nil || agent == "" {
		return
	}
	k := agentKey{Agent: agent, Provider: provider, Model: model}
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.m[k]
	if c == nil {
		c = &agentCount{}
		a.m[k] = c
	}
	c.Input += u.Input
	c.Output += u.Output
}

// snapshot returns a detached copy of all agent cells. Callers may read it
// without holding the lock (the flusher diffs it once per minute).
func (a *agentCounter) snapshot() map[agentKey]agentCount {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make(map[agentKey]agentCount, len(a.m))
	for k, v := range a.m {
		out[k] = *v
	}
	return out
}

func (a *agentCounter) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.m = map[agentKey]*agentCount{}
}
