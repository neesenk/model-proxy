// capability_matrix_test.go — the conformance gate for the declared
// capability matrix: every registered pair has an entry, no same-protocol
// entry exists, and every declared Refused feature is actually rejected by
// the live scanner for that exact pair (declared == enforced; the table may
// never drift from convert_capabilities.go).
package protocol

import (
	"os"
	"regexp"
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

// TestCapabilityMatrixRefusalsBidirectional closes the loop the
// declared→enforced walk above cannot: for every conversion pair and every
// feature the scanner CAN refuse (the refusedFixtures vocabulary), the
// scanner's actual verdict must match the matrix exactly. A refusal the
// scanner enforces but the matrix does not declare is a silent refusal
// surface (doctor under-reports); a declared refusal the scanner does not
// enforce is a phantom capability. Direction-conditional refusals
// (cache_retention/custom_tool toward anthropic only) fall out of the same
// loop: the fixture converts toward the other backend, so the matrix must
// not declare it there.
func TestCapabilityMatrixRefusalsBidirectional(t *testing.T) {
	for _, client := range []wire.Protocol{protocolAnthropic, protocolOpenAI, protocolResponses} {
		for _, backend := range []wire.Protocol{protocolAnthropic, protocolOpenAI, protocolResponses} {
			if client == backend {
				continue
			}
			pair := conversionPair{client: client, backend: backend}
			declared := map[string]bool{}
			for _, f := range conversionCapabilityMatrix[pair].Refused {
				declared[f] = true
			}
			for key, fixture := range refusedFixtures {
				protoFeature := strings.SplitN(key, "|", 2)
				if wire.Protocol(protoFeature[0]) != client {
					continue
				}
				feature := protoFeature[1]
				err := validateConversionCapabilities([]byte(fixture), string(client), string(backend))
				unsupported, refused := asUnsupportedConversion(err)
				if refused && unsupported.Feature != feature {
					t.Errorf("%s→%s: fixture for %s triggered refusal of a different feature %q — fixture drift", client, backend, feature, unsupported.Feature)
					continue
				}
				if refused && !declared[feature] {
					t.Errorf("scanner refuses %s on %s→%s but the matrix does not declare it", feature, client, backend)
				}
				if !refused && declared[feature] {
					t.Errorf("matrix declares %s refused on %s→%s but the scanner lets the fixture through", feature, client, backend)
				}
			}
		}
	}
}

// TestRefusedFixtureVocabularyIsClosed pins the scanner's closed feature
// vocabulary to the fixture table: every unsupportedFeature literal emitted in
// convert_capabilities.go must have a fixture, and no fixture may name a
// feature the scanner no longer emits. This is the seam that keeps the
// bidirectional test self-maintaining — a new scanner refusal without a
// fixture (and matrix declaration) turns this red first. Source-scanning is
// warranted here because the emission vocabulary cannot be enumerated through
// behavior alone (an unknown feature has no fixture to probe with); the
// pattern pins the exact unsupportedFeature call sites.
func TestRefusedFixtureVocabularyIsClosed(t *testing.T) {
	src, err := os.ReadFile("convert_capabilities.go")
	if err != nil {
		t.Fatalf("read scanner source: %v", err)
	}
	emitted := map[string]bool{}
	for _, m := range regexp.MustCompile(`unsupportedFeature\(\s*"[a-z]+",\s*targetProto,\s*"([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		emitted[m[1]] = true
	}
	if len(emitted) == 0 {
		t.Fatal("feature extraction matched nothing — the call-shape anchor in convert_capabilities.go changed; update the pattern")
	}
	have := map[string]bool{}
	for key := range refusedFixtures {
		have[strings.SplitN(key, "|", 2)[1]] = true
	}
	for feature := range emitted {
		if !have[feature] {
			t.Errorf("scanner emits refusal feature %q with no refusedFixtures entry — add one so the bidirectional test covers it", feature)
		}
	}
	for feature := range have {
		if !emitted[feature] {
			t.Errorf("refusedFixtures carries feature %q the scanner never emits — stale fixture", feature)
		}
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
