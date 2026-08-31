package guard

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

// claimSet unit tests: overlap queries against sorted disjoint intervals.
func TestClaimSetOverlapsAndClaim(t *testing.T) {
	var c claimSet
	claim := func(s, e int) { c.claim(s, e) }
	claim(10, 20)
	claim(30, 40)
	cases := []struct {
		start, end int
		want       bool
	}{
		{0, 5, false},   // before
		{5, 10, false},  // touching leading edge (half-open)
		{10, 20, true},  // identical
		{12, 18, true},  // inside
		{15, 25, true},  // straddles trailing edge
		{20, 25, false}, // touching trailing edge (half-open)
		{21, 29, false}, // between intervals
		{0, 100, true},  // covering
		{35, 36, true},  // inside second
		{40, 50, false}, // after
		{10, 40, true},  // spanning the gap
	}
	for _, tc := range cases {
		if got := c.overlaps(tc.start, tc.end); got != tc.want {
			t.Errorf("overlaps(%d,%d) = %v, want %v", tc.start, tc.end, got, tc.want)
		}
	}
	// A non-overlapping insert between existing intervals keeps the set
	// sorted and disjoint.
	claim(22, 28)
	if !slices.IsSorted(c.starts) {
		t.Errorf("starts not sorted after middle insert: %v", c.starts)
	}
	if c.overlaps(20, 22) || c.overlaps(28, 30) {
		t.Error("adjacent-but-disjoint intervals must not overlap")
	}
}

// Decode budget: probe variants repeated at short intervals inside ONE token
// run longer than maxEncodedSpan make every expanded span a sliding window
// (start and end both advance), so per-probe containment dedup cannot
// collapse them. The per-scan decode budget must bound the encoded channel
// regardless of hit count.
func TestEncodedChannelDecodeBudget(t *testing.T) {
	v := b64Interior(base64.RawStdEncoding, []byte("glpat-"), 0)
	if v == "" {
		t.Fatal("no align-0 probe variant for glpat-")
	}
	// 'Z' keeps the run a single base64 token run while never completing a
	// hex probe (not a hex digit).
	unit := v + "Z"
	run := strings.Repeat(unit, (2*maxEncodedSpan+len(unit))/len(unit)+int(4096))
	s := mustScanner(t, nil, nil, nil)
	found, stats := s.findAllCounted([]byte(run))
	if stats.decodes > maxDecodesPerScan {
		t.Errorf("sliding-span adversary: decodes = %d, want <= %d (decode budget)",
			stats.decodes, maxDecodesPerScan)
	}
	if len(found) != 0 {
		t.Errorf("interleaved variant run must decode to garbage: found = %v", found)
	}
}

// Phase 1 must stop retaining cheap probe occurrences before phase 2. A
// three-byte probe separated into distinct token spans produces one offset per
// unit; the body deliberately exceeds the retention cap.
func TestPhase1ProbePositionBudget(t *testing.T) {
	s := &Scanner{probes: []encodedProbe{{variant: []byte("aaa")}}}
	s.compilePrefilter()
	body := []byte(strings.Repeat("aaa.", maxPhase1ProbePositionsPerScan+17))

	found, stats := s.findAllCounted(body)
	if len(found) != 0 {
		t.Fatalf("probe-only scanner found matches: %v", found)
	}
	if stats.phase1ProbePositions != maxPhase1ProbePositionsPerScan {
		t.Errorf("retained probe positions = %d, want cap %d",
			stats.phase1ProbePositions, maxPhase1ProbePositionsPerScan)
	}
	if stats.decodes > maxDecodesPerScan {
		t.Errorf("probe decodes = %d, want <= %d", stats.decodes, maxDecodesPerScan)
	}
}

