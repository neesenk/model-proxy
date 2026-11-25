package adjudicate

// r6_scrub_test.go pins the fragment-mask contract of scrub (R6 review B7):
// every byte of the ORIGINAL text that lies inside an occurrence of some
// >= 8-byte substring of the hit is masked, and nothing else is. The oracle
// below states that contract declaratively (substring search + union of
// window occurrences) so the assertions do not depend on how scrub is
// implemented.

import (
	"strings"
	"testing"
)

const (
	r6Window = 8
	r6Mask   = "[MASKED]"
)

// r6AnyStart reports whether any position in [lo, hi) starts a window.
func r6AnyStart(starts []bool, lo, hi int) bool {
	if lo < 0 {
		lo = 0
	}
	for ; lo < hi; lo++ {
		if starts[lo] {
			return true
		}
	}
	return false
}

// r6ExpectedScrub is the declarative fragment mask for a hit of at least one
// window: a text byte is masked when it lies inside an occurrence of an
// 8-byte window of hit (every >= 8-byte fragment occurrence is a chain of
// such windows, and no other byte belongs to one), found by plain substring
// search. Consecutive masked bytes form one token, except where no window
// starts in the 7 bytes before a window start — there the chain is broken
// and a second token begins. hitBytes (optional) prefilters windows that
// contain a byte hit does not have at all; such a window cannot occur in hit.
func r6ExpectedScrub(hit, text string, hitBytes map[byte]bool) string {
	starts := make([]bool, len(text))
	for p := 0; p+r6Window <= len(text); p++ {
		w := text[p : p+r6Window]
		if hitBytes != nil {
			foreign := false
			for i := 0; i < len(w); i++ {
				if !hitBytes[w[i]] {
					foreign = true
					break
				}
			}
			if foreign {
				continue
			}
		}
		starts[p] = strings.Contains(hit, w)
	}
	masked := make([]bool, len(text))
	for p, s := range starts {
		if s {
			for k := 0; k < r6Window; k++ {
				masked[p+k] = true
			}
		}
	}
	var b strings.Builder
	for p := 0; p < len(text); {
		if !masked[p] {
			b.WriteByte(text[p])
			p++
			continue
		}
		for p < len(text) && masked[p] {
			if starts[p] && !r6AnyStart(starts, p-r6Window+1, p) {
				b.WriteString(r6Mask)
			}
			p++
		}
	}
	return b.String()
}

