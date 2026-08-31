package app

import (
	"container/list"
	"sync"

	"model-proxy/internal/guard"
)

// Split-exfiltration detection bounds. A session window keeps only the TAIL
// of recent request bodies: a secret split across requests reassembles only
// while every fragment still sits inside the window.
const (
	// sessionScanWindowBytes caps one session's retained tail. A secret whose
	// fragments land more than 32KiB of body traffic apart is NOT reassembled
	// (documented limitation — the window is a bounded heuristic, not a
	// session recorder). Truncation also resets the session's fragment
	// progress: once earlier bytes are gone, their partial matches must not
	// credit later completions.
	sessionScanWindowBytes = 32 << 10
	// sessionScanMaxSessions bounds the LRU: 256 sessions × 32KiB ≤ 8MiB total.
	sessionScanMaxSessions = 256
)

// sessionScanEntry is one session's retained request-body tail plus its
// cross-request fragment progress (indexed by the scanner's known-secret
// index — secret VALUES are never stored here).
type sessionScanEntry struct {
	id       string
	window   []byte
	scanner  *guard.Scanner // generation that progress belongs to
	progress []int          // per-secret matched prefix length; nil = none
	// known/knownValid cache the known-secret channel verdict (would
	// ScanKnown(window) hit?) for the CURRENT window and scanner generation.
	// The split-exfil pass needs the tail-alone verdict on every request to
	// exclude secrets already fully inside the window; recomputing it per
	// request would re-scan up to 32KiB even though the window only changes
	// through this session's own Adds. Add refreshes the cache outside the
	// store lock (see its comment); knownValid=false means "unknown" and
	// callers fall back to scanning the tail themselves.
	known      bool
	knownValid bool
	// seq versions window mutations so the unlocked verdict refresh can tell
	// whether the window it scanned is still current before publishing.
	seq uint64
}

// sessionScanStore is a bounded LRU mapping x-claude-code-session-id → the
// tail of that session's recent request bodies + fragment progress, for
// split-exfiltration detection (a known credential smuggled out in pieces,
// one fragment per request). The session key is CLIENT-CONTROLLED: an agent
// that omits or rotates the header never engages the channel, and 256
// distinct ids flush the whole LRU — a bounded heuristic against cooperative
// clients, not a detection guarantee against deliberate evasion (decision 20).
//
// RED LINES: windows hold raw request bytes, which may CONTAIN CREDENTIALS —
// they live in process memory only and are never logged, serialized,
// persisted, or exposed via any API/event. The store is deliberately NOT part
// of RuntimeSnapshot: it is cross-generation runtime observation state (like
// metricsStore / the event hub), created once at construction and untouched
// by reload. It has its own small lock; no I/O ever happens under it, and
// automaton scans happen outside it (Add's verdict refresh) so one session's
// large body never stalls other sessions behind the mutex.
type sessionScanStore struct {
	mu    sync.Mutex
	ll    *list.List // front = most recently used; values are *sessionScanEntry
	items map[string]*list.Element
}

func newSessionScanStore() *sessionScanStore {
	return &sessionScanStore{ll: list.New(), items: map[string]*list.Element{}}
}

// Snapshot returns a copy of the session's retained window, its fragment
// progress for scanner sc, and the cached known-secret verdict for that exact
// window (knownOK=false when there is no valid cache entry — unknown session,
// scanner generation change, or a lost refresh race — in which case the
// caller rescans the tail itself). The session is marked recently used.
// Copies let the caller scan outside the lock.
func (s *sessionScanStore) Snapshot(id string, sc *guard.Scanner) (tail []byte, progress []int, known, knownOK bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[id]
	if !ok {
		return nil, nil, false, false
	}
	s.ll.MoveToFront(el)
	ent := el.Value.(*sessionScanEntry)
	tail = append([]byte(nil), ent.window...)
	if ent.scanner != sc {
		return tail, nil, false, false
	}
	return tail, append([]int(nil), ent.progress...), ent.known, ent.knownValid
}

