package guard

import (
	"strings"
	"testing"
)

// Secrets overlapping either edge of the snippet window must be clipped, not
// appended raw: an unclipped span drives pos past e0 (or behind s0) and
// panics the flushes (regression: slice bounds out of range in MaskSnippet,
// which killed the /api/security/explain handler mid-response).
func TestMaskSnippetSecretOverlapsWindowEdge(t *testing.T) {
	s, err := NewScanner(nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Two secrets back to back with a short gap; with ctx=10 the neighbor's
	// span crosses the window edge of whichever hit is rendered.
	first := "sk-ant-api03-X9fQ2vB7nM4kL8pR1tW6yU3iO0aS5dF7gH9jK2lZ4"
	second := "sk-ant-api03-Aa1Bb2Cc3Dd4Ee5Ff6Gg7Hh8Ii9Jj0Kk1Ll2Mm3Nn4Oo5"
	body := []byte(`{"a":"` + first + `","b":"` + second + `"}`)
	matches := s.Locate(body, []string{"anthropic_api_key"})
	if len(matches) != 2 {
		t.Fatalf("Locate found %d matches, want 2", len(matches))
	}
	for _, tc := range []struct {
		name string
		m    LocatedMatch
	}{
		{"right edge (trailing secret crosses e0)", matches[0]},
		{"left edge (leading secret crosses s0)", matches[1]},
	} {
		pre, hit, post := s.MaskSnippet(body, tc.m.Start, tc.m.End, true, 10)
		if strings.Contains(pre+hit+post, first) || strings.Contains(pre+hit+post, second) {
			t.Errorf("%s: snippet leaks a secret: %q", tc.name, pre+hit+post)
		}
		if !strings.HasPrefix(hit, "sk-a") || !strings.Contains(hit, "…") {
			t.Errorf("%s: masked hit = %q, want head…tail form", tc.name, hit)
		}
		// The clipped neighbor appears masked (head…tail, or *** when the
		// clipped remainder is shorter than 8 bytes) in the context on its side
		// of the hit — never as raw key bytes.
		masked := func(ctx string) bool {
			return strings.Contains(ctx, "…") || strings.Contains(ctx, "***")
		}
		if tc.m == matches[0] && !masked(post) {
			t.Errorf("right edge: clipped trailing secret missing from post: %q", post)
		}
		if tc.m == matches[1] && !masked(pre) {
			t.Errorf("left edge: clipped leading secret missing from pre: %q", pre)
		}
	}
}
