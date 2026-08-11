package wirecap

import "time"

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

// Resolve applies the wire-verdict fallback matrix after explicit protocol
// configuration and provider protocol hints have declined to choose.
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
			switch {
			case capabilities.Anthropic == Yes:
				return "anthropic", false
			case capabilities.Responses == Yes:
				return "responses", true
			case capabilities.Responses == No:
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