// Repeated exact known-secret hits exercise both consumers of the automaton:
// findAll retains only a bounded raw-position tier, while ScanKnown retains no
// positions at all (one presence bit for the raw channel).
func TestPhase1KnownSecretPositionBudget(t *testing.T) {
	secret := strings.Repeat("a", minSecretLen)
	s := &Scanner{secrets: []knownSecretSet{buildSecretSet(secret, false)}}
	s.compilePrefilter()
	body := []byte(strings.Repeat("a", maxPhase1KnownRawPositionsPerScan+minSecretLen+17))

	found, stats := s.findAllCounted(body)
	if stats.phase1KnownRawPositions != maxPhase1KnownRawPositionsPerScan {
		t.Errorf("retained raw known-secret positions = %d, want cap %d",
			stats.phase1KnownRawPositions, maxPhase1KnownRawPositionsPerScan)
	}
	if stats.phase1KnownEncodedPositions != 0 {
		t.Errorf("retained encoded known-secret positions = %d, want 0", stats.phase1KnownEncodedPositions)
	}
	if len(found) == 0 || found[0].name != knownSecret {
		t.Fatalf("findAll known-secret matches = %v, want at least one %q match", found, knownSecret)
	}

	names, knownStats := s.scanKnownCounted(body)
	if len(names) != 1 || names[0] != knownSecret {
		t.Errorf("ScanKnown = %v, want [%s]", names, knownSecret)
	}
	if knownStats.phase1Presence != 1 {
		t.Errorf("ScanKnown retained presence state = %d, want 1", knownStats.phase1Presence)
	}
}

// Per-probe span dedup: a token run whose decoded text holds a truncated
// openai shape AND a complete gitlab token. openai_api_key sits earlier in
// the rule table, so its probe settles the shared span first and fails its
// regex — the gitlab probe must still get its own decode instead of being
// silenced by cross-probe containment dedup.
func TestEncodedChannelPerProbeSpanDedup(t *testing.T) {
	gl := "glpat-" + newFixtureRNG(0xC0FFEE).chars(20, alphaWord)
	text := "truncated: sk-ab\ncomplete: " + gl + "\n"
	blob := base64.StdEncoding.EncodeToString([]byte(text))
	s := mustScanner(t, nil, nil, nil)
	got := s.Scan([]byte("payload " + blob + " end"))
	if !slices.Contains(got, "gitlab_pat") {
		t.Errorf("later probe must not be silenced by earlier probe's settled span: Scan = %v, want gitlab_pat", got)
	}
	if slices.Contains(got, "openai_api_key") {
		t.Errorf("truncated sk- shape must not match: Scan = %v", got)
	}
}

// The encoded channel claims through the same capped path as every other
// source: with the claimed-match table one short of maxMatchesPerScan, the
// FIRST encoded hit still lands (filling the table) and the SECOND is
// dropped — a direct append would push the table past the bound the
// constant promises.
func TestEncodedChannelClaimsRespectMatchCap(t *testing.T) {
	secret := strings.Repeat("a", minSecretLen)
	s := mustScanner(t, nil, []string{secret}, nil)
	mk := func(seed uint64) string {
		gl := "glpat-" + newFixtureRNG(seed).chars(20, alphaWord)
		return base64.StdEncoding.EncodeToString([]byte("token: " + gl))
	}
	// cap-1 disjoint plaintext known-secret claims, then two encoded blobs.
	body := []byte(strings.Repeat(secret+" ", maxMatchesPerScan-1) + mk(0xB10B1) + " " + mk(0xB10B2))

	found, _ := s.findAllCounted(body)
	if len(found) != maxMatchesPerScan {
		t.Fatalf("claimed matches = %d, want exactly the cap %d (encoded channel must not exceed it)",
			len(found), maxMatchesPerScan)
	}
	encoded := 0
	for _, m := range found {
		if m.name == "gitlab_pat" {
			encoded++
		}
	}
	if encoded != 1 {
		t.Fatalf("encoded-channel claims at the cap boundary = %d, want 1 (first lands, second capped)", encoded)
	}
}

// Linear claiming: a body repeating one secret shape tens of thousands of
// times used to cost quadratic overlap checks (each hit scanned the whole
// claimed list). With the interval set every occurrence is still claimed and
// redacted, and the pass stays linear — this test completes in milliseconds,
// not the minutes the pairwise scan needed at this repetition count.
func TestScanRepeatedSecretStaysLinear(t *testing.T) {
	key := "sk-" + newFixtureRNG(0x5EED).chars(32, alphaWord)
	const n = 100000
	body := []byte(strings.Repeat(key+" ", n))
	s := mustScanner(t, nil, nil, nil)
	if got := s.Scan(body); len(got) != 1 || got[0] != "openai_api_key" {
		t.Errorf("Scan = %v, want [openai_api_key]", got)
	}
	out := string(s.Redact(body))
	if strings.Contains(out, key) {
		t.Error("Redact left the secret in place")
	}
	if c := strings.Count(out, RedactPlaceholder); c != n {
		t.Errorf("Redact replaced %d of %d occurrences", c, n)
	}
}
