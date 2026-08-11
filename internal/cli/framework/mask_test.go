package framework

import (
	"testing"
)

func TestMask(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "(empty)"},
		{"ab", "****"},      // too short → full mask
		{"abcdefg", "****"}, // len 7 < 8 → full mask
		{"abcdefgh", "ab…gh"},
		{"SSO_C=verylongsecrettoken123", "SS…23"},
		{"d042cb5f6e8b8d4e8f7a413475dad38fd3d4c873ea6daf533ba7d5a60ec6720a", "d0…0a"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Errorf("Mask(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestMask_NeverLeaksRaw ensures a full secret never appears in masked output.
func TestMask_NeverLeaksRaw(t *testing.T) {
	secret := "AKIAIOSFODNN7EXAMPLE"
	got := Mask(secret)
	if got == secret {
		t.Fatalf("mask leaked raw secret: %q", got)
	}
	// Only first 2 + last 2 may appear.
	if len(got) >= len(secret) {
		t.Errorf("masked output not shorter than secret: got %q", got)
	}
}
