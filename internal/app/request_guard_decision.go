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
	action := cfg.SecretsAction()
	pathsOn := cfg.PathsAction() != "off"
	switch {
	case action != "off" && pathsOn:
		// Both channels over the SAME body: one shared automaton pass — the
		// paths gate verdict rides the secrets scan's phase 1 instead of
		// paying ~10 per-literal bytes.Contains sweeps afterwards.
		decision.secrets, decision.strongPath, decision.weakPath = scanner.ScanSecretsAndPaths(body)
	case action != "off":
		decision.secrets = scanner.Scan(body)
	case pathsOn:
		decision.strongPath, decision.weakPath = scanner.ScanPathsContext(decision.forwardBody)
	}
	if len(decision.secrets) > 0 && action == "redact" {
		decision.forwardBody = scanner.Redact(body)
		if pathsOn {
			// Paths classify the POST-REDACT body (unchanged contract):
			// redaction rewrote bytes, so the pre-redact gate/classification
			// cannot be reused.
			decision.strongPath, decision.weakPath = scanner.ScanPathsContext(decision.forwardBody)
		}
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
