package wirecap

// model_legs.go — folding one model's probed legs into its protocol matrix
// and into the keep verdict a models refresh acts on. This is the rule's
// single owner: the daemon twin (internal/admin/models_refresh.go), the CLI
// (internal/cli/models/check.go) and the boot/reload probe pass
// (internal/app/modelcaps.go) all read it from here.

// ModelLegFold accumulates the probed legs of ONE model: their protocol
// matrix and the evidence for the models-refresh keep rule.
type ModelLegFold struct {
	matrix                      ModelProtocols
	anyYes, probedNo, anyProbed bool
}

// Add classifies one probed leg (ClassifyModelStatus), records its verdict
// in the matrix under its leg name and returns that verdict. leg is "chat",
// "anthropic" or "responses" (probe.LegChat / LegAnthropic / LegResponses);
// any other name is recorded nowhere.
func (f *ModelLegFold) Add(leg string, probed bool, status int, err error, body []byte) Verdict {
	v := ClassifyModelStatus(probed, status, err, body)
	switch leg {
	case "chat":
		f.matrix.Chat = v
	case "anthropic":
		f.matrix.Anthropic = v
	case "responses":
		f.matrix.Responses = v
	}
	if v == Yes {
		f.anyYes = true
	}
	if probed {
		f.anyProbed = true
		if v == No {
			f.probedNo = true
		}
	}
	return v
}

// Matrix is the protocol matrix of the legs added so far.
func (f *ModelLegFold) Matrix() ModelProtocols { return f.matrix }

// Keep reports whether a models refresh keeps the model — the "definitive
// negative" rule:
//
//   - any leg Yes ⇒ keep;
//   - no probed leg No — every probed leg inconclusive, a transient
//     429/5xx/auth sweep holds no negative information ⇒ keep: dropping it
//     would write one rate-limit storm back as a config deletion;
//   - otherwise (a probed leg said No, or nothing was probed) ⇒ drop.
//
// A leg whose base is not configured classifies No by definition and is not
// "probed": it never counts as the negative evidence.
func (f *ModelLegFold) Keep() bool {
	return f.anyYes || (f.anyProbed && !f.probedNo)
}
