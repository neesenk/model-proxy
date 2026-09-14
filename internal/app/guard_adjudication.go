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
	p.adjudication.Start(p, adjudicationCaller{p: p, xchg: scheduledExchange{p: p}}, adjudicationSink{p: p})
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

// ContentBlocked projects the persisted repeat-interception index (sha256 of
// hit bytes → the original high verdict's attribution). Secret-kind hits
// only — the service never records path literals into it.
func (a adjudicatorAdapter) ContentBlocked(hit string) (kind, rule, reason, evidence, model string, blocked bool) {
	bc, ok := a.svc.ContentBlocked(hit)
	if !ok {
		return "", "", "", "", "", false
	}
	return bc.Kind, bc.Rule, bc.Reason, bc.Evidence, bc.Model, true
}

// BlockSession implements the exact-match interception path: the block table
// is the same one high verdicts use (persists until an explicit unblock).
// reason carries the operator attribution (credential source label + masked
// key display) recorded on the block for the Security page.
func (a adjudicatorAdapter) BlockSession(sessionID, rule, requestID, reason string) {
	a.svc.Block(sessionID, adjudicate.Block{
		Kind:      adjudicate.KindSecret,
		Rule:      rule,
		Reason:    reason,
		RequestID: requestID,
		Ts:        time.Now().UnixMilli(),
	})
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

// adjudicationCaller performs one judge model call through the shared
// scheduling seam (modelExchange → Manager ordering + cooldown skip + health
// recording → probe.Do transport). It deliberately never goes through the
// forward transport pipeline: the judged snippet contains the matched
// pattern and would re-trigger the guard (self-recursion), and internal
// adjudication traffic must not pollute the request log, cache or stats.
// This struct owns the judge domain only: prompt shape and verdict parsing.
type adjudicationCaller struct {
	p    *Proxy
	xchg modelExchange
}

// judgeSystem is the fixed three-level classifier prompt. high = block the
// session, medium = record only, low = the ignored tier; the model must also
// cite the factual basis for the call (scrubbed + capped before storage).
const judgeSystem = `You are the security classifier of an LLM gateway. A secret-detection rule matched a snippet of an outbound request body. Classify the risk and justify it from what the snippet actually shows.

Answer STRICTLY as JSON, nothing else:
{"risk":"high"|"medium"|"low","reason":"judgment logic, max 200 characters","evidence":"factual basis in the snippet, max 300 characters"}

"high" = a real, usable credential is being sent out: a live API key or token, real private-key material, or a tool call genuinely reading/exfiltrating credential files (e.g. cat/cp/tar of ~/.ssh or keychain paths).
"medium" = risk-shaped but not confirmable from the snippet: a tool call or command touching credential paths/files without verifiable live material, operational context where a real secret may be involved, or partial/ambiguous key material.
"low" = benign content: placeholder/example/fixture/dummy key, documentation or regex source code, variable name, masked or redacted value, plain path mention in legitimate coding work.`

// adjudicationVerdictJSON pulls the first {...} object out of a model reply.
var adjudicationVerdictJSON = regexp.MustCompile(`\{[^{}]*\}`)

type adjudicationReply struct {
	Risk     string `json:"risk"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
}

func (c adjudicationCaller) Adjudicate(ctx context.Context, model string, j adjudicate.Job) (verdict, reason, evidence string, usage adjudicate.Usage, err error) {
	snap := c.p.SnapshotRuntime()
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
		// The judge's reply is terse JSON: extended thinking is pure latency
		// (measured 15s vs 2s on the flash tier) and burns the call budget.
		// Spec-standard field — honoring endpoints skip thinking, others
		// ignore it or reject the request and the failover chain moves on.
		"thinking": map[string]string{"type": "disabled"},
		"system":   judgeSystem,
		"messages": []map[string]string{{"role": "user", "content": user}},
	})
	if err != nil {
		return "", "", "", adjudicate.Usage{}, err
	}
	// The reply contract is judge domain: an empty reply, a missing/unparseable
	// verdict JSON or an out-of-enum risk fails the target (the exchange then
	// fails over to the next one, exactly like a transport failure).
	var parsed adjudicationReply
	validate := func(rep exchangeResult) error {
		text := extractAnthropicText(rep.Body)
		if text == "" {
			return fmt.Errorf("empty model reply")
		}
		m := adjudicationVerdictJSON.FindString(text)
		if m == "" {
			return fmt.Errorf("no JSON verdict: %s", truncateAdjudication(text, 120))
		}
		var r adjudicationReply
		if err := json.Unmarshal([]byte(m), &r); err != nil {
			return fmt.Errorf("verdict JSON: %w", err)
		}
		switch strings.ToLower(strings.TrimSpace(r.Risk)) {
		case "high", "medium", "low":
			parsed = r
			return nil
		}
		return fmt.Errorf("verdict %q not high/medium/low", r.Risk)
	}
	rep, err := c.xchg.exchange(ctx, snap, model, prompt, validate)
	if err != nil {
		return "", "", "", adjudicate.Usage{}, err
	}
	usage = extractAnthropicUsage(rep.Body)
	switch strings.ToLower(strings.TrimSpace(parsed.Risk)) {
	case "high":
		return adjudicate.VerdictHigh, parsed.Reason, parsed.Evidence, usage, nil
	case "medium":
		return adjudicate.VerdictMedium, parsed.Reason, parsed.Evidence, usage, nil
	default:
		return adjudicate.VerdictLow, parsed.Reason, parsed.Evidence, usage, nil
	}
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

func (s adjudicationSink) emit(j adjudicate.Job, kind string, verdict, reason, evidence, model string) {
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
	forward.AuditGuardHit(snap.SecLog, forward.GuardAuditHit{
		Kind: kind, Names: []string{j.Rule}, Action: j.Action,
		RequestID: j.RequestID, SessionID: j.SessionID,
		Agent: j.Agent, Proto: j.Proto, Exposed: j.Exposed,
		Verdict: verdict, Reason: reason, Evidence: evidence, Model: model,
	})
}

// High records the hit like the classic immediate record plus verdict=high.
// The kind must mirror the hit channel (secret/path) — seclog.KindSecret on a
// path hit would misfile the audit record.
func (s adjudicationSink) High(j adjudicate.Job, reason, evidence, model string) {
	s.emit(j, adjudicationSeclogKind(j.Kind), adjudicate.VerdictHigh, reason, evidence, model)
}

// Medium is the record-only tier: an audit record with verdict=medium (no
// session block), ONCE per unique content — cached occurrences only bump the
// counter (the ring entry is the per-occurrence visibility).
func (s adjudicationSink) Medium(j adjudicate.Job, reason, evidence, model string) {
	if s.p.metrics != nil {
		s.p.metrics.Inc("guard", "adjudicated_medium", counters.EvGuardHits)
	}
	snap := s.p.SnapshotRuntime()
	forward.AuditGuardHit(snap.SecLog, forward.GuardAuditHit{
		Kind: adjudicationSeclogKind(j.Kind), Names: []string{j.Rule}, Action: j.Action,
		RequestID: j.RequestID, SessionID: j.SessionID,
		Agent: j.Agent, Proto: j.Proto, Exposed: j.Exposed,
		Verdict: adjudicate.VerdictMedium, Reason: reason, Evidence: evidence, Model: model,
	})
}

// Low is the ignored tier: the JSONL trail carries one trace per unique
// content (the store excludes low verdicts by design), with no live event —
// the operator-facing event stream stays quiet for benign content, which is
// the point of the channel. Cached occurrences write nothing at all.
func (s adjudicationSink) Low(j adjudicate.Job, reason, evidence, model string) {
	if s.p.metrics != nil {
		s.p.metrics.Inc("guard", "adjudicated_low", counters.EvGuardHits)
	}
	snap := s.p.SnapshotRuntime()
	forward.AuditGuardHit(snap.SecLog, forward.GuardAuditHit{
		Kind: adjudicationSeclogKind(j.Kind), Names: []string{j.Rule}, Action: j.Action,
		RequestID: j.RequestID, SessionID: j.SessionID,
		Agent: j.Agent, Proto: j.Proto, Exposed: j.Exposed,
		Verdict: adjudicate.VerdictLow, Reason: reason, Evidence: evidence, Model: model,
	})
}

// Failed re-emits the classic immediate record for error/skipped verdicts
// (fail-open: an unavailable judge must never silence the guard).
func (s adjudicationSink) Failed(j adjudicate.Job, verdict, detail string) {
	s.emit(j, adjudicationSeclogKind(j.Kind), verdict, detail, "", "")
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
	calls, in, out, lows := p.adjudication.Stats()
	return appapi.SecurityAdjudicationStats{Calls: calls, InputTokens: in, OutputTokens: out, LowVerdicts: lows}
}

// adjudicationEnabled implements admin.Ports.AdjudicationEnabled: the CURRENT
// generation's guard.adjudicate switch.
func (p *Proxy) adjudicationEnabled() bool {
	_, _, _, enabled := p.AdjudicationConfig()
	return enabled
}

// adjudicationRecent implements admin.Ports.AdjudicationRecent. Same alias
// story as adjudicationBlocks: Result IS the DTO.
func (p *Proxy) adjudicationRecent() []appapi.SecurityAdjudication {
	if p.adjudication == nil {
		return nil
	}
	return p.adjudication.Recent()
}
