// guard_adjudication.go — the app-side wiring of the AI second-opinion
// channel for guard pattern hits (guard.adjudicate): the forward.Adjudicator
// adapter, the adjudicate.RuntimeConfig/Sink implementations, and the
// provider-direct model Caller. The service itself is process-lifetime
// (internal/adjudicate), created and started in NewProxyWithStatePath,
// drained in Close BEFORE the security audit log shuts down so verdict-side
// audit records cannot enqueue into a drained logger.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"model-proxy/internal/adjudicate"
	"model-proxy/internal/appapi"
	"model-proxy/internal/forward"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/seclog"
	"model-proxy/internal/probe"
)

// startAdjudication builds and starts the process-lifetime adjudication
// service. It is ALWAYS created (workers idle when the config gate is off)
// so a reload can turn the channel on, and so persisted session blocks keep
// being enforced regardless of the current guard.adjudicate state — blocks
// clear only via Unblock (CLI/WebUI), by design. stateDir is derived from the
// injected state path (the CacheStatePath recipe), never from the real HOME —
// every Proxy, tests included, owns isolated adjudication state.
func (p *Proxy) startAdjudication(stateDir string) {
	a := p.cfg.Guard.Adjudicate
	p.adjudication = adjudicate.New(adjudicate.Options{
		Workers:  a.WorkerCount(),
		MaxQueue: a.QueueCap(),
		CacheMax: a.CacheCapacity(),
		StateDir: stateDir,
	})
	p.adjudication.Start(p, adjudicationCaller{p: p}, adjudicationSink{p: p})
}

// adjudicationStateDir derives the adjudication state directory from the
// injected state path — the same directory as quota_state.json and
// cache_state.json (the CacheStatePath recipe), so tests stay hermetic
// instead of touching the real ~/.model-proxy. An empty state path keeps
// the adjudication state in memory.
func adjudicationStateDir(statePath string) string {
	if statePath == "" {
		return ""
	}
	return filepath.Dir(statePath)
}

// adjudicatorAdapter projects the service onto the forward pipeline port.
type adjudicatorAdapter struct{ svc *adjudicate.Service }

func (a adjudicatorAdapter) Enqueue(ga forward.GuardAdjudication) bool {
	return a.svc.Enqueue(adjudicate.Job{
		Kind:      ga.Kind,
		Rule:      ga.Rule,
		Hit:       ga.Hit,
		Pre:       ga.Pre,
		Post:      ga.Post,
		RequestID: ga.RequestID,
		SessionID: ga.SessionID,
		Agent:     ga.Agent,
		Proto:     ga.Proto,
		Exposed:   ga.Exposed,
		Action:    ga.Action,
		Ts:        ga.Ts,
	})
}

func (a adjudicatorAdapter) SessionBlocked(sessionID string) (rule, requestID string, blocked bool) {
	bl, ok := a.svc.Blocked(sessionID)
	return bl.Rule, bl.RequestID, ok
}

// AdjudicationConfig implements adjudicate.RuntimeConfig: the CURRENT
// generation's model, per-call timeout and session-block switch, resolved
// under a brief read lock (never held across the model call itself).
func (p *Proxy) AdjudicationConfig() (model string, timeout time.Duration, blockSession, enabled bool) {
	p.mu.RLock()
	cfg := p.cfg
	p.mu.RUnlock()
	if cfg == nil || !cfg.Guard.AdjudicateEnabled() {
		return "", 0, false, false
	}
	a := cfg.Guard.Adjudicate
	return a.Model, a.TimeoutDuration(), a.BlockSession, true
}

// adjudicationCaller performs one provider-direct model call (the probe
// exchange recipe: impl.RewriteRequest → auth → send). It deliberately never
// goes through the forward pipeline: the judged snippet contains the matched
// pattern and would re-trigger the guard (self-recursion), and internal
// adjudication traffic must not pollute the request log, cache or stats.
type adjudicationCaller struct{ p *Proxy }

// judgeSystem is the fixed classifier prompt.
const judgeSystem = `You are the security classifier of an LLM gateway. A secret-detection rule matched a snippet of an outbound request body. Decide whether the snippet shows a REAL credential security risk, or benign content (test fixture, dummy/placeholder key, documentation example, regex source code, benign file-path reference).

Answer STRICTLY as JSON, nothing else:
{"risk":"high"|"low","reason":"short justification, max 40 characters"}

"high" = a real, usable credential is being sent out (live API key or token, real private-key material, or a tool call genuinely reading credential files for exfiltration).
"low" = placeholder/example/fixture/documentation/variable name, a masked or redacted value, or a benign path mention in legitimate coding work.`

// adjudicationVerdictJSON pulls the first {...} object out of a model reply.
var adjudicationVerdictJSON = regexp.MustCompile(`\{[^{}]*\}`)

type adjudicationReply struct {
	Risk   string `json:"risk"`
	Reason string `json:"reason"`
}

