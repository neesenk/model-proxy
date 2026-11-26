package wirecap

import (
	"errors"
	"testing"
)

// TestModelLegFold: the models-refresh keep rule and the leg→matrix mapping
// have one owner. The cases are the rule's own branches — any Yes keeps, an
// all-inconclusive sweep keeps, a definitive No drops, and a leg that was
// never probed is neither evidence nor a matrix entry beyond its
// definitional No.
func TestModelLegFold(t *testing.T) {
	// leg is one probe sample; want is the verdict Add must return for it.
	type leg struct {
		name   string
		probed bool
		status int
		err    error
		want   Verdict
	}
	ok := func(name string) leg { return leg{name: name, probed: true, status: 200, want: Yes} }
	notFound := func(name string) leg { return leg{name: name, probed: true, status: 404, want: No} }
	swept := func(name string) leg { return leg{name: name, probed: true, status: 429, want: Unknown} }
	down := func(name string) leg { return leg{name: name, probed: true, status: 503, want: Unknown} }
	unreachable := func(name string) leg {
		return leg{name: name, probed: true, err: errors.New("dial tcp: i/o timeout"), want: Unknown}
	}
	noBase := func(name string) leg { return leg{name: name, probed: false, want: No} }

	cases := []struct {
		name string
		legs []leg
		want ModelProtocols
		keep bool
	}{
		{
			name: "any leg yes keeps",
			legs: []leg{notFound("chat"), ok("responses")},
			want: ModelProtocols{Chat: No, Responses: Yes},
			keep: true,
		},
		{
			name: "all probed legs inconclusive keeps",
			legs: []leg{swept("chat"), down("responses"), noBase("anthropic")},
			want: ModelProtocols{Chat: Unknown, Anthropic: No, Responses: Unknown},
			keep: true,
		},
		{
			name: "a probed no with nothing else drops",
			legs: []leg{notFound("chat"), notFound("responses")},
			want: ModelProtocols{Chat: No, Responses: No},
			keep: false,
		},
		{
			name: "a probed no next to an inconclusive leg drops",
			legs: []leg{notFound("chat"), swept("responses")},
			want: ModelProtocols{Chat: No, Responses: Unknown},
			keep: false,
		},
		{
			name: "an unprobed no is not negative evidence",
			legs: []leg{noBase("chat"), ok("anthropic"), noBase("responses")},
			want: ModelProtocols{Chat: No, Anthropic: Yes, Responses: No},
			keep: true,
		},
		{
			name: "nothing probed drops",
			legs: []leg{noBase("chat"), noBase("responses")},
			want: ModelProtocols{Chat: No, Responses: No},
			keep: false,
		},
		{
			name: "an unreachable leg is inconclusive",
			legs: []leg{unreachable("chat")},
			want: ModelProtocols{Chat: Unknown},
			keep: true,
		},
		{
			name: "no legs at all drops",
			legs: nil,
			want: ModelProtocols{},
			keep: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var fold ModelLegFold
			for _, l := range tc.legs {
				v := fold.Add(l.name, l.probed, l.status, l.err, nil)
				if v != l.want {
					t.Fatalf("Add(%s) = %v, want %v", l.name, v, l.want)
				}
			}
			if got := fold.Matrix(); got != tc.want {
				t.Fatalf("Matrix = %+v, want %+v", got, tc.want)
			}
			if keep := fold.Keep(); keep != tc.keep {
				t.Fatalf("Keep = %v, want %v", keep, tc.keep)
			}
		})
	}

	// The mapping knows exactly the three legs: anything else is classified
	// (its verdict comes back) but recorded nowhere.
	var fold ModelLegFold
	if v := fold.Add("decisions", true, 200, nil, nil); v != Yes {
		t.Fatalf("Add(decisions) = %v, want the leg's own verdict Yes", v)
	}
	if got := fold.Matrix(); got != (ModelProtocols{}) {
		t.Fatalf("a non-matrix leg was recorded: %+v", got)
	}
}