// Add appends the PRE-REDACT body to the session's window (a redacted window
// would destroy the very fragments this pass exists to reassemble), records
// the fragment progress computed for this request, keeps only the last
// sessionScanWindowBytes, and evicts least-recently-used sessions beyond
// sessionScanMaxSessions. progress is merged element-wise max with any
// concurrent same-session state for the same scanner generation, EXCEPT the
// indices the scanner deliberately reset this request (reset[i]=true: a
// secret that completed across requests, or whose complete value appeared in
// one body): there the new state is an authoritative 0 — max-merging would
// resurrect stale progress and re-fire known_secret_fragmented from a later
// suffix fragment. A window truncation drops ALL fragment progress (the bytes
// backing it are gone — cross-window splits are a documented non-goal).
//
// bodyKnown is the caller's per-request verdict (the current body alone
// already hits the known-secret channel). Add also refreshes the window's
// cached known-secret verdict OUTSIDE the lock, so the automaton pass never
// stalls other sessions; a refresh that loses a same-session race simply
// leaves the cache invalid (callers rescan — detection is unaffected).
func (s *sessionScanStore) Add(id string, body []byte, sc *guard.Scanner, progress []int, reset []bool, bodyKnown bool) {
	if len(body) == 0 {
		return
	}
	// Bound the copy BEFORE taking the lock: the window keeps only the last
	// sessionScanWindowBytes, so earlier bytes of a larger body can never
	// survive truncation. Slicing here keeps the under-lock memcpy bounded by
	// the window size instead of max_request_body_bytes (64MiB default).
	if len(body) > sessionScanWindowBytes {
		body = body[len(body)-sessionScanWindowBytes:]
	}
	// span is the maximum number of bytes a known-secret occurrence can reach
	// across the old-window/body junction (no known-secret needle is longer
	// than span+1 — see Scanner.MaxKnownNeedleLen).
	span := sc.MaxKnownNeedleLen() - 1
	if span < 0 {
		span = 0
	}

	s.mu.Lock()
	el, ok := s.items[id]
	var ent *sessionScanEntry
	if !ok {
		// An empty window provably contains no known secret: the verdict
		// starts valid-and-clean.
		ent = &sessionScanEntry{id: id, knownValid: true}
		el = s.ll.PushFront(ent)
		s.items[id] = el
	} else {
		s.ll.MoveToFront(el)
		ent = el.Value.(*sessionScanEntry)
	}
	// Capture the incremental-verdict inputs BEFORE mutating the window: the
	// prior verdict (valid only for the same scanner generation) and the old
	// window's junction suffix, which a spanning occurrence could continue.
	oldKnown := ent.known
	oldValid := ent.knownValid && ent.scanner == sc
	var oldTail []byte
	if span > 0 && len(ent.window) > 0 {
		n := min(span, len(ent.window))
		oldTail = append([]byte(nil), ent.window[len(ent.window)-n:]...)
	}
	ent.window = append(ent.window, body...)
	truncated := false
	if len(ent.window) > sessionScanWindowBytes {
		ent.window = append([]byte(nil), ent.window[len(ent.window)-sessionScanWindowBytes:]...)
		truncated = true
	}
	switch {
	case truncated:
		ent.progress = nil
	case ent.scanner == sc:
		ent.progress = mergeProgress(ent.progress, progress, reset)
	default:
		ent.progress = append([]int(nil), progress...)
	}
	ent.scanner = sc
	// Invalidate the cached verdict; the refresh below republishes it.
	ent.known, ent.knownValid = false, false
	ent.seq++
	seq := ent.seq
	// Copy what the unlocked refresh reads (≤ window size, or ≤ 2×span).
	var window []byte
	var junction []byte
	if truncated || !oldValid {
		window = append([]byte(nil), ent.window...)
	} else if !oldKnown {
		junction = append(oldTail, body[:min(len(body), span)]...)
	}
	for s.ll.Len() > sessionScanMaxSessions {
		back := s.ll.Back()
		delete(s.items, back.Value.(*sessionScanEntry).id)
		s.ll.Remove(back)
	}
	s.mu.Unlock()

	// Refresh the verdict outside the lock. Cases:
	//  - truncation dropped bytes, or the prior verdict was unusable: rescan
	//    the whole new window;
	//  - the prior verdict was a hit and no bytes were dropped: still a hit;
	//  - otherwise the new window is old++body, so any hit lies inside body
	//    (bodyKnown, the caller's per-request verdict) or spans the junction
	//    — and a spanning occurrence sits entirely in the last span bytes of
	//    the old window plus the first span bytes of body.
	known := oldKnown
	if truncated || !oldValid {
		known = len(sc.ScanKnown(window)) > 0
	} else if !oldKnown {
		known = bodyKnown || len(sc.ScanKnown(junction)) > 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.items[id]; ok && cur == el && ent.seq == seq {
		ent.known, ent.knownValid = known, true
	}
}

// mergeProgress combines two per-secret progress states element-wise (max),
// tolerating either side being nil or shorter — except reset indices, where
// the new state is an authoritative 0 declared by the scanner (a completed
// secret must not keep firing from stale progress; see Add).
func mergeProgress(a, b []int, reset []bool) []int {
	if len(a) < len(b) {
		a, b = b, a
	}
	out := append([]int(nil), a...)
	for i, v := range b {
		if i < len(out) && v > out[i] {
			out[i] = v
		}
	}
	for i, r := range reset {
		if r && i < len(out) {
			out[i] = 0
		}
	}
	return out
}

// Len reports the number of tracked sessions (test/diagnostic seam).
func (s *sessionScanStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ll.Len()
}