func (c adjudicationCaller) Adjudicate(ctx context.Context, model string, j adjudicate.Job) (verdict, reason string, usage adjudicate.Usage, err error) {
	snap := c.p.SnapshotRuntime()
	targets := snap.ExpandedRoutes[model]
	if len(targets) == 0 {
		return "", "", adjudicate.Usage{}, fmt.Errorf("adjudicate model %q has no route", model)
	}
	var kindLine string
	switch j.Kind {
	case forward.AdjudicationKindPath:
		kindLine = "a sensitive-path hit inside a TOOL CALL (the agent is invoking a tool with this argument)"
	default:
		kindLine = "a secret-pattern hit"
	}
	user := fmt.Sprintf("rule: %s\nkind: %s\n\ncontext (the match is between [[ and ]]):\n%s[[%s]]%s",
		j.Rule, kindLine, clip(j.Pre), j.Hit, clip(j.Post))
	prompt, err := json.Marshal(map[string]any{
		// max_tokens is generous (1024), not minimal: judging models are often
		// thinking models whose visible text arrives only after a thinking
		// block — a tight cap truncates to thinking-only replies.
		"max_tokens": 1024,
		"system":     judgeSystem,
		"messages":   []map[string]string{{"role": "user", "content": user}},
	})
	if err != nil {
		return "", "", adjudicate.Usage{}, err
	}
	// Target failover, mirroring the forward pipeline's route semantics at
	// smaller scale: try up to 3 route targets in order, skipping providers
	// without an anthropic_base_url (the call uses the /v1/messages leg); the
	// first decisive reply wins. One slow provider must not sink the verdict.
	const maxAttempts = 3
	var lastErr error
	attempted := 0
	for _, t := range targets {
		if attempted >= maxAttempts {
			break
		}
		provCfg, okCfg := snap.Cfg.Providers[t.Provider]
		impl, okImpl := snap.Providers[t.Provider]
		if !okCfg || !okImpl {
			continue
		}
		base := provCfg.AnthropicBaseURL
		if base == "" {
			continue
		}
		body, err := withModelField(prompt, t.Model)
		if err != nil {
			return "", "", adjudicate.Usage{}, err
		}
		attempted++
		rep, err := probe.Do(ctx, c.p.client, provCfg, impl, probe.Request{
			BaseURL: base,
			Path:    "/v1/messages",
			Body:    body,
		})
		if err != nil {
			lastErr = fmt.Errorf("adjudication call (%s): %w", t.Provider, err)
			continue
		}
		if rep.Status < 200 || rep.Status >= 300 {
			lastErr = fmt.Errorf("adjudication call (%s): status %d: %s", t.Provider, rep.Status, truncateAdjudication(string(rep.Body), 200))
			continue
		}
		usage = extractAnthropicUsage(rep.Body)
		text := extractAnthropicText(rep.Body)
		if text == "" {
			lastErr = fmt.Errorf("adjudication call (%s): empty model reply", t.Provider)
			continue
		}
		m := adjudicationVerdictJSON.FindString(text)
		if m == "" {
			lastErr = fmt.Errorf("adjudication reply (%s) has no JSON verdict: %s", t.Provider, truncateAdjudication(text, 120))
			continue
		}
		var r adjudicationReply
		if err := json.Unmarshal([]byte(m), &r); err != nil {
			lastErr = fmt.Errorf("adjudication verdict JSON (%s): %w", t.Provider, err)
			continue
		}
		switch strings.ToLower(strings.TrimSpace(r.Risk)) {
		case "high":
			return adjudicate.VerdictHigh, r.Reason, usage, nil
		case "low":
			return adjudicate.VerdictLow, r.Reason, usage, nil
		}
		lastErr = fmt.Errorf("adjudication verdict (%s) %q not high/low", t.Provider, r.Risk)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("adjudicate model %q: no route target with an anthropic_base_url", model)
	}
	return "", "", usage, lastErr
}

// withModelField rewrites the marshaled prompt body's model field onto one
// target's upstream model id (the route's per-target names differ).
func withModelField(prompt []byte, model string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(prompt, &m); err != nil {
		return nil, err
	}
	m["model"] = model
	return json.Marshal(m)
}

