package takeover_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/providerbuild"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/takeover"
)

// probecaps_test.go covers takeover's offline consumption of the daemon's
// per-model protocol probe matrix (model_caps.json): verdicts flip protocol
// selection where endpoint declarations overstate support, stale fingerprints
// are ignored, and a missing file degrades to static behavior.

// writeModelCaps seeds <home>/.model-proxy/model_caps.json with one provider
// entry; fingerprint "" means "compute the correct one from prov".
func writeModelCaps(t *testing.T, home, provName string, prov configdomain.Provider, fingerprint string, models map[string]runtimewire.ModelProtocols) {
	t.Helper()
	if fingerprint == "" {
		fingerprint = providerbuild.ProtocolConfigFingerprint(prov)
	}
	doc := map[string]any{
		"version": 1,
		"providers": map[string]any{
			provName: map[string]any{
				"fingerprint": fingerprint,
				"probed_at":   time.Now().UTC(),
				"models":      models,
			},
		},
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model_caps.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProbeCaps_FlipsSelectionWhereDeclarationOverstates(t *testing.T) {
	// Provider declares BOTH endpoints, but the probe found the anthropic
	// leg rejects this model. Statically anthropic+openai would tie and the
	// default (anthropic) variant would win; the verdict must flip the pi
	// family to the openai variant — matching what the runtime actually does.
	home := t.TempDir()
	t.Setenv("HOME", home)
	prov := configdomain.Provider{
		OpenAIBaseURL:    "https://gw.invalid/v1",
		AnthropicBaseURL: "https://gw.invalid",
		Models:           []string{"m-x"},
	}
	writeModelCaps(t, home, "gw", prov, "", map[string]runtimewire.ModelProtocols{
		"m-x": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No},
	})
	cfg := cfgWith(map[string]configdomain.Provider{"gw": prov}, nil)

	clients, err := takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi-openai" {
		t.Fatalf("probe-aware resolution = %v, want [pi-openai] (anthropic leg probed no)", got)
	}
}

func TestProbeCaps_StaleFingerprintIgnored(t *testing.T) {
	// Same scene, but the recorded fingerprint does not match the current
	// provider config (base URL changed since the probe): the stale verdict
	// must be dropped and selection falls back to the static tie → default.
	home := t.TempDir()
	t.Setenv("HOME", home)
	prov := configdomain.Provider{
		OpenAIBaseURL:    "https://gw.invalid/v1",
		AnthropicBaseURL: "https://gw.invalid",
		Models:           []string{"m-x"},
	}
	writeModelCaps(t, home, "gw", prov, "stale-fingerprint", map[string]runtimewire.ModelProtocols{
		"m-x": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No},
	})
	cfg := cfgWith(map[string]configdomain.Provider{"gw": prov}, nil)

	clients, err := takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi" {
		t.Fatalf("stale verdict must be ignored = %v, want default [pi]", got)
	}
}

func TestProbeCaps_ProbeYesClaimsResponses(t *testing.T) {
	// Only openai_base_url is declared (static: openai native only), but the
	// probe found /responses works for this model — split can then file the
	// model under the responses variant.
	home := t.TempDir()
	t.Setenv("HOME", home)
	prov := configdomain.Provider{OpenAIBaseURL: "https://gw.invalid/v1", Models: []string{"m-r"}}
	writeModelCaps(t, home, "gw", prov, "", map[string]runtimewire.ModelProtocols{
		"m-r": {Chat: runtimewire.No, Anthropic: runtimewire.No, Responses: runtimewire.Yes},
	})
	cfg := cfgWith(map[string]configdomain.Provider{"gw": prov}, nil)

	clients, err := takeover.ResolveClientsMode(cfg, "pi", t.TempDir(), takeover.ModeSplit)
	if err != nil {
		t.Fatalf("ResolveClientsMode(pi, split): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi-responses" {
		t.Fatalf("split with responses-only probe = %v, want [pi-responses]", got)
	}
}

func TestProbeCaps_MissingFileDegradesToStatic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	prov := configdomain.Provider{
		OpenAIBaseURL:    "https://gw.invalid/v1",
		AnthropicBaseURL: "https://gw.invalid",
		Models:           []string{"m-x"},
	}
	cfg := cfgWith(map[string]configdomain.Provider{"gw": prov}, nil)

	clients, err := takeover.ResolveClients(cfg, "pi", t.TempDir())
	if err != nil {
		t.Fatalf("ResolveClients(pi): %v", err)
	}
	if got := namesOf(clients); len(got) != 1 || got[0] != "pi" {
		t.Fatalf("no probe file: static tie must give default [pi], got %v", got)
	}
}
