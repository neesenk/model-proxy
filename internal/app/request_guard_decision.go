package app

import "model-proxy/internal/guard"

// requestGuardDecision is the pure, per-request portion of the outbound guard
// contract shared by live forwarding and /debug/route. Observation side
// effects (counters, events, audit, and session-fragment state) stay in the
// live path; both callers nevertheless make the same secret/path decision and
// derive the same body that may participate in the response-cache key.
type requestGuardDecision struct {
	forwardBody []byte
	secrets     []string
	strongPath  []string
	weakPath    []string
}

func evaluateRequestGuard(cfg GuardConfig, scanner *guard.Scanner, body []byte) requestGuardDecision {
	decision := requestGuardDecision{forwardBody: body}
	if scanner == nil {
		return decision
	}
	if action := cfg.SecretsAction(); action != "off" {
		decision.secrets = scanner.Scan(body)
		if len(decision.secrets) > 0 && action == "redact" {
			decision.forwardBody = scanner.Redact(body)
		}
	}
	if cfg.PathsAction() != "off" {
		decision.strongPath, decision.weakPath = scanner.ScanPathsContext(decision.forwardBody)
	}
	return decision
}

func (d requestGuardDecision) blocks(cfg GuardConfig) (kind string, names []string) {
	if len(d.secrets) > 0 && cfg.SecretsAction() == "block" {
		return "secret", d.secrets
	}
	if len(d.strongPath) > 0 && cfg.PathsAction() == "block" {
		return "sensitive_path", d.strongPath
	}
	return "", nil
}
