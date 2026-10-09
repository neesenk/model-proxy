// capability_matrix_test.go — the conformance gate for the declared
// capability matrix: every registered pair has an entry, no same-protocol
// entry exists, and every declared Refused feature is actually rejected by
// the live scanner for that exact pair (declared == enforced; the table may
// never drift from convert_capabilities.go).
package protocol

import (
	"strings"
	"testing"

	"model-proxy/internal/protocol/wire"
)

// refusedFixtures carries one minimal request body per (client protocol,
// feature) that triggers the scanner's rejection for the pair under test.
// Feature names match unsupportedConversionError.Feature exactly.
var refusedFixtures = map[string]string{
	// openai (chat) client fixtures
	"openai|cache_retention": `{"prompt_cache_retention":"1h","messages":[]}`,
	"openai|multi_choice":    `{"n":2,"messages":[]}`,
	"openai|logprobs":        `{"logprobs":true,"messages":[]}`,
	"openai|audio":           `{"audio":{"voice":"alloy"},"messages":[]}`,
	"openai|hosted_tool":     `{"tools":[{"type":"code_interpreter"}],"messages":[]}`,
	// responses client fixtures
	"responses|cache_retention": `{"prompt_cache_retention":"1h","input":[]}`,
	"responses|logprobs":        `{"top_logprobs":4,"input":[]}`,
	"responses|custom_tool":     `{"tools":[{"type":"custom","name":"freeform"}],"input":[]}`,
	"responses|hosted_tool":     `{"tools":[{"type":"code_interpreter"}],"input":[]}`,
	"responses|input_item":      `{"input":[{"type":"item_reference","id":"x"}]}`,
	"responses|audio":           `{"input":[{"type":"message","content":[{"type":"input_audio","input_audio":"x"}]}]}`,
	// anthropic client fixtures
	"anthropic|mcp":           `{"mcp_servers":[{"type":"url","url":"https://x","name":"x"}],"messages":[]}`,
	"anthropic|hosted_tool":   `{"tools":[{"type":"computer_20250124","name":"computer"}],"messages":[]}`,
	"anthropic|content_block": `{"messages":[{"role":"user","content":[{"type":"video","source":{}}]}]}`,
}

// TestCapabilityMatrixCoversRegistry: exactly the registry's pairs are
// declared — no missing pair, no same-protocol or unknown pair.
func TestCapabilityMatrixCoversRegistry(t *testing.T) {
	for pair := range protocolConversions {
		if _, ok := conversionCapabilityMatrix[pair]; !ok {
			t.Errorf("registered pair %s→%s missing from capability matrix", pair.client, pair.backend)
		}
	}
	for pair := range conversionCapabilityMatrix {
		if _, ok := protocolConversions[pair]; !ok {
			t.Errorf("capability matrix entry %s→%s is not a registered conversion", pair.client, pair.backend)
		}
		if pair.client == pair.backend {
			t.Errorf("capability matrix must not contain same-protocol pair %s", pair.client)
		}
	}
	if len(conversionCapabilityMatrix) != len(protocolConversions) {
		t.Fatalf("matrix size %d != registry size %d", len(conversionCapabilityMatrix), len(protocolConversions))
	}
}

// TestCapabilityMatrixRefusalsEnforced: every declared Refused feature is
// rejected by validateConversionCapabilities for that exact pair, with the
// declared feature name. The decisions stubs refuse any body at all.
func TestCapabilityMatrixRefusalsEnforced(t *testing.T) {
	for pair, entry := range conversionCapabilityMatrix {
		for _, feature := range entry.Refused {
			if feature == "*" {
				// Whole-protocol refusal lives in the registry's request
				// entry (the fail-closed stub), not the scanner: the scanner
				// only knows the three chat-shaped client protocols.
				conversion, ok := protocolConversions[pair]
				if !ok || conversion.request == nil {
					t.Errorf("pair %s→%s has no request entry to refuse with", pair.client, pair.backend)
					continue
				}
				if _, err := conversion.request([]byte(`{"x":1}`), convertReqOpts{}); err == nil {
					t.Errorf("pair %s→%s declared whole-protocol refusal but a plain body converted", pair.client, pair.backend)
				}
				continue
			}
			fixture, ok := refusedFixtures[string(pair.client)+"|"+feature]
			if !ok {
				t.Errorf("no refusal fixture for %s→%s feature %s — add one to refusedFixtures", pair.client, pair.backend, feature)
				continue
			}
			err := validateConversionCapabilities([]byte(fixture), string(pair.client), string(pair.backend))
			if err == nil {
				t.Errorf("pair %s→%s declares refused feature %s but the scanner let the fixture through", pair.client, pair.backend, feature)
				continue
			}
			unsupported, ok := asUnsupportedConversion(err)
			if !ok {
				t.Errorf("pair %s→%s feature %s: scanner error is not an unsupportedConversionError: %v", pair.client, pair.backend, feature, err)
				continue
			}
			if unsupported.Feature != feature {
				t.Errorf("pair %s→%s: scanner feature = %q, matrix declares %q", pair.client, pair.backend, unsupported.Feature, feature)
			}
		}
	}
}

// TestCapabilityMatrixDirectionalConditionals pins the refusals that are
// conditional on the TARGET protocol: cache_retention is refused only toward
// anthropic (its 1h breakpoint TTL ceiling) — the same body must convert
// toward the other backend.
func TestCapabilityMatrixDirectionalConditionals(t *testing.T) {
	retention := `{"prompt_cache_retention":"1h","messages":[]}`
	if err := validateConversionCapabilities([]byte(retention), "openai", "responses"); err != nil {
		t.Errorf("openai→responses refused prompt_cache_retention: %v (matrix declares it anthropic-only)", err)
	}
	if err := validateConversionCapabilities([]byte(retention), "openai", "anthropic"); err == nil {
		t.Error("openai→anthropic must refuse prompt_cache_retention")
	}

	// custom_tool is refused only toward anthropic (no raw-input tool
	// contract there); toward openai it converts.
	custom := `{"tools":[{"type":"custom","name":"freeform"}],"input":[]}`
	if err := validateConversionCapabilities([]byte(custom), "responses", "openai"); err != nil {
		t.Errorf("responses→openai refused custom tool: %v (matrix declares it anthropic-only)", err)
	}
	if err := validateConversionCapabilities([]byte(custom), "responses", "anthropic"); err == nil {
		t.Error("responses→anthropic must refuse custom/freeform tools")
	}
}

// TestConversionCapabilitiesAccessor: the exported view answers per pair and
// reports same-protocol directions as absent.
func TestConversionCapabilitiesAccessor(t *testing.T) {
	entry, ok := ConversionCapabilities(wire.Protocol("openai"), wire.Protocol("anthropic"))
	if !ok || len(entry.Refused) == 0 {
		t.Fatalf("openai→anthropic accessor = %+v ok=%v, want declared refusals", entry, ok)
	}
	if !strings.Contains(strings.Join(entry.Lossy, " "), "thinking") {
		t.Errorf("openai→anthropic lossy = %v, want the thinking-replay degradation named", entry.Lossy)
	}
	if _, ok := ConversionCapabilities(wire.Protocol("openai"), wire.Protocol("openai")); ok {
		t.Error("same-protocol direction must report absent")
	}
}
