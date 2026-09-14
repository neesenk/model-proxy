// guard.go — the pure per-request outbound guard decision shared by live
// forwarding and /debug/route, plus the security-audit enqueue helper.
package forward

import (
	"time"

	"model-proxy/internal/guard"
	"model-proxy/internal/observe/seclog"
)

// Guard hit kinds used by the adjudication channel (mirrors seclog kinds).
const (
	AdjudicationKindSecret = "secret"
	AdjudicationKindPath   = "path"
)

// maxAdjudicationsPerRequest bounds the jobs one request can enqueue per
// channel: a body stuffed with distinct secret-shaped strings must not turn
// into an unbounded model-call bill; past the cap the remaining names fail
// open to the classic immediate record.
const maxAdjudicationsPerRequest = 4

// GuardAdjudication is one guard pattern hit deferred to the AI
// second-opinion channel. Hit carries the matched bytes and Pre/Post the
// masked context windows — in memory only, never persisted or logged by the
// adjudication service.
type GuardAdjudication struct {
	Kind      string // "secret" | "path"
	Rule      string // pattern type name / path category name
	Hit       string
	Pre, Post string
	RequestID string
	SessionID string
	Agent     string
	Proto     string
	Exposed   string
	Action    string // configured action at hit time
	Ts        int64
}

// Adjudicator is the process-lifetime AI second-opinion service port
// (internal/app adapts internal/adjudicate). Nil — or the config gate off —
// keeps the classic immediate-record behavior for every guard hit.
type Adjudicator interface {
	// Enqueue offers one deferred hit; false = queue full/closed and the
	// caller must fail open (classic immediate record, verdict "skipped").
	Enqueue(a GuardAdjudication) bool
	// SessionBlocked reports a session block (high verdict or exact-match
	// interception; persists until unblocked via CLI/WebUI).
	SessionBlocked(sessionID string) (rule, requestID string, blocked bool)
	// BlockSession adds one session block directly — the exact-match
	// interception path (a configured credential appeared verbatim; zero
	// false positives by construction, no LLM round-trip). reason carries the
	// operator-facing attribution (credential source label + masked key
	// display — identity, never the value). Like high-verdict blocks it
	// persists until explicitly unblocked.
	BlockSession(sessionID, rule, requestID, reason string)
	// ContentBlocked reports that these raw SECRET-hit bytes were already
	// adjudicated high on an earlier request (persisted sha256 index, hash
	// only — the bytes never persist). The pipeline intercepts repeats
	// verbatim, the same treatment as the known-secret exact channel: record
	// (action block, the original verdict's reason/evidence), block the
	// session when present, reject with 400. Secret-kind hits only — path
	// literals repeat legitimately across contexts and stay per-occurrence.
	ContentBlocked(hit string) (kind, rule, reason, evidence, model string, blocked bool)
}

// RequestGuardDecision is the pure, per-request portion of the outbound guard
// contract shared by live forwarding and /debug/route. Observation side
// effects (counters, events, audit, and session-fragment state) stay in the
// live path; both callers nevertheless make the same secret/path decision and
// derive the same body that may participate in the response-cache key.
type RequestGuardDecision struct {
	ForwardBody []byte
	Secrets     []string
	StrongPath  []string
	WeakPath    []string
}

func EvaluateRequestGuard(cfg GuardConfig, scanner *guard.Scanner, body []byte) RequestGuardDecision {
	decision := RequestGuardDecision{ForwardBody: body}
	if scanner == nil {
		return decision
	}
	action := cfg.SecretsAction()
	pathsOn := cfg.PathsAction() != "off"
	switch {
	case action == "redact" && pathsOn:
		// One scan claims the secret spans once; names and the redacted body
		// both come from that single claimed table (no second full scan for
		// Redact). Paths classify the POST-REDACT body (unchanged contract):
		// redaction rewrites bytes, so a pre-redact gate/classification
		// cannot be reused.
		decision.Secrets, decision.ForwardBody = scanner.ScanAndRedact(body)
		decision.StrongPath, decision.WeakPath = scanner.ScanPathsContext(decision.ForwardBody)
	case action != "off" && pathsOn:
		// Both channels over the SAME body: one shared automaton pass — the
		// paths gate verdict rides the secrets scan's phase 1 instead of
		// paying ~10 per-literal bytes.Contains sweeps afterwards.
		decision.Secrets, decision.StrongPath, decision.WeakPath = scanner.ScanSecretsAndPaths(body)
	case action != "off":
		if action == "redact" {
			decision.Secrets, decision.ForwardBody = scanner.ScanAndRedact(body)
		} else {
			decision.Secrets = scanner.Scan(body)
		}
	case pathsOn:
		decision.StrongPath, decision.WeakPath = scanner.ScanPathsContext(decision.ForwardBody)
	}
	return decision
}

func (d RequestGuardDecision) Blocks(cfg GuardConfig) (kind string, names []string) {
	if len(d.Secrets) > 0 && cfg.SecretsAction() == "block" {
		return "secret", d.Secrets
	}
	if len(d.StrongPath) > 0 && cfg.PathsAction() == "block" {
		return "sensitive_path", d.StrongPath
	}
	return "", nil
}

