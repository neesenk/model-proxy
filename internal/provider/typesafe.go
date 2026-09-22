package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// TypeSafeProvider implements the TypeSafe System One decisions API
// (api.typesafe.ai): a decision model (Jev) that takes {model, state,
// questions} and returns typed answers instead of generated text. The wire
// protocol is "decisions" (client path /v1/decisions → upstream /systemone on
// a versioned base); there is no streaming, no tools, and no chat-protocol
// conversion (fail-closed stubs in internal/protocol).
//
// The provider is a pure decisions backend: its base URL comes from
// decisions_base_url (falling back to openai_base_url for gateways like
// OpenRouter that serve /systemone on the chat base). A single API key
// authenticates everything via Authorization: Bearer.
type TypeSafeProvider struct {
	*ApiKeyBase
	baseProbe
	cfg *Config
}

func init() {
	Register("typesafe", func(cfg *Config, providerName string) (Provider, error) {
		return &TypeSafeProvider{
			ApiKeyBase: newApiKeyBaseBound(cfg, providerName),
			cfg:        cfg,
		}, nil
	})
}

// decisionsURL builds an endpoint URL on the decisions base (with the
// openai-base fallback, mirroring targetexec.NewPlan's selection).
func (p *TypeSafeProvider) decisionsURL(path string) string {
	base := p.cfg.DecisionsBaseURL
	if base == "" {
		base = p.cfg.OpenAIBaseURL
	}
	return strings.TrimRight(base, "/") + path
}

// RewriteRequest is a no-op: the plan layer already maps the decisions client
// path (/v1/decisions) to the upstream path (/systemone) on the decisions base.
func (p *TypeSafeProvider) RewriteRequest(targetURL string, body []byte, path string) (string, []byte) {
	return targetURL, body
}

func (p *TypeSafeProvider) Logout() error { return p.DeleteKey() }

func (p *TypeSafeProvider) FetchModels() ([]string, error) {
	return p.FetchModelsContext(context.Background())
}

func (p *TypeSafeProvider) FetchModelsContext(ctx context.Context) ([]string, error) {
	infos, err := fetchModelInfosBearerURL(ctx, p.decisionsURL("/models"), p.AuthHeaders)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(infos))
	for _, m := range infos {
		ids = append(ids, m.ID)
	}
	return ids, nil
}

// FilterModelIDs works around an upstream quirk (verified 2026-09): TypeSafe's
// /v1/models advertises the short family id (jev-1.13) which /systemone then
// REJECTS as unknown — the callable versioned id appends ".0" (jev-1.13.0).
// Rewrite short jev-X.Y ids to their callable form (the short form is
// reported as dropped so the rewrite is visible); everything else passes.
func (p *TypeSafeProvider) FilterModelIDs(ids []string) (kept, dropped []string) {
	for _, id := range ids {
		if isShortJevFamilyID(id) {
			kept = append(kept, id+".0")
			dropped = append(dropped, id+" (uncallable as-is; probed as "+id+".0)")
			continue
		}
		kept = append(kept, id)
	}
	return kept, dropped
}

// isShortJevFamilyID reports whether id is exactly "jev-<digits>.<digits>"
// (not jev-latest/jev-preview, not already versioned jev-X.Y.Z).
func isShortJevFamilyID(id string) bool {
	rest, ok := strings.CutPrefix(id, "jev-")
	if !ok {
		return false
	}
	major, minor, ok := strings.Cut(rest, ".")
	if !ok || major == "" || minor == "" {
		return false
	}
	for _, r := range major {
		if r < '0' || r > '9' {
			return false
		}
	}
	for _, r := range minor {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ProbeRequest probes callability with a minimal System One decision (a Noul
// question is the cheapest typed answer the API produces).
func (p *TypeSafeProvider) ProbeRequest(modelID string) ProbeRequest {
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"state": "ping",
		"questions": map[string]any{
			"reachable": map[string]any{
				"type":         "noul",
				"instructions": "The state is a connectivity check.",
			},
		},
	})
	return ProbeRequest{Method: http.MethodPost, Path: "/systemone", Body: body}
}

// Quota reports BillingUnknown: TypeSafe exposes no public billing/quota API
// (balance is console-only at console.typesafe.ai).
func (p *TypeSafeProvider) Quota() (*QuotaSnapshot, error) {
	return &QuotaSnapshot{
		Billing: BillingUnknown,
		Notes: []string{
			"Input-token billing ($0.042/M, output free); balance is console-only",
			"Console: https://console.typesafe.ai",
		},
		AsOf: time.Now(),
	}, nil
}
