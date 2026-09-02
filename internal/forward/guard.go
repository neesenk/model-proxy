// guard.go — the pure per-request outbound guard decision shared by live
// forwarding and /debug/route, plus the security-audit enqueue helper.
package forward

import (
	"time"

	"model-proxy/internal/guard"
	"model-proxy/internal/observe/seclog"
)

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

// AuditGuardHit enqueues one security audit record for a guard hit on the
// request's snapshot logger (nil when the request's generation has audit off).
// The record carries pattern/path NAMES and the action only — matched content
// never enters any field (credential red line).
func AuditGuardHit(logger *seclog.Logger, kind string, names []string, action, requestID, agent, proto, exposed string) {
	if logger == nil {
		return
	}
	logger.Enqueue(&seclog.Record{
		Kind:      kind,
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     agent,
		Protocol:  proto,
		Exposed:   exposed,
		Names:     names,
		Action:    action,
	})
}
