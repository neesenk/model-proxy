package config

// RoutingDecision records the route-tier policy outcome for one request.
// It lives in the config package so it can be carried through the execution
// engine (targetexec) and the request log without creating a forbidden
// dependency between those two packages. It is log metadata only: no request
// or response text, only structural names and selector confidence/difficulty.
type RoutingDecision struct {
	Source   string          `json:"source"`
	Grade    string          `json:"grade,omitempty"`
	Target   string          `json:"target,omitempty"`
	Selector *SelectorChoice `json:"selector,omitempty"`
	Latch    string          `json:"latch,omitempty"`
}

// SelectorChoice records one route-level selector invocation result.
type SelectorChoice struct {
	Choice     string  `json:"choice"`
	Confidence float64 `json:"confidence"`
	Difficulty float64 `json:"difficulty"`
	Enforced   bool    `json:"enforced"`
	Err        string  `json:"err,omitempty"`
}