// TestScrub_R6_ExactMasks pins the exact output for the mask shapes scrub
// must produce. Cases marked "superset"/"intentional" record where the
// previous per-substring ReplaceAll differed: it masked only the first
// matching 8-byte window of a longer fragment (leaving a < 8-byte tail),
// cut genuine fragment bytes off at a full-hit boundary, masked non-fragment
// text that happened to follow a mask token, and rewrote its own token.
func TestScrub_R6_ExactMasks(t *testing.T) {
	cases := []struct {
		name, hit, text, want string
	}{
		{"full hit", "sk-proj-abcdefghij1234567890",
			"echoes sk-proj-abcdefghij1234567890 and \x1b[31mcolor\n",
			"echoes [MASKED] and  [31mcolor "},
		{"chained fragment collapses into one mask (was: [MASKED]klmn)",
			"abcdefghijklmnop", "xxcdefghijklmnyy", "xx[MASKED]yy"},
		{"overlapping periodic windows chain (was: [MASKED]ab)",
			"abababababababab", "xxabababababyy", "xx[MASKED]yy"},
		{"adjacent fragments are separate masks",
			"abcdefghijklmnopqrstuvwxyz", "AAcdefghijstuvwxyzBB", "AA[MASKED][MASKED]BB"},
		{"seven-byte gap between fragments survives",
			"abcdefghijklmnopqrstuvwxyz", "cdefghij1234567stuvwxyz", "[MASKED]1234567[MASKED]"},
		{"no window start between two windows: two tokens for one masked range",
			"abcdefghPPPPPPPPstuvwxYY", "abcdefghstuvwxYY", "[MASKED][MASKED]"},
		{"genuine fragment before a full hit is masked too (was: q[MASKED])",
			"abcdefghqabcdefg", "qabcdefghqabcdefg", "[MASKED]"},
		{"eight-byte fragment masks, seven-byte one survives",
			"abcdefghijklmnopqrstuvwxyz", "cdefghi|cdefghij|cdefghij", "cdefghi|[MASKED]|[MASKED]"},
		{"fragment at the text start and a sub-window tail at the end",
			"abcdefghijklmnop", "cdefghij|tail|bcdefgh", "[MASKED]|tail|bcdefgh"},
		{"window-sized hit",
			"abcdefgh", "xabcdefghyabcdefgh", "x[MASKED]y[MASKED]"},
		{"multibyte hit: byte windows, whole fragment masked (was: [MASKED]\\xadストB)",
			"日本語テキストsecret", "A語テキストB", "A[MASKED]B"},
		{"invalid utf-8 becomes U+FFFD before masking",
			"abcdefgh", "x\xffabcdefgh", "x\ufffd[MASKED]"},
		{"control characters collapse before masking",
			"abcdefgh", "ab\ncdefgh\x7f", "ab cdefgh "},
		{"short hit is masked wholesale",
			"abc", "the abc token", "the [MASKED] token"},
		{"sub-window fragment of a long hit survives",
			"sk-proj-abcdefghij1234567890", "token k-proj- end", "token k-proj- end"},
		{"empty hit only collapses control characters",
			"", "nothing\x1bto mask\n", "nothing to mask "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := scrub(tc.hit, tc.text); got != tc.want {
				t.Errorf("scrub(%q, %q) = %q, want %q", tc.hit, tc.text, got, tc.want)
			}
		})
	}
}

// TestScrub_R6_MaskLiteralHitIsNotText covers hits that themselves contain
// the mask token or its edge pieces. The scanner must never read the tokens
// it emits as text: only bytes that are part of a fragment occurrence IN THE
// ORIGINAL TEXT are masked, and every emitted token stays intact. The old
// enumeration matched "[MASKED]" + following text against fragments of such
// hits — masking bytes that belong to no fragment (and rewriting the token).
func TestScrub_R6_MaskLiteralHitIsNotText(t *testing.T) {
	cases := []struct {
		name, hit, text, want string
	}{
		{"token + following text is not a fragment (was: [MASKE[MASKED])",
			"ppppppppD]abcdef", "ppppppppabcdef", "[MASKED]abcdef"},
		{"full hit, then a repeat that is no fragment (was: [MASKED]cdefgh)",
			"[MASKED]bcdefgh", "[MASKED]bcdefghbcdefgh", "[MASKED]bcdefgh"},
		{"full hit + fragments + sub-window decoy",
			"tok-[MASKED]-abcdefgh",
			"<p>tok-[MASKED]-abcdefgh</p> and [MASKED]-abc ... ASKED]- tail",
			"<p>[MASKED]</p> and [MASKED] ... ASKED]- tail"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scrub(tc.hit, tc.text)
			if got != tc.want {
				t.Fatalf("scrub(%q, %q) = %q, want %q", tc.hit, tc.text, got, tc.want)
			}
			// Every hit fragment (>= 8 bytes) occurring in the original text
			// is gone: the text that survives next to the tokens holds no
			// window of the hit.
			survivors := strings.ReplaceAll(got, r6Mask, "")
			for p := 0; p+r6Window <= len(survivors); p++ {
				if strings.Contains(tc.hit, survivors[p:p+r6Window]) {
					t.Fatalf("hit fragment %q survives at %d: %q", survivors[p:p+r6Window], p, survivors)
				}
			}
			// The mask literals themselves are intact: no "[" outside a
			// complete token (no partially rewritten or self-masked token).
			if strings.Contains(survivors, "[") || strings.Count(got, "[") != strings.Count(got, r6Mask) {
				t.Fatalf("mask literals not intact: %q", got)
			}
			if expected := r6ExpectedScrub(tc.hit, tc.text, nil); got != expected {
				t.Fatalf("scrub = %q, want the declarative mask %q", got, expected)
			}
		})
	}
}