// extractAnthropicText joins the text blocks of a non-streaming
// /v1/messages reply (thinking blocks are skipped).
func extractAnthropicText(body []byte) string {
	var rep struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body, &rep) != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range rep.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func truncateAdjudication(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// extractAnthropicUsage reads the usage block of a non-streaming
// /v1/messages reply (input_tokens/output_tokens).
func extractAnthropicUsage(body []byte) adjudicate.Usage {
	var rep struct {
		Usage struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &rep) != nil {
		return adjudicate.Usage{}
	}
	return adjudicate.Usage{InputTokens: rep.Usage.InputTokens, OutputTokens: rep.Usage.OutputTokens}
}

// clip trims a context window to sane bytes for the prompt.
func clip(s string) string {
	const max = 1024
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// adjudicationSink turns verdicts into the observation surface: counters,
// live events and security audit records, mirroring the classic immediate
// record the forward path emits (plus the verdict fields).
type adjudicationSink struct{ p *Proxy }

func (s adjudicationSink) emit(j adjudicate.Job, kind string, verdict, reason string) {
	p := s.p
	field := "secrets="
	if kind == seclog.KindPath {
		field = "paths="
	}
	if p.metrics != nil {
		p.metrics.Inc("guard", j.Rule, counters.EvGuardHits)
		p.metrics.Inc("guard", "adjudicated_"+verdict, counters.EvGuardHits)
	}
	detail := field + j.Rule + " action=" + j.Action + " verdict=" + verdict
	if reason != "" {
		detail += " reason=" + reason
	}
	p.events.Publish(observeevents.Event{
		Type:      "guard",
		Ts:        time.Now().UnixMilli(),
		RequestID: j.RequestID,
		SessionID: j.SessionID,
		Agent:     j.Agent,
		Protocol:  j.Proto,
		Exposed:   j.Exposed,
		Detail:    detail,
	})
	snap := p.SnapshotRuntime()
	forward.AuditGuardHit(snap.SecLog, kind, []string{j.Rule}, j.Action, j.RequestID, j.Agent, j.Proto, j.Exposed, verdict, reason)
}

// High records the hit like the classic immediate record plus verdict=high.
// The kind must mirror the hit channel (secret/path) — seclog.KindSecret on a
// path hit would misfile the audit record.
func (s adjudicationSink) High(j adjudicate.Job, reason, model string) {
	s.emit(j, adjudicationSeclogKind(j.Kind), adjudicate.VerdictHigh, reason)
}

// Low records the suppressed verdict to the audit log ONCE per unique
// content (cached occurrences only bump the counter): the Activity feed and
// `audit` CLI keep a durable record of what was judged benign — the verdict
// field is the suppression marker — while history echo cannot reflood the
// log. No live event: the operator-facing event stream stays quiet for
// benign content, which is the point of the channel.
func (s adjudicationSink) Low(j adjudicate.Job, reason, model string, cached bool) {
	if s.p.metrics != nil {
		s.p.metrics.Inc("guard", "adjudicated_low", counters.EvGuardHits)
	}
	if cached {
		return
	}
	kind := seclog.KindSecret
	if j.Kind == forward.AdjudicationKindPath {
		kind = seclog.KindPath
	}
	snap := s.p.SnapshotRuntime()
	forward.AuditGuardHit(snap.SecLog, kind, []string{j.Rule}, j.Action, j.RequestID, j.Agent, j.Proto, j.Exposed, adjudicate.VerdictLow, reason)
}

// Failed re-emits the classic immediate record for error/skipped verdicts
// (fail-open: an unavailable judge must never silence the guard).
func (s adjudicationSink) Failed(j adjudicate.Job, verdict, detail string) {
	s.emit(j, adjudicationSeclogKind(j.Kind), verdict, detail)
}

// adjudicationSeclogKind maps an adjudication hit kind onto the audit-log
// record kind (unknown kinds keep the secret channel).
func adjudicationSeclogKind(kind string) string {
	if kind == forward.AdjudicationKindPath {
		return seclog.KindPath
	}
	return seclog.KindSecret
}

// adjudicatorPort returns the forward pipeline adapter, or nil when the
// service is absent (isolated tests) — the pipeline then keeps the classic
// immediate-record behavior for every guard hit.
func (p *Proxy) adjudicatorPort() forward.Adjudicator {
	if p.adjudication == nil {
		return nil
	}
	return adjudicatorAdapter{svc: p.adjudication}
}

// adjudicationBlocks implements admin.Ports.AdjudicationBlocks. The snapshot
// type is already the appapi DTO (type alias), so this is a direct return.
func (p *Proxy) adjudicationBlocks() []appapi.SecurityBlock {
	if p.adjudication == nil {
		return nil
	}
	return p.adjudication.Blocks()
}

// adjudicationUnblock implements admin.Ports.AdjudicationUnblock.
func (p *Proxy) adjudicationUnblock(sessionID string) bool {
	if p.adjudication == nil {
		return false
	}
	return p.adjudication.Unblock(sessionID)
}

// adjudicationStats implements admin.Ports.AdjudicationStats.
func (p *Proxy) adjudicationStats() appapi.SecurityAdjudicationStats {
	if p.adjudication == nil {
		return appapi.SecurityAdjudicationStats{}
	}
	calls, in, out := p.adjudication.Stats()
	return appapi.SecurityAdjudicationStats{Calls: calls, InputTokens: in, OutputTokens: out}
}

// adjudicationRecent implements admin.Ports.AdjudicationRecent. Same alias
// story as adjudicationBlocks: Result IS the DTO.
func (p *Proxy) adjudicationRecent() []appapi.SecurityAdjudication {
	if p.adjudication == nil {
		return nil
	}
	return p.adjudication.Recent()
}
