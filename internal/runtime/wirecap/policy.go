package wirecap

import (
	"regexp"
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

// ClassifyProviderStatus maps one PROVIDER-level probe outcome to a
// capability conclusion. Differs from ClassifyStatus in exactly one case: a
// 400 whose body carries a model/tool rejection wording is No, not yes.
// Provider-level legs are probed with a function tool attached (agent-grade,
// same as model legs), and a 400 like "Function tools ... are not supported
// for gpt-5.6-luna in /v1/chat/completions" means the endpoint exists but
// the tools-attached leg is not callable — claiming yes would replay the
// bare-ping false positive the tool attachment exists to catch. A provider
// no expires via negativeTTL, so a wrong phrase match self-heals on the next
// probe pass (unlike model-level nos, which are fingerprint-gated).
func ClassifyProviderStatus(status int, err error, body []byte) Verdict {
	if err != nil {
		return Unknown
	}
	if status == 400 {
		lower := strings.ToLower(string(body))
		for _, phrase := range modelRejectionPhrases {
			if strings.Contains(lower, phrase) {
				return No
			}
		}
		for _, re := range modelRejectionREs {
			if re.MatchString(lower) {
				return No
			}
		}
	}
	return ClassifyStatus(status, err)
}

// modelRejectionPhrases are the 400-error wordings that mean "this model is
// not served here" rather than a request-shape dispute. Matched
// case-insensitively against the (possibly gateway-wrapped) error body. Bare
// "model" is deliberately NOT enough — shape disputes mention the model too
// ("... with this model").
// modelRejectionREs carry wordings too specific for plain substring
// matching. The bare "not supported for" substring was retired in v4: it
// also matched plan-tier rejections ("Streaming is not supported for this
// plan") and a model-level no has no TTL — one misread locked the leg until
// the config fingerprint changed.
var modelRejectionREs = []*regexp.Regexp{
	// aqp gateway: "<feature> ... are not supported for <model> in <path>" —
	// the model is rejected ON THIS LEG ("Function tools with
	// reasoning_effort are not supported for gpt-5.6-luna in
	// /v1/chat/completions").
	regexp.MustCompile(`(?i)not supported for [a-z0-9][a-z0-9._*-]* in /`),
}

var modelRejectionPhrases = []string{
	"model not found",
	"does not exist",
	"unsupported model",
	"invalid model",
	"model is not supported",
	// shopee gateway: {"retcode":40403,"message":"Model not supported by
	// this endpoint"} — the endpoint exists but refuses THIS model; without
	// these entries the probe misreads the 400 as a shape dispute (yes).
	"model not supported",
	"not supported by this endpoint",
	"unknown model",
	"no such model",
	"not supported with this model",
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
		for _, re := range modelRejectionREs {
			if re.MatchString(lower) {
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
//	anthropic client + model.anthropic == no                     → convert to chat
//	  (a probed no overrides the base declaration — the leg is dead for THIS
//	  model; same rule as routing.NativeProtocolsWithVerdict, so takeover and
//	  forward never disagree)
//	anthropic client + model fully concluded, responses != yes   → convert to chat
//	responses client + model.responses == yes                    → passthrough
//	responses client + model.responses == no                     → first probed-yes
//	  alternative (chat, then anthropic); an unconcluded alternative beats the
//	  known-dead legs (unknown ≠ dead); all-no stays chat so the upstream
//	  error surfaces cleanly
//	chat client + model.chat == no                               → first probed-yes
//	  alternative (responses, then anthropic), unknown alternatives next;
//	  all-no stays passthrough so the upstream error surfaces cleanly
//	chat client otherwise                                         → passthrough
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
			case modelCaps.Anthropic == No:
				// A probed model-level no overrides the endpoint declaration
				// (routing.NativeProtocolsWithVerdict's settle treats it the
				// same way) — the anthropic leg is dead for this model, so
				// never fall back to provider-level anthropic passthrough.
				// The openai base takes over, preferring responses when the
				// provider-level verdict backs it and the model-level
				// responses leg did not conclude no (a model-level no beats
				// a provider-level yes — rewindable via the 404 correction);
				// otherwise chat, worth one attempt even when unconcluded
				// (unknown ≠ dead).
				if cok && capabilities.Responses == Yes && modelCaps.Responses != No {
					return "responses", true
				}
				return "openai", false
			case modelCaps.Concluded():
				// No anthropic base and responses concluded not-yes → chat is
				// the definitional openai-base fallback.
				return "openai", false
			}
		case "responses":
			switch modelCaps.Responses {
			case Yes:
				return "responses", false
			case No:
				// The responses leg is dead — fall to the first probed-yes
				// alternative (shopee's claude models are anthropic-only).
				// An UNCONCLUDED alternative beats the known-dead legs too
				// (unknown ≠ dead, same philosophy as the anthropic branch's
				// chat fallback); only when every leg concluded no does the
				// choice stay chat, letting the upstream error surface.
				if modelCaps.Chat == Yes {
					return "openai", false
				}
				if modelCaps.Anthropic == Yes {
					return "anthropic", false
				}
				if modelCaps.Chat == Unknown {
					return "openai", false
				}
				if modelCaps.Anthropic == Unknown {
					return "anthropic", false
				}
				return "openai", false
			}
		default:
			// Chat client: passthrough is the default, but a probed model-level
			// chat no is authoritative — the same rule the anthropic branch
			// applies (routing.NativeProtocolsWithVerdict excludes the leg).
			// Convert to the first probed-yes alternative instead of riding a
			// dead leg; unconcluded or all-no stays passthrough (status quo
			// while probing / the upstream error surfaces and is now cleanly
			// translatable).
			if modelCaps.Chat == No {
				if modelCaps.Responses == Yes {
					return "responses", true
				}
				if modelCaps.Anthropic == Yes {
					return "anthropic", false
				}
				// Unknown alternatives beat the known-dead chat leg.
				if modelCaps.Responses == Unknown {
					return "responses", true
				}
				if modelCaps.Anthropic == Unknown {
					return "anthropic", false
				}
			}
			return clientProtocol, false
		}
	}
	return Resolve(clientProtocol, hasAnthropicBase, capabilities, cok)
}
