package guard

import (
	"bytes"
	"strings"
	"testing"
)

func buildTestAC(t *testing.T, needles ...string) (*acMatcher, []string) {
	t.Helper()
	b := newACBuilder()
	for i, n := range needles {
		b.add([]byte(n), i)
	}
	return b.compile(), needles
}

// searchAll returns "id@end" for every match, in emission order.
func searchAll(m *acMatcher, body string) []string {
	var got []string
	m.search([]byte(body), func(id, end int) {
		got = append(got, strings.Join([]string{itoa(id), itoa(end)}, "@"))
	})
	return got
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [8]byte
	p := len(buf)
	for i > 0 {
		p--
		buf[p] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[p:])
}

// Classic Aho-Corasick example: suffix matches must fire through
// dictionary-suffix links.
func TestACSuffixMatches(t *testing.T) {
	m, _ := buildTestAC(t, "he", "she", "his", "hers")
	got := strings.Join(searchAll(m, "ushers"), " ")
	// "she" ends at 5, "he" ends at 5 (suffix) and at 6? "ushers": u s h e r s
	// she → end 4? offsets: s(1) h(2) e(3) → "she" ends at 4; "he" ends at 4.
	// "hers" → ends at 6; "he"+"ers"... "hers" contains "he" at end 4 only.
	for _, want := range []string{"1@4", "0@4", "3@6"} {
		if !strings.Contains(got, want) {
			t.Errorf("search(ushers) = %s, want match %s", got, want)
		}
	}
}

// Duplicate needle strings record every id on the shared terminal state.
func TestACDuplicateNeedles(t *testing.T) {
	m, _ := buildTestAC(t, "abc", "abc", "bc")
	got := strings.Join(searchAll(m, "abc"), " ")
	for _, want := range []string{"0@3", "1@3", "2@3"} {
		if !strings.Contains(got, want) {
			t.Errorf("search(abc) = %s, want match %s", got, want)
		}
	}
}

// Occurrences of one needle are reported in increasing end order, including
// overlapping ones.
func TestACEndOrderAndOverlap(t *testing.T) {
	m, _ := buildTestAC(t, "aa")
	var ends []int
	m.search([]byte("aaaa"), func(id, end int) { ends = append(ends, end) })
	if len(ends) != 3 || ends[0] != 2 || ends[1] != 3 || ends[2] != 4 {
		t.Errorf("search(aaaa) ends = %v, want [2 3 4]", ends)
	}
}

// Every needle registered in a Scanner's automaton must be found when present
// in a body — guards against automaton construction bugs silently disabling
// a rule's prefilter.
func TestPrefilterCoversAllNeedles(t *testing.T) {
	s, err := NewScanner(nil, Known([]string{"synthetic-pool-secret-1234"}...), nil)
	if err != nil {
		t.Fatal(err)
	}
	needles := map[int][]byte{}
	for i, r := range s.rules {
		_ = i
		for _, lit := range r.literals {
			needles[len(needles)] = lit
		}
	}
	for _, p := range s.probes {
		needles[len(needles)] = p.variant
	}
	for _, sec := range s.secrets {
		needles[len(needles)] = sec.raw
		for _, v := range sec.encoded {
			needles[len(needles)] = v
		}
	}
	for _, p := range builtinPaths {
		for _, lit := range p.literals {
			needles[len(needles)] = lit
		}
	}
	for _, lit := range s.extraPaths {
		needles[len(needles)] = lit
	}
	// Concatenate with a non-token separator and require every needle to be
	// found at least once.
	parts := make([][]byte, 0, len(needles))
	for _, n := range needles {
		parts = append(parts, n)
	}
	body := bytes.Join(parts, []byte("\n"))
	hit := map[int]bool{}
	s.ac.search(body, func(id, end int) { hit[id] = true })
	for id := range s.refs {
		if !hit[id] {
			ref := s.refs[id]
			t.Errorf("needle id %d (kind %d idx %d vidx %d) never found in corpus", id, ref.kind, ref.idx, ref.vidx)
		}
	}
}

// TestACMatchesBruteForceSpec checks the automaton against the definition of
// its output on random needle sets (fixed seed): the emitted (id, end) pairs
// are exactly those with body[end-len(needle):end] == needle — every
// occurrence, overlaps and duplicate needles included — and each needle's
// occurrences arrive in increasing end order. The alphabet includes a high
// byte and a byte no needle uses.
func TestACMatchesBruteForceSpec(t *testing.T) {
	alphabet := []byte{'a', 'b', 'c', 0xff, '-'}
	state := uint64(0x9e3779b97f4a7c15)
	next := func(n int) int {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return int(state % uint64(n))
	}
	randomBytes := func(n int, letters []byte) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = letters[next(len(letters))]
		}
		return out
	}
	for round := 0; round < 400; round++ {
		needles := make([][]byte, 1+next(8))
		for i := range needles {
			needles[i] = randomBytes(1+next(5), alphabet[:4]) // '-' never in a needle
		}
		b := newACBuilder()
		for id, needle := range needles {
			b.add(needle, id)
		}
		m := b.compile()
		body := randomBytes(next(80), alphabet)

		want := map[[2]int]bool{}
		for end := 1; end <= len(body); end++ {
			for id, needle := range needles {
				if len(needle) <= end && bytes.Equal(body[end-len(needle):end], needle) {
					want[[2]int{id, end}] = true
				}
			}
		}
		got := map[[2]int]bool{}
		lastEnd := map[int]int{}
		m.search(body, func(id, end int) {
			key := [2]int{id, end}
			if got[key] {
				t.Fatalf("round %d: (%d,%d) emitted twice", round, id, end)
			}
			got[key] = true
			if end <= lastEnd[id] {
				t.Fatalf("round %d: needle %d ends not increasing (%d after %d)", round, id, end, lastEnd[id])
			}
			lastEnd[id] = end
		})
		if len(got) != len(want) {
			t.Fatalf("round %d: needles %q body %q: got %d matches, want %d (%v vs %v)", round, needles, body, len(got), len(want), got, want)
		}
		for key := range want {
			if !got[key] {
				t.Fatalf("round %d: needles %q body %q: missing match %v", round, needles, body, key)
			}
		}
	}
}

// TestACFullByteAlphabet: needles may use every byte value (known secrets
// and their encodings are arbitrary bytes); the class table must not wrap.
func TestACFullByteAlphabet(t *testing.T) {
	b := newACBuilder()
	for c := 0; c < 256; c++ {
		b.add([]byte{byte(c), byte(255 - c)}, c)
	}
	m := b.compile()
	var got []int
	m.search([]byte{0, 255, 7, 248, 255, 0}, func(id, end int) { got = append(got, id*1000+end) })
	want := []int{0*1000 + 2, 7*1000 + 4, 255*1000 + 6}
	if len(got) != len(want) {
		t.Fatalf("matches = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matches = %v, want %v", got, want)
		}
	}
}
