// capability_matrix.go — the declarative per-pair view of what protocol
// conversion REFUSES (capability scanner, fail-closed before conversion) and
// what converts with a documented degradation. Consumers: `model-proxy
// doctor` route dry-run (replaces a formerly hardcoded lossy string) and the
// conformance test that pins every declared Refused entry to the scanner's
// actual rejection. The authority for WHAT is refused is
// validateConversionCapabilities (convert_capabilities.go); this table is
// the declarative mirror that must never drift — the conformance test
// (capability_matrix_test.go) enforces declared == enforced per pair, and
// protocol-conversion.md「能力矩阵」points here.
package protocol

import (
	"strings"

	"model-proxy/internal/protocol/wire"
)

// PairCapabilities declares one conversion pair's request-side contract.
type PairCapabilities struct {
	// Refused lists capability-scanner feature names the pair rejects
	// fail-closed before any conversion runs (the client feature cannot be
	// represented without data or execution-semantics loss). The sentinel
	// "*" marks a whole-protocol refusal (the decisions stubs).
	Refused []string
	// Lossy lists documented degradations that convert WITH diagnostics or
	// warnings (stable diag codes; see protocol-conversion.md「有损字段」).
	// These are advisory, not refusals.
	Lossy []string
}

func caps(refused []string, lossy ...string) PairCapabilities {
	return PairCapabilities{Refused: refused, Lossy: lossy}
}

// conversionCapabilityMatrix mirrors the scanner's per-direction refusals.
// cache_retention is refused only toward anthropic (its 1h breakpoint TTL
// ceiling); the chat n>1/logprobs/audio/hosted-tool refusals apply to every
// non-native target of that client protocol.
var conversionCapabilityMatrix = map[conversionPair]PairCapabilities{
	{client: protocolOpenAI, backend: protocolAnthropic}: caps(
		[]string{"cache_retention", "multi_choice", "logprobs", "audio", "hosted_tool"},
		"thinking (reasoning replay not implemented — reasoner/thinking models lose it on this pair)",
	),
	{client: protocolAnthropic, backend: protocolOpenAI}: caps(
		[]string{"mcp", "hosted_tool", "content_block"},
		"thinking (reasoning replay not implemented — reasoner/thinking models lose it on this pair)",
	),
	{client: protocolOpenAI, backend: protocolResponses}: caps(
		[]string{"multi_choice", "logprobs", "audio", "hosted_tool"},
		"multimodal output (no generic image output item)",
	),
	{client: protocolResponses, backend: protocolAnthropic}: caps(
		[]string{"cache_retention", "logprobs", "custom_tool", "hosted_tool", "input_item", "audio"},
		"multimodal output (no generic image output item)",
	),
	{client: protocolResponses, backend: protocolOpenAI}: caps(
		[]string{"logprobs", "hosted_tool", "input_item", "audio"},
		"multimodal output (no generic image output item)",
	),
	{client: protocolAnthropic, backend: protocolResponses}: caps(
		[]string{"mcp", "hosted_tool", "content_block"},
		"multimodal output (no generic image output item)",
	),
	// Every decisions pair is a registered fail-closed stub: typed
	// state/questions cannot become chat messages (or back).
	{client: protocolDecisions, backend: protocolAnthropic}: caps([]string{"*"}),
	{client: protocolDecisions, backend: protocolOpenAI}:    caps([]string{"*"}),
	{client: protocolDecisions, backend: protocolResponses}: caps([]string{"*"}),
	{client: protocolAnthropic, backend: protocolDecisions}: caps([]string{"*"}),
	{client: protocolOpenAI, backend: protocolDecisions}:    caps([]string{"*"}),
	{client: protocolResponses, backend: protocolDecisions}: caps([]string{"*"}),
}

// ConversionCapabilities returns the declared capability view for one
// conversion direction. ok=false means no conversion exists (same protocol
// needs none; the pair is outside the registry).
func ConversionCapabilities(from, to wire.Protocol) (PairCapabilities, bool) {
	if from == to {
		return PairCapabilities{}, false
	}
	entry, ok := conversionCapabilityMatrix[conversionPair{client: from, backend: to}]
	return entry, ok
}

// inboundProtocols are the client protocols the proxy serves; ConversionDoctorLines
// reports the per-direction capability summary for one backend protocol.
var inboundProtocols = []wire.Protocol{protocolAnthropic, protocolOpenAI, protocolResponses}

// ConversionDoctorLines renders the doctor-facing capability summary for one
// backend protocol: one line per inbound client protocol that would convert,
// listing the scanner refusals (fail-closed) and documented degradations.
// Same-protocol clients need no conversion and are omitted.
func ConversionDoctorLines(backend string) []string {
	target := wire.Protocol(backend)
	var lines []string
	for _, from := range inboundProtocols {
		if from == target {
			continue
		}
		entry, ok := ConversionCapabilities(from, target)
		if !ok {
			continue
		}
		line := string(from) + "→" + backend
		switch {
		case len(entry.Refused) == 1 && entry.Refused[0] == "*":
			line += ": refused (no conversion exists)"
		case len(entry.Refused) > 0:
			line += ": refuses " + strings.Join(entry.Refused, ", ")
		default:
			line += ": converts"
		}
		if len(entry.Lossy) > 0 {
			line += "; lossy: " + strings.Join(entry.Lossy, "; ")
		}
		lines = append(lines, line)
	}
	return lines
}
