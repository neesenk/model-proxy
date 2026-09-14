package session

import (
	"fmt"
	"strings"
	"testing"

	"model-proxy/internal/guard"
)

// fragPoolKey is a synthetic 40-char pool key, splittable into fragments that
// each clear guard's minKnownFrag (8). All fixtures are synthetic — never
// real credentials (AGENTS.md credential red line), and failure output must
// not echo fixture bytes.
var fragPoolKey = "poolkey-" + strings.Repeat("zK7v", 8)

// mustGuardScanner builds a known-secret scanner for store-level tests; each
// call returns a distinct pointer (a fresh "generation").
func mustGuardScanner(t *testing.T) *guard.Scanner {
	t.Helper()
	sc, err := guard.NewScannerWithOptions(nil, guard.Known([]string{fragPoolKey}...), nil, guard.Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// Store-level bounds: tail truncation, LRU eviction, generation reset.
func TestSessionScanStore_Bounds(t *testing.T) {
	s := NewStore()
	sc := mustGuardScanner(t)

	// Tail truncation keeps only the last sessionScanWindowBytes.
	big := strings.Repeat("a", sessionScanWindowBytes+100)
	s.Add("s", []byte("HEAD-"+big), sc, nil, nil, false)
	tail, _, _, _ := s.Snapshot("s", sc)
	if len(tail) != sessionScanWindowBytes {
		t.Errorf("window len = %d, want capped at %d", len(tail), sessionScanWindowBytes)
	}
	if strings.HasPrefix(string(tail), "HEAD-") {
		t.Errorf("window must keep the TAIL, not the head")
	}

	// LRU: more than sessionScanMaxSessions sessions evicts the oldest
	// (including "s", idle since before the flood).
	for i := 0; i < sessionScanMaxSessions+20; i++ {
		s.Add(fmt.Sprintf("sess-%d", i), []byte("x"), sc, nil, nil, false)
	}
	if n := s.Len(); n != sessionScanMaxSessions {
		t.Errorf("sessions = %d, want capped at %d", n, sessionScanMaxSessions)
	}
	if tail, _, _, _ := s.Snapshot("sess-0", sc); tail != nil {
		t.Errorf("oldest session must be evicted")
	}
	if tail, _, _, _ := s.Snapshot(fmt.Sprintf("sess-%d", sessionScanMaxSessions+19), sc); tail == nil {
		t.Errorf("newest session must survive eviction")
	}

	// A scanner-generation change invalidates fragment progress.
	s2 := NewStore()
	s2.Add("g", []byte("body"), sc, []int{12}, nil, false)
	if _, progress, _, _ := s2.Snapshot("g", mustGuardScanner(t)); progress != nil {
		t.Errorf("progress must reset across scanner generations, got %v", progress)
	}
}

// A body larger than the window must contribute exactly its last
// sessionScanWindowBytes — the pre-lock slice (bounded copy) must produce the
// same window as appending the full body ever did.
func TestSessionScanStore_LargeBodyKeepsExactTail(t *testing.T) {
	s := NewStore()
	sc := mustGuardScanner(t)
	s.Add("s", []byte("PREFIX-"), sc, nil, nil, false)
	big := strings.Repeat("b", 1<<20) + "TAILMARK"
	s.Add("s", []byte(big), sc, nil, nil, false)
	tail, _, _, _ := s.Snapshot("s", sc)
	full := "PREFIX-" + big
	want := full[len(full)-sessionScanWindowBytes:]
	if string(tail) != want {
		t.Errorf("large-body window mismatch: len %d, want the exact last %d bytes (tail %q...)",
			len(tail), sessionScanWindowBytes, tail[len(tail)-8:])
	}
	if !strings.HasSuffix(string(tail), "TAILMARK") {
		t.Errorf("window must end with the body's own tail")
	}
}

// The scanner-declared reset zeroes exactly the completed secret's stored
// progress, while an ordinary clean request (no fragment, no reset) keeps the
// max-merged progress.
func TestSessionScanStore_ProgressResetHonored(t *testing.T) {
	s := NewStore()
	sc := mustGuardScanner(t)
	s.Add("s", []byte("prefix-frag"), sc, []int{20}, nil, false)
	// Clean request: next[0]=0 means "no fragment seen", NOT a reset — the
	// max-merge must keep the stored 20.
	s.Add("s", []byte("clean"), sc, []int{0}, nil, false)
	_, progress, _, _ := s.Snapshot("s", sc)
	if len(progress) != 1 || progress[0] != 20 {
		t.Fatalf("clean request must keep stored progress, got %v", progress)
	}
	// Completing request: reset[0] makes the returned 0 authoritative.
	s.Add("s", []byte("suffix-frag"), sc, []int{0}, []bool{true}, false)
	_, progress, _, _ = s.Snapshot("s", sc)
	if len(progress) != 1 || progress[0] != 0 {
		t.Fatalf("declared reset must zero stored progress, got %v", progress)
	}
}

// The cached window verdict must track window content: clean after clean
// adds, hit after the full key lands, invalid across scanner generations.
func TestSessionScanStore_KnownVerdictCache(t *testing.T) {
	s := NewStore()
	sc := mustGuardScanner(t)
	s.Add("s", []byte("clean body"), sc, nil, nil, false)
	_, _, known, ok := s.Snapshot("s", sc)
	if !ok || known {
		t.Errorf("clean window verdict = (%v, %v), want (false, true)", known, ok)
	}
	// bodyKnown=true (the caller's per-request verdict) must publish a hit
	// without any window rescan.
	s.Add("s", []byte(fragPoolKey), sc, nil, nil, true)
	_, _, known, ok = s.Snapshot("s", sc)
	if !ok || !known {
		t.Errorf("window holding the full key verdict = (%v, %v), want (true, true)", known, ok)
	}
	// A generation change invalidates the cache for the new scanner.
	if _, _, _, ok := s.Snapshot("s", mustGuardScanner(t)); ok {
		t.Error("verdict cache must not carry across scanner generations")
	}
}

// The cached window verdict must catch a key that is contiguous ONLY across
// the old-window/body junction (the incremental refresh scans the junction
// region, not the whole window), and a truncation must clear a stale hit via
// the full rescan.
func TestSessionScanStore_KnownVerdictAcrossJunction(t *testing.T) {
	s := NewStore()
	sc := mustGuardScanner(t)
	s.Add("s", []byte("pad "+fragPoolKey[:20]), sc, nil, nil, false)
	if _, _, known, ok := s.Snapshot("s", sc); !ok || known {
		t.Fatalf("half key in window: verdict = (%v, %v), want (false, true)", known, ok)
	}
	s.Add("s", []byte(fragPoolKey[20:]+" pad"), sc, nil, nil, false)
	if _, _, known, ok := s.Snapshot("s", sc); !ok || !known {
		t.Errorf("junction-spanning key must flip the cached verdict to hit, got (%v, %v)", known, ok)
	}
	// Pushing everything out of the window drops the key: the truncation
	// path rescans the new window and clears the verdict.
	s.Add("s", []byte(strings.Repeat("y", sessionScanWindowBytes)), sc, nil, nil, false)
	if _, _, known, ok := s.Snapshot("s", sc); !ok || known {
		t.Errorf("after truncation: verdict = (%v, %v), want (false, true)", known, ok)
	}
}
