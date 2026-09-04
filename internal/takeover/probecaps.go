package takeover

import (
	"os"
	"path/filepath"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// probecaps.go — offline consumption of the daemon's per-model protocol
// probe matrix. The probe exists because a provider's several base URLs do
// NOT all serve every model (e.g. shopee's chat endpoint rejects its claude
// models while its anthropic endpoint serves them), so endpoint declarations
// alone overstate native support. takeover reads the persisted verdicts to
// keep its protocol selection aligned with what the runtime actually does.

// probeCaps loads ~/.model-proxy/model_caps.json and maps it to routing
// verdicts keyed by provider → model. A missing/unreadable file degrades to
// nil (static endpoint-declaration fallback). A provider whose
// protocol-relevant config changed since the probe (fingerprint mismatch —
// same invalidation the daemon uses) is dropped: stale verdicts must not
// steer protocol selection.
func probeCaps(cfg *configdomain.Config) map[string]map[string]routing.ModelProtocolVerdict {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	path := runtimewire.ModelCapsPath(filepath.Join(home, ".model-proxy", "quota_state.json"))
	loaded, err := runtimewire.LoadModelCapsFile(path)
	if err != nil || loaded == nil {
		return nil
	}
	out := map[string]map[string]routing.ModelProtocolVerdict{}
	for name, entry := range loaded {
		prov, ok := cfg.Providers[name]
		if !ok || providerbuild.ProtocolConfigFingerprint(prov) != entry.Fingerprint {
			continue
		}
		models := make(map[string]routing.ModelProtocolVerdict, len(entry.Models))
		for m, mp := range entry.Models {
			models[m] = routing.ModelProtocolVerdict{
				Chat:      triOf(mp.Chat),
				Anthropic: triOf(mp.Anthropic),
				Responses: triOf(mp.Responses),
			}
		}
		out[name] = models
	}
	return out
}

func triOf(v runtimewire.Verdict) routing.Tri {
	switch v {
	case runtimewire.Yes:
		return routing.TriYes
	case runtimewire.No:
		return routing.TriNo
	}
	return routing.TriUnknown
}

// verdictFor returns the probe verdict for one route target, or nil when no
// usable record exists (provider/model unprobed or stale).
func verdictFor(caps map[string]map[string]routing.ModelProtocolVerdict, t configdomain.RouteTarget) *routing.ModelProtocolVerdict {
	if models, ok := caps[t.Provider]; ok {
		if v, ok := models[t.Model]; ok {
			return &v
		}
	}
	return nil
}
