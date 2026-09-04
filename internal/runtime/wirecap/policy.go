package wirecap

import (
	"strings"
	"time"
)

// LegFresh reports whether a concluded verdict is still trusted. Positive
// conclusions remain valid until corrected at runtime; negative conclusions
// expire so a transient 404 cannot downgrade an endpoint forever.
func LegFresh(
	verdict Verdict,
	probedAt time.Time,
	now time.Time,
	negativeTTL time.Duration,
) bool {
	switch verdict {
	case Yes:
		return true
	case No:
		return now.Sub(probedAt) < negativeTTL
	default:
		return false
	}
}

// ClassifyStatus maps one HTTP probe outcome to a capability conclusion.
func ClassifyStatus(status int, err error) Verdict {
	if err != nil {
		return Unknown
	}
	if status == 404 {
		return No
	}
	if status >= 200 && status < 500 {
		return Yes
	}
	return Unknown
}

// modelRejectionPhrases are the 400-error wordings that mean "this model is
// not served here" rather than a request-shape dispute. Matched
// case-insensitively against the (possibly gateway-wrapped) error body. Bare
// "model" is deliberately NOT enough — shape disputes mention the model too
// ("... with this model").
var modelRejectionPhrases = []string{
	"model not found",
	"does not exist",
	"unsupported model",
	"invalid model",
	"model is not supported",
	"unknown model",
	"no such model",
	"not supported with this model",
	// "<feature> is/are not supported for <model> in <path>" — the gateway
	// rejects the model ON THIS LEG (aqp: "Function tools with
	// reasoning_effort are not supported for gpt-5.6-luna in
	// /v1/chat/completions"), which is precisely what the tools-attached
	// probe leg must record as unsupported.
	"not supported for",
}

// ClassifyModelStatus maps one protocol-leg probe outcome to a model-level
// capability conclusion. Differences from the provider-level ClassifyStatus:
//   - probed=false (leg's base URL not configured) → No: unsupported by
//     definition, not by observation.
//   - 400 carrying a model-rejection wording → No; any other 400 → Yes (a
//     shape dispute still proves the route serves the model — deliberately
//     coarse, same philosophy as the provider level).
//   - 401/403/429 → Unknown: auth/quota answers prove the route exists but
//     say nothing about THIS model (unlike the provider level, where they
//     count as yes).
func ClassifyModelStatus(probed bool, status int, err error, body []byte) Verdict {
	if !probed {
		return No
	}
	if err != nil {
		return Unknown
	}
	switch status {
	case 404:
		return No
	case 401, 403, 429:
		return Unknown
	case 400:
		lower := strings.ToLower(string(body))
		for _, phrase := range modelRejectionPhrases {
			if strings.Contains(lower, phrase) {
				return No
			}
		}
		return Yes
	}
	if status >= 200 && status < 300 {
		return Yes
	}
	if status > 400 && status < 500 {
		return Yes
	}
	return Unknown
}

// Resolve applies the wire-verdict fallback matrix after explicit protocol
// configuration and provider protocol hints have declined to choose.
// Anthropic support is config-declared (anthropic_base_url), so the only
// probed verdicts are the openai-base chat/responses legs:
//
//	anthropic client + hasAnthropicBase        → passthrough (dedicated base)
//	anthropic client + verdict.responses == yes → convert to responses
//	anthropic client + verdict.responses == no  → convert to chat
//	anthropic client + otherwise unknown        → passthrough (status quo while probing)
//	responses client + verdict.responses == no  → convert to chat
//	responses client + otherwise                → passthrough
//	chat client                                 → passthrough
func Resolve(
	clientProtocol string,
	hasAnthropicBase bool,
	capabilities Capabilities,
	ok bool,
) (protocol string, viaResponsesVerdict bool) {
	switch clientProtocol {
	case "anthropic":
		if hasAnthropicBase {
			return "anthropic", false
		}
		if ok {
			switch capabilities.Responses {
			case Yes:
				return "responses", true
			case No:
				return "openai", false
			}
		}
		return clientProtocol, false
	case "responses":
		if ok && capabilities.Responses == No {
			return "openai", false
		}
		return clientProtocol, false
	default:
		return clientProtocol, false
	}
}

// ResolveModel applies the MODEL-level capability matrix before the
// provider-level Resolve. A model-level hit (mok) with concluded legs wins;
// unconcluded combinations fall through to the provider-level matrix. The
// returned viaResponsesVerdict marks a verdict-driven switch TO responses —
// the only case the runtime 404 correction rewinds.
//
//	anthropic client + hasAnthropicBase + model.anthropic == yes → passthrough
//	anthropic client + model.responses == yes                    → convert to responses
//	  (covers both "no anthropic base" and "model absent from the anthropic base")
//	anthropic client + model fully concluded, responses != yes   → convert to chat
//	responses client + model.responses == no                     → convert to chat
//	responses client + model.responses == yes                    → passthrough
//	chat client                                                  → passthrough
func ResolveModel(
	clientProtocol string,
	hasAnthropicBase bool,
	modelCaps ModelProtocols,
	mok bool,
	capabilities Capabilities,
	cok bool,
) (protocol string, viaResponsesVerdict bool) {
	if mok {
		switch clientProtocol {
		case "anthropic":
			switch {
			case hasAnthropicBase && modelCaps.Anthropic == Yes:
				return "anthropic", false
			case modelCaps.Responses == Yes:
				return "responses", true
			case modelCaps.Concluded():
				// Anthropic unavailable (no base or model-level no) and
				// responses concluded not-yes → chat is the definitional
				// openai-base fallback.
				return "openai", false
			}
		case "responses":
			switch modelCaps.Responses {
			case Yes:
				return "responses", false
			case No:
				return "openai", false
			}
		default:
			return clientProtocol, false
		}
	}
	return Resolve(clientProtocol, hasAnthropicBase, capabilities, cok)
}