// r6Rng is a small deterministic generator so the samples below do not depend
// on math/rand's sequence.
type r6Rng struct{ s uint64 }

func (r *r6Rng) next() uint64 {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	return r.s >> 33
}

func (r *r6Rng) intn(n int) int { return int(r.next() % uint64(n)) }

func (r *r6Rng) pick(alphabet []string) string { return alphabet[r.intn(len(alphabet))] }

// TestScrub_R6_MatchesDeclarativeMask draws hits and texts from tiny
// alphabets (dense overlaps, periodic windows, broken chains) and, in a third
// of the samples, from an alphabet that includes the mask token and its edge
// pieces, and checks each scrub against the declarative mask.
func TestScrub_R6_MatchesDeclarativeMask(t *testing.T) {
	plain := [][]string{
		{"a", "b"},
		{"a", "b", "c"},
		{"s", "k", "-", "p", "r", "o", "j", "0", "1"},
	}
	withToken := []string{"a", "b", "[MASKED]", "[", "]", "D]", "[M", "KED]"}
	rng := &r6Rng{s: 20261125}
	for i := 0; i < 4000; i++ {
		symbols := plain[i%len(plain)]
		if i%3 == 0 {
			symbols = withToken
		}
		hitLen := r6Window + rng.intn(40)
		hitParts := make([]string, hitLen)
		for k := range hitParts {
			hitParts[k] = rng.pick(symbols)
		}
		hit := strings.Join(hitParts, "")
		textLen := rng.intn(300)
		textParts := make([]string, textLen)
		for k := range textParts {
			textParts[k] = rng.pick(symbols)
		}
		text := strings.Join(textParts, "")
		want := r6ExpectedScrub(hit, text, nil)
		if got := scrub(hit, text); got != want {
			t.Fatalf("sample %d: scrub(hit=%q, text=%q) = %q, want %q", i, hit, text, got, want)
		}
		// Coverage, stated on the original text: no >= 8-byte hit substring
		// occurs in the text that survives next to the emitted tokens.
		for p := 0; p+r6Window <= len(want); p++ {
			w := want[p : p+r6Window]
			if strings.Contains(w, "[") || strings.Contains(w, "]") {
				continue // spans an emitted token
			}
			if strings.Contains(hit, w) {
				t.Fatalf("sample %d: hit window %q survives: %q", i, w, want)
			}
		}
	}
}

