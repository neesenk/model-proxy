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
}

// sessionScanStore is a bounded LRU mapping x-claude-code-session-id → the
// tail of that session's recent request bodies + fragment progress, for
// split-exfiltration detection (a known credential smuggled out in pieces,
// one fragment per request).
//
// RED LINES: windows hold raw request bytes, which may CONTAIN CREDENTIALS —
// they live in process memory only and are never logged, serialized,
// persisted, or exposed via any API/event. The store is deliberately NOT part
// of RuntimeSnapshot: it is cross-generation runtime observation state (like
// metricsStore / the event hub), created once at construction and untouched
// by reload. It has its own small lock; no I/O ever happens under it.
type sessionScanStore struct {
	mu    sync.Mutex
	ll    *list.List // front = most recently used; values are *sessionScanEntry
	items map[string]*list.Element
}

func newSessionScanStore() *sessionScanStore {
	return &sessionScanStore{ll: list.New(), items: map[string]*list.Element{}}
}

// Snapshot returns a copy of the session's retained window and its fragment
// progress for scanner sc (nil progress when the session is unknown or the
// scanner generation changed — fragment state is generation-scoped because
// progress indexes the scanner's secret list). The session is marked
// recently used. Copies let the caller scan outside the lock.
func (s *sessionScanStore) Snapshot(id string, sc *guard.Scanner) (tail []byte, progress []int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[id]
	if !ok {
		return nil, nil
	}
	s.ll.MoveToFront(el)
	ent := el.Value.(*sessionScanEntry)
	tail = append([]byte(nil), ent.window...)
	if ent.scanner != sc {
		return tail, nil
	}
	return tail, append([]int(nil), ent.progress...)
}

// Add appends the PRE-REDACT body to the session's window (a redacted window
// would destroy the very fragments this pass exists to reassemble), records
// the fragment progress computed for this request, keeps only the last
// sessionScanWindowBytes, and evicts least-recently-used sessions beyond
// sessionScanMaxSessions. progress is merged element-wise max with any
// concurrent same-session state for the same scanner generation; a window
// truncation drops ALL fragment progress (the bytes backing it are gone —
// cross-window splits are a documented non-goal).
func (s *sessionScanStore) Add(id string, body []byte, sc *guard.Scanner, progress []int) {
	if len(body) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[id]
	var ent *sessionScanEntry
	if !ok {
		ent = &sessionScanEntry{id: id}
		el = s.ll.PushFront(ent)
		s.items[id] = el
	} else {
		s.ll.MoveToFront(el)
		ent = el.Value.(*sessionScanEntry)
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
		ent.progress = mergeProgress(ent.progress, progress)
	default:
		ent.progress = append([]int(nil), progress...)
	}
	ent.scanner = sc
	for s.ll.Len() > sessionScanMaxSessions {
		back := s.ll.Back()
		delete(s.items, back.Value.(*sessionScanEntry).id)
		s.ll.Remove(back)
	}
}

// mergeProgress combines two per-secret progress states element-wise (max),
// tolerating either side being nil or shorter.
func mergeProgress(a, b []int) []int {
	if len(a) < len(b) {
		a, b = b, a
	}
	out := append([]int(nil), a...)
	for i, v := range b {
		if i < len(out) && v > out[i] {
			out[i] = v
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