// GuardAuditHit is the audit projection of one guard hit. It carries
// identity and classification labels only — matched content never enters any
// field (credential red line); Reason/Evidence arrive pre-scrubbed from the
// adjudication channel.
type GuardAuditHit struct {
	Kind      string
	Names     []string
	Action    string
	RequestID string
	SessionID string
	Agent     string
	Proto     string
	Exposed   string
	Verdict   string
	Reason    string
	Evidence  string
	Model     string
	// Detail is free-form operator context (drift records, exact-match
	// attribution) — labels and masked displays only, never matched content.
	Detail string
}

// AuditGuardHit enqueues one security audit record for a guard hit on the
// request's snapshot logger (nil when the request's generation has audit
// off). The record carries pattern/path NAMES and the action only — matched
// content never enters any field (credential red line). verdict is empty for
// the classic immediate record, or the adjudication verdict ("high"/
// "medium"/"error"/"skipped") when the record comes from (or fail-opens out
// of) the AI second-opinion channel; reason/evidence are the scrubbed,
// length-capped model judgment logic and factual basis.
func AuditGuardHit(logger *seclog.Logger, h GuardAuditHit) {
	if logger == nil {
		return
	}
	logger.Enqueue(&seclog.Record{
		Ts:        time.Now().UnixMilli(),
		Kind:      h.Kind,
		RequestID: h.RequestID,
		SessionID: h.SessionID,
		Agent:     h.Agent,
		Protocol:  h.Proto,
		Exposed:   h.Exposed,
		Names:     h.Names,
		Action:    h.Action,
		Verdict:   h.Verdict,
		Reason:    h.Reason,
		Evidence:  h.Evidence,
		Model:     h.Model,
		Detail:    h.Detail,
	})
}

// exactSecretNames selects the known-secret exact channels from a secret-hit
// name list (the zero-false-positive family the pipeline intercepts
// synchronously — raw and encoded configured credentials; the cross-request
// fragmented completion is reported under its own name).
func exactSecretNames(names []string) []string {
	var known []string
	for _, n := range names {
		if n == "known_secret" || n == "known_secret_encoded" {
			known = append(known, n)
		}
	}
	return known
}

// patternSecretNames drops the exact channels: what remains is the
// false-positive-prone rule-table/custom channel eligible for AI
// adjudication.
func patternSecretNames(names []string) []string {
	var pattern []string
	for _, n := range names {
		if n != "known_secret" && n != "known_secret_encoded" {
			pattern = append(pattern, n)
		}
	}
	return pattern
}

// buildAdjudications locates the spans of the given hit names in body and
// builds one job per DISTINCT matched span (deduped by bytes), capped at
// maxAdjudicationsPerRequest. Spans past the cap — and names none of whose
// spans produced a job (every span deduped onto another rule's identical
// bytes) — come back as leftover names so the caller fails open: nothing
// beyond the cap may vanish silently. strongOnly filters sensitive-path hits
// to strong occurrences (weak hits never reach this helper and never count as
// leftover). Context windows come from the scanner's MaskSnippet with
// maskHit=false: the judged hit stays raw while OTHER secret hits inside the
// window are masked.
func buildAdjudications(sc *guard.Scanner, body []byte, names []string, kind string, strongOnly bool, ctxBytes int, meta GuardAdjudication) (jobs []GuardAdjudication, leftover []string) {
	if len(names) == 0 {
		return nil, nil
	}
	if ctxBytes <= 0 {
		return nil, names
	}
	located := sc.Locate(body, names)
	seenSpan := map[string]bool{}
	seenLeftover := map[string]bool{}
	covered := map[string]bool{}     // names with at least one job
	adjudicable := map[string]bool{} // names with at least one strong/pattern span
	addLeftover := func(name string) {
		if !seenLeftover[name] {
			seenLeftover[name] = true
			leftover = append(leftover, name)
		}
	}
	for _, m := range located {
		if strongOnly && m.Strength != guard.StrengthStrong {
			continue
		}
		adjudicable[m.Name] = true
		if len(jobs) >= maxAdjudicationsPerRequest {
			// A capped name may already have jobs for earlier spans; it still
			// fails open because the overflow spans get no verdict of their
			// own (audit records are name-granular).
			addLeftover(m.Name)
			continue
		}
		hit := string(body[m.Start:m.End])
		if seenSpan[hit] {
			continue
		}
		seenSpan[hit] = true
		pre, hitCtx, post := sc.MaskSnippet(body, m.Start, m.End, false, ctxBytes)
		job := meta
		job.Kind = kind
		job.Rule = m.Name
		job.Hit = hitCtx
		job.Pre, job.Post = pre, post
		jobs = append(jobs, job)
		covered[m.Name] = true
	}
	// Names with adjudicable spans that produced no job at all (every span
	// deduped onto another rule's identical bytes) keep the classic record
	// too. Names without any adjudicable span (e.g. weak-only path hits, if
	// a caller passes them) are silently ignored — that is the weak-hit
	// contract, not a fail-open.
	for _, n := range names {
		if adjudicable[n] && !covered[n] {
			addLeftover(n)
		}
	}
	return jobs, leftover
}

// enqueueAdjudications offers built jobs to the adjudicator and returns the
// rule names that could NOT be enqueued (queue full / service closed) — the
// caller fails open and gives them the classic immediate record. Jobs whose
// content is already in flight or cached are consumed silently.
func enqueueAdjudications(adj Adjudicator, jobs []GuardAdjudication) []string {
	if len(jobs) == 0 {
		return nil
	}
	var failOpen []string
	for _, j := range jobs {
		if !adj.Enqueue(j) {
			failOpen = append(failOpen, j.Rule)
		}
	}
	return failOpen
}
