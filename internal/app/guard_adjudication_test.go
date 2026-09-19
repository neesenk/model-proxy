package app

import (
	"encoding/json"
	"testing"
)

// TestExtractFirstJSONObject_NestedBraces: the judge reply may contain braces
// inside the reason string; the extractor must return the outer JSON object,
// not the first flat brace pair.
func TestExtractFirstJSONObject_NestedBraces(t *testing.T) {
	inner := `{"risk":"high","reason":"because {\"foo\":\"bar\"}","evidence":"x"}`
	text := "thinking... " + inner + " trailing"
	got := extractFirstJSONObject(text)
	if got == "" {
		t.Fatal("extractor returned empty for a valid nested object")
	}
	var r adjudicationReply
	if err := json.Unmarshal([]byte(got), &r); err != nil {
		t.Fatalf("extracted object is not valid JSON: %v\nextracted: %s", err, got)
	}
	if r.Risk != "high" || r.Reason != `because {"foo":"bar"}` {
		t.Errorf("parsed reply = %+v, want high risk with nested reason", r)
	}
}

func TestExtractFirstJSONObject_Boundaries(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain object", `{"a":1}`, `{"a":1}`},
		{"surrounded by text", `prefix {"a":1} suffix`, `{"a":1}`},
		{"no object", `just text`, ""},
		{"unbalanced", `{"a":1`, ""},
		{"nested objects", `{"outer":{"inner":2}}`, `{"outer":{"inner":2}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFirstJSONObject(tc.input)
			if got != tc.want {
				t.Errorf("extractFirstJSONObject(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