// TestScrub_R6_LargeSampleMasksExactFragments drives the sizes the old
// per-substring enumeration could not finish (a 64KB hit has ~2e9 substrings;
// measured on the old code: 8KB hit = 11s, ~6x per doubling) and pins the
// exact result: planted fragments collapse into one token each, sub-window
// decoys survive, and every byte inside a fragment occurrence is masked.
func TestScrub_R6_LargeSampleMasksExactFragments(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	rng := &r6Rng{s: 7}
	hitBytes := make([]byte, 64*1024)
	seen := map[byte]bool{}
	for i := range hitBytes {
		hitBytes[i] = alphabet[rng.intn(len(alphabet))]
		seen[hitBytes[i]] = true
	}
	hit := string(hitBytes)
	// Filler alphabet disjoint from the hit's: no window mixing filler and
	// planted bytes can occur in the hit, so the mask plan below is exact.
	filler := []byte("~ ^\n")
	for _, c := range filler {
		if seen[c] {
			t.Fatalf("filler byte %q also occurs in the hit", c)
		}
	}

	segment := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = filler[rng.intn(len(filler))]
		}
		return string(b)
	}
	frag := func(from, to int) string { return hit[from:to] }

	type piece struct {
		text   string
		masked bool
	}
	adjacentA, adjacentB := frag(1000, 1040), frag(1030, 1050)
	tail, decoyAfter := frag(60000, 60017), frag(40000, 40007)
	pieces := []piece{
		{frag(32768, 32776), true}, // exactly one window, at the text start
		{segment(40 * 1024), false},
		{adjacentA, true}, // 40-byte fragment
		{adjacentB, true}, // adjacent overlapping fragment: a second token
		{segment(30 * 1024), false},
		{frag(2000, 2007), false}, // 7-byte decoy
		{segment(1024), false},
		{tail, true},        // 17-byte fragment
		{decoyAfter, false}, // 7-byte decoy directly after a fragment
		{segment(20 * 1024), false},
		{frag(0, len(hit)), true}, // the full 64KB hit
		{segment(30 * 1024), false},
		{frag(50000, 50032), true}, // 32-byte fragment ending the text
	}
	// The hand-written expectation below relies on no mask run chaining
	// across the two boundaries where planted hit material sits next to hit
	// material. That is a property of the random hit (about 1 in 36 per
	// boundary), so it is checked rather than assumed.
	r6AssertNoStraddle(t, hit, adjacentA, adjacentB)
	r6AssertNoStraddle(t, hit, tail, decoyAfter)
	var textLen int
	for _, p := range pieces {
		textLen += len(p.text)
	}
	// Pad in front of the closing fragment so it still ends the text.
	if pad := 256*1024 - textLen; pad > 0 {
		pieces = append(pieces[:len(pieces)-1], piece{segment(pad), false}, pieces[len(pieces)-1])
		textLen += pad
	} else if pad < 0 {
		t.Fatalf("fixture overflows: %d bytes", textLen)
	}

	// scrub collapses the filler's newlines to spaces before masking; the
	// expectation is stated on that collapsed text.
	var text, want strings.Builder
	text.Grow(textLen)
	for _, p := range pieces {
		text.WriteString(p.text)
		if p.masked {
			want.WriteString(r6Mask)
		} else {
			want.WriteString(strings.ReplaceAll(p.text, "\n", " "))
		}
	}
	if text.Len() != 256*1024 || len(hit) != 64*1024 {
		t.Fatalf("fixture sizes: text %d bytes, hit %d bytes; want 256KB / 64KB", text.Len(), len(hit))
	}
	got := scrub(hit, text.String())
	if at := strings.Index(got, "\n"); at >= 0 {
		t.Fatalf("large sample: newline survived the control-character collapse at %d", at)
	}
	if got != want.String() {
		at := r6FirstDiff(got, want.String())
		t.Fatalf("large sample: scrub output mismatch at byte %d (len got %d, want %d)", at, len(got), want.Len())
	}

	// Coverage stated on the original text, via the declarative mask: every
	// byte inside a fragment occurrence is masked, every other byte survives.
	collapsed := strings.ReplaceAll(text.String(), "\n", " ")
	if expected := r6ExpectedScrub(hit, collapsed, seen); got != expected {
		at := r6FirstDiff(got, expected)
		t.Fatalf("large sample: scrub disagrees with the declarative mask at byte %d", at)
	}
}

// r6AssertNoStraddle fails when a window of hit spans the boundary between
// two adjacent planted fragments left and right (the text has left
// immediately followed by right). Such a window would chain the left
// fragment's mask run into the right one and invalidate an expectation that
// treats the two as separate pieces.
func r6AssertNoStraddle(t *testing.T, hit, left, right string) {
	t.Helper()
	if len(left) < r6Window-1 || len(right) < r6Window-1 {
		t.Fatalf("fixture: fragments of %d and %d bytes are too short to straddle", len(left), len(right))
	}
	for k := 1; k < r6Window; k++ {
		w := left[len(left)-k:] + right[:r6Window-k]
		if strings.Contains(hit, w) {
			t.Fatalf("fixture: window %q (%d byte(s) of the left fragment + the start of the right one) occurs in the hit; pick other fragment offsets", w, k)
		}
	}
}

// r6FirstDiff returns the offset of the first differing byte of a and b, or
// min(len(a), len(b)) when one is a prefix of the other.
func r6FirstDiff(a, b string) int {
	n := min(len(a), len(b))
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}
