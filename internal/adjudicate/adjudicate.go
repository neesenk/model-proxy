// Package adjudicate owns the async AI second-opinion channel for guard
// pattern hits: instead of recording a rule-table/custom-pattern secret hit
// (or a strong sensitive-path hit) immediately, a designated model judges
// whether the matched content is a real leak (verdict high: recorded through
// the sink and may block the client session), an ambiguous risk (verdict
// medium: recorded, no block) or benign code/docs content (verdict low: the
// ignored tier — full JSONL trail only). Verdicts are cached by (kind, rule,
// model, hit-bytes) so conversation-history echo of one occurrence costs one
// model call, and both the verdict cache and the session block table persist
// across restarts.
//
// The package is deliberately pure: model calls go through the Caller port
// and observation side effects through the Sink port (both implemented by
// internal/app). Job.Hit carries matched secret bytes in memory only — it is
// never persisted, logged, or exposed via any DTO; persisted state stores
// hashes and verdicts exclusively.
package adjudicate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Verdict values. High/Medium/Low are model verdicts — high blocks the
// session, medium is record-only, low is the ignored tier (full JSONL trail,
// no queryable record). Error (call failed) and Skipped (queue full) are
// fail-open outcomes the sink records like a classic immediate hit.
const (
	VerdictHigh    = "high"
	VerdictMedium  = "medium"
	VerdictLow     = "low"
	VerdictError   = "error"
	VerdictSkipped = "skipped"
)

// Hit kinds, mirroring seclog kinds.
const (
	KindSecret = "secret"
	KindPath   = "path"
)

// maxReasonLen bounds the model-provided judgment logic before it reaches the
// sink; maxEvidenceLen bounds the factual basis the model cited. Both are
// short classification texts, never a channel for payload content (both are
// scrubbed: control characters stripped, hit bytes masked).
const (
	maxReasonLen   = 200
	maxEvidenceLen = 300
)

// Job is one guard hit awaiting adjudication. Hit is the matched bytes;
// Pre/Post are the masked context windows around it (other secret hits
// inside the window already masked by the caller).
type Job struct {
	Kind      string
	Rule      string // pattern type name or path category name
	Hit       string
	Pre       string
	Post      string
	RequestID string
	SessionID string
	Agent     string
	Proto     string
	Exposed   string
	Action    string // configured guard action at hit time
	Ts        int64
}

// Result is one completed (or failed) adjudication — ring visibility for the
// WebUI/CLI surfaces. Reason is the judgment logic and Evidence the factual
// basis the model cited; both are scrubbed (control chars stripped, hit
// bytes masked) before they enter a Result.
type Result struct {
	Ts        int64  `json:"ts"`
	Kind      string `json:"kind"`
	Rule      string `json:"rule"`
	Verdict   string `json:"verdict"`
	Reason    string `json:"reason,omitempty"`
	Evidence  string `json:"evidence,omitempty"`
	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Action    string `json:"action,omitempty"`
	Cached    bool   `json:"cached,omitempty"`
}

// Usage is the token accounting of one adjudication model call, as reported
// by the judging model's reply. Zero values are legitimate (a failed call,
// or a provider that returns no usage block).
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Caller performs one model adjudication. verdict must be VerdictHigh,
// VerdictMedium or VerdictLow; any error fails open to the classic immediate
// record. reason is the judgment logic and evidence the factual basis, both
// already parsed from the model reply. usage reports the call's token cost
// for the service's LLM-usage metrics (cache hits never reach a Caller and
// are not billed).
//
// CONTRACT: err must NOT contain the matched hit bytes, body excerpts, or
// any transform/fragment of the request content. Error details are persisted
// (scrubbed) but scrub is defense-in-depth; callers should report structural
// failures only (status, shape, length, parse errors).
type Caller interface {
	Adjudicate(ctx context.Context, model string, j Job) (verdict, reason, evidence string, usage Usage, err error)
}

// Sink receives the observation side effects, implemented by the app layer:
// High emits audit record + live event + counters (and is called AFTER the
// session block below is applied, so unblock surfaces already see it);
// Medium is the record-only tier; Low is the ignored tier (the app writes
// the JSONL-only trace). Every sink method fires ONCE per unique content —
// cached history-echo occurrences never re-emit, the ring entry is their
// per-occurrence visibility; Failed re-emits the classic immediate record
// with the fail-open verdict ("error"/"skipped").
type Sink interface {
	High(j Job, reason, evidence, model string)
	Medium(j Job, reason, evidence, model string)
	Low(j Job, reason, evidence, model string)
	Failed(j Job, verdict, detail string)
}

// RuntimeConfig is resolved per job from the CURRENT config generation: the
// designated model, per-call timeout, and whether high verdicts block the
// session. enabled reports whether the channel is still on (a reload may
// have turned it off mid-flight; the job is then dropped silently).
type RuntimeConfig interface {
	AdjudicationConfig() (model string, timeout time.Duration, blockSession, enabled bool)
}

// Options sizes the service. Zero values select the documented defaults
// (they mirror guard.adjudicate; the app passes the loaded config through).
type Options struct {
	Workers  int
	MaxQueue int
	CacheMax int
	RingSize int
	// StateDir locates the persisted state files
	// (<dir>/guard_verdicts.json, guard_blocks.json, guard_stats.json);
	// empty keeps state in memory (isolated tests).
	StateDir string
	// Now overrides the clock in tests.
	Now func() time.Time
}

// Service is the process-lifetime adjudication engine: bounded job queue,
// workers, in-flight dedup, verdict cache, session blocks, result ring.
// Construct once, Start with the ports, Close drains.
type Service struct {
	opts   Options
	queue  chan Job
	cfg    RuntimeConfig
	caller Caller
	sink   Sink

	cache  *verdictCache
	blocks *blockStore
	ring   *resultRing

	mu       sync.Mutex
	inflight map[string]bool
	closed   bool
	// applyWg tracks the cached-verdict applies Enqueue spawns off the worker
	// pool, so Close does not return before every accepted apply's sink/audit
	// side effects landed (the app drains seclog only after Close returns).
	// Adds happen under mu, so they are all visible once closed is set.
	applyWg sync.WaitGroup

	// llm usage accounting: real model calls only (cache hits and in-flight
	// dedup never reach a Caller). Guarded by statsMu so the metrics read
	// never contends with the queue hot path. The counters are persisted
	// (guard_stats.json) so the usage surface survives restarts. Disk writes
	// are serialized by statsFlushMu and happen outside statsMu.
	statsMu      sync.Mutex
	statsFlushMu sync.Mutex
	statsPath    string
	statsCalls   int64
	statsInTok   int64
	statsOutTok  int64
	statsLows    int64
	// last counters successfully persisted; guarded by statsFlushMu so writes
	// are monotonic and a stale snapshot cannot regress the on-disk low count.
	statsLastCalls int64
	statsLastIn    int64
	statsLastOut   int64
	statsLastLows  int64

	// blockedContent is the repeat-interception index of HIGH-verdict hit
	// bytes (sha256-keyed, persisted hash-only in guard_blocked.json).
	blockedContent *blockedContentStore

	// allowedContent is the operator-override table: hit bytes whose risk an
	// operator has finally judged acceptable (via session Unblock's cascade).
	// Both hit gates (repeat interception, fresh adjudication) skip allowed
	// bytes — the judge never re-litigates an operator decision.
	allowedContent *allowedContentStore

	stop chan struct{}
	done chan struct{}
}

// New builds the service and loads persisted state (best-effort: a missing
// or corrupt file starts empty — persisted state is a cache, never a
// correctness dependency).
func New(opts Options) *Service {
	if opts.Workers <= 0 {
		opts.Workers = 2
	}
	if opts.MaxQueue <= 0 {
		opts.MaxQueue = 256
	}
	if opts.CacheMax <= 0 {
		opts.CacheMax = 4096
	}
	if opts.RingSize <= 0 {
		opts.RingSize = 256
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Service{
		opts:     opts,
		queue:    make(chan Job, opts.MaxQueue),
		inflight: make(map[string]bool),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	s.cache = loadVerdictCache(statePath(opts.StateDir, "guard_verdicts.json"), opts.CacheMax)
	s.blocks = loadBlockStore(statePath(opts.StateDir, "guard_blocks.json"))
	s.blockedContent = loadBlockedContentStore(statePath(opts.StateDir, "guard_blocked.json"), opts.CacheMax)
	s.allowedContent = loadAllowedContentStore(statePath(opts.StateDir, "guard_allowed.json"), opts.CacheMax)
	s.statsPath = statePath(opts.StateDir, "guard_stats.json")
	s.statsCalls, s.statsInTok, s.statsOutTok, s.statsLows = loadUsageStats(s.statsPath)
	s.ring = newResultRing(opts.RingSize)
	return s
}

// Start launches the workers. Exactly once; the returned service is not
// usable before it.
func (s *Service) Start(cfg RuntimeConfig, caller Caller, sink Sink) {
	s.cfg, s.caller, s.sink = cfg, caller, sink
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < s.opts.Workers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.work()
			}()
		}
		wg.Wait()
		close(s.done)
	}()
}

// work consumes jobs until stop. After stop it keeps draining the queue
// until empty (bounded by the drain deadline in Close) so shutdown does not
// silently drop already-accepted work.
func (s *Service) work() {
	for {
		select {
		case <-s.stop:
			for {
				select {
				case j := <-s.queue:
					s.process(j)
				default:
					return
				}
			}
		case j := <-s.queue:
			s.process(j)
		}
	}
}

// Enqueue offers one job. It returns false when the queue is full or the
// service is closed/closing — the caller then fails open (immediate classic
// record, verdict "skipped"). Jobs whose content is already being adjudicated
// are dropped silently (the in-flight job produces the verdict), returning
// true.
func (s *Service) Enqueue(j Job) bool {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return false
	}
	// Operator override: allowed bytes are neither re-intercepted nor
	// re-judged — reporting acceptance (not queue overflow) keeps the
	// forward path from failing open to the classic immediate record.
	if _, ok := s.allowedContent.Allowed(j.Hit); ok {
		s.mu.Unlock()
		return true
	}
	model, _, blockSession, enabled := s.cfg.AdjudicationConfig()
	if !enabled {
		s.mu.Unlock()
		return true // channel turned off under us: suppress, no fail-open noise
	}
	key := CacheKey(j, model)
	if s.inflight[key] {
		s.mu.Unlock()
		return true
	}
	if v, ok := s.cache.get(key); ok {
		// Cached verdict: apply asynchronously through the same path a worker
		// would (no queue latency for the common history-echo case). The Add
		// runs under mu so Close — which sets closed under the same mu before
		// waiting on applyWg — can never pass its wait ahead of this apply.
		s.applyWg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.applyWg.Done()
			s.apply(j, v.Verdict, v.Reason, v.Evidence, model, blockSession, true)
		}()
		return true
	}
	s.inflight[key] = true
	// Deliver under mu: the send is non-blocking (no deadlock), and keeping it
	// inside the critical section closes the accept-after-drain window — once
	// Close sets closed under the same mu, no accepted job can land in the
	// queue behind the workers' drain check.
	select {
	case s.queue <- j:
		s.mu.Unlock()
		return true
	default:
		delete(s.inflight, key)
		s.mu.Unlock()
		return false
	}
}

// process runs on a worker for one queued job.
func (s *Service) process(j Job) {
	model, timeout, blockSession, enabled := s.cfg.AdjudicationConfig()
	key := CacheKey(j, model)
	defer func() {
		s.mu.Lock()
		delete(s.inflight, key)
		s.mu.Unlock()
	}()
	if !enabled {
		return // reload turned the channel off: drop
	}
	if v, ok := s.cache.get(key); ok {
		s.apply(j, v.Verdict, v.Reason, v.Evidence, model, blockSession, true)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	verdict, reason, evidence, usage, err := s.caller.Adjudicate(ctx, model, j)
	cancel()
	s.statsMu.Lock()
	s.statsCalls++
	s.statsInTok += usage.InputTokens
	s.statsOutTok += usage.OutputTokens
	s.statsMu.Unlock()
	// Persist outside statsMu: fsync must not block other workers or readers.
	// statsFlushMu serializes writers so the on-disk counters never regress.
	// The write is monotonic (skip if no counter increased) so a process write
	// that races past a low-verdict apply cannot overwrite a higher low count
	// with the stale value it read before apply ran.
	s.flushStatsLocked()
	if err != nil || (verdict != VerdictHigh && verdict != VerdictMedium && verdict != VerdictLow) {
		// err may be nil here (a nil error with an off-vocabulary verdict) —
		// dereferencing it would panic the worker and take the daemon down.
		detail := "model returned unrecognized verdict " + verdict
		if err != nil {
			detail = err.Error()
		}
		s.ring.add(Result{Ts: j.Ts, Kind: j.Kind, Rule: j.Rule, Verdict: VerdictError,
			Reason: truncate(scrub(j.Hit, detail), maxReasonLen), Model: model,
			RequestID: j.RequestID, SessionID: j.SessionID, Action: j.Action})
		s.sink.Failed(j, VerdictError, truncate(scrub(j.Hit, detail), maxReasonLen))
		return
	}
	reason = truncate(scrub(j.Hit, reason), maxReasonLen)
	evidence = truncate(scrub(j.Hit, evidence), maxEvidenceLen)
	s.cache.put(key, verdictEntry{Verdict: verdict, Reason: reason, Evidence: evidence, Model: model, Ts: s.opts.Now().UnixMilli()})
	s.apply(j, verdict, reason, evidence, model, blockSession, false)
}

// apply fans one verdict out to the ring, the sink, and (high + blockSession
// + session present) the block table. blockSession is resolved by the caller
// from the current config generation.
func (s *Service) apply(j Job, verdict, reason, evidence, model string, blockSession bool, cached bool) {
	res := Result{Ts: s.opts.Now().UnixMilli(), Kind: j.Kind, Rule: j.Rule, Verdict: verdict,
		Reason: reason, Evidence: evidence, Model: model, RequestID: j.RequestID, SessionID: j.SessionID,
		Action: j.Action, Cached: cached}
	s.ring.add(res)
	switch verdict {
	case VerdictHigh:
		// The block refreshes on every cached occurrence (a later session
		// carrying the same leaked content gets blocked even though the
		// verdict came from the cache), but the AUDIT record/event/counters
		// fire once per unique content: re-emitting them on every
		// history-echo occurrence would rebuild exactly the audit flood this
		// channel exists to suppress — the ring entry below is the
		// per-occurrence visibility.
		//
		// Record the repeat-interception index BEFORE the session block so
		// observers that poll the block table never see a block whose
		// associated content hash has not yet been recorded (the block flush
		// can yield while the worker is still in this function).
		if j.Kind == KindSecret {
			s.blockedContent.Record(j.Hit, BlockedContent{
				Kind: j.Kind, Rule: j.Rule, Reason: reason, Evidence: evidence,
				Model: model, Ts: res.Ts,
			})
		}
		if blockSession && j.SessionID != "" {
			bl := Block{
				Kind: j.Kind, Rule: j.Rule, Rules: []string{j.Rule},
				Reason: reason, Model: model,
				RequestID: j.RequestID, Ts: res.Ts,
			}
			if j.Kind == KindSecret {
				// Record the verdict's enforcement artifacts so a later
				// operator Unblock can cascade to the content itself.
				bl.ContentHashes = []string{hashHit(j.Hit)}
				bl.CacheKeys = []string{CacheKey(j, model)}
			}
			s.blocks.Block(j.SessionID, bl)
		}
		if !cached {
			s.sink.High(j, reason, evidence, model)
		}
	case VerdictMedium:
		if !cached {
			s.sink.Medium(j, reason, evidence, model)
		}
	case VerdictLow:
		// The cumulative suppressed count covers EVERY occurrence (cached
		// echoes too): it is the suppression-rate numerator, not a per-content
		// record — rows stay ring-only by design.
		s.statsMu.Lock()
		s.statsLows++
		s.statsMu.Unlock()
		s.flushStatsLocked()
		if !cached {
			s.sink.Low(j, reason, evidence, model)
		}
	}
}

// Close stops intake and waits for the workers to drain the queue or the
// deadline, whichever comes first, then waits for the cached-verdict applies
// Enqueue spawned (applyWg). Jobs still queued past the deadline are
// dropped (their Enqueue already returned true; the caller-side fail-open
// contract covers only Enqueue=false — dropped-at-shutdown jobs are the
// documented shutdown-loss window, same direction as seclog's drain).
func (s *Service) Close(drain time.Duration) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-s.done
		s.applyWg.Wait()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.stop)
	select {
	case <-s.done:
	case <-time.After(drain):
	}
	// The cached-verdict applies Enqueue spawned run outside the worker
	// WaitGroup; Close returns only after every accepted one completed, so a
	// late apply can never lose its audit record to the seclog drain that
	// follows in the app's shutdown order.
	s.applyWg.Wait()
}

// CacheKey derives the stable verdict-cache key for one job: the hash covers
// kind, rule, model and the matched bytes — never the bytes themselves.
func CacheKey(j Job, model string) string {
	h := sha256.New()
	h.Write([]byte(j.Kind))
	h.Write([]byte{0})
	h.Write([]byte(j.Rule))
	h.Write([]byte{0})
	h.Write([]byte(model))
	h.Write([]byte{0})
	h.Write([]byte(j.Hit))
	return hex.EncodeToString(h.Sum(nil))
}

// Blocked reports whether a session is blocked (set by a high verdict).
func (s *Service) Blocked(sessionID string) (Block, bool) { return s.blocks.Blocked(sessionID) }

// Block adds one session block directly (test/admin surface; the high-verdict
// path applies it automatically).
func (s *Service) Block(sessionID string, b Block) { s.blocks.Block(sessionID, b) }

// Unblock removes one session block and returns the removed entry (for the
// unblock audit trail); false when not blocked.
// Unblock removes one session block and cascades the operator's risk
// judgment to the content behind it: every recorded repeat-interception
// entry moves into the operator-override table (same bytes are never
// re-intercepted and never re-judged), and the verdict-cache entries drop so
// no cached high can resurrect them. It returns the removed entry for the
// unblock audit record; false when the session was not blocked.
func (s *Service) Unblock(sessionID string) (Block, bool) {
	bl, ok := s.blocks.Unblock(sessionID)
	if !ok {
		return Block{}, false
	}
	if len(bl.ContentHashes) == 0 && len(bl.CacheKeys) == 0 {
		return bl, true // pre-cascade entry (or hash-less channel): session-only release
	}
	now := s.opts.Now().UnixMilli()
	for _, h := range bl.ContentHashes {
		bc, hadEntry := s.blockedContent.Entry(h)
		s.blockedContent.Remove(h)
		ac := AllowedContent{Kind: bl.Kind, Rule: bl.Rule, Reason: bl.Reason, Source: "session-unblock", Ts: now}
		if hadEntry {
			ac.Kind, ac.Rule, ac.Reason = bc.Kind, bc.Rule, bc.Reason
		}
		s.allowedContent.AllowHash(h, ac)
	}
	s.cache.remove(bl.CacheKeys...)
	return bl, true
}

// BlockWithHit blocks a session for a REPEAT interception: the hit bytes are
// known, so the block entry records the content hash (and the cache key,
// attributed from the original index entry) — keeping the Unblock cascade
// intact no matter which path produced the block.
func (s *Service) BlockWithHit(sessionID, rule, requestID, reason, hit string) {
	hash := hashHit(hit)
	bc, _ := s.blockedContent.Entry(hash)
	bl := Block{
		Kind: bc.Kind, Rule: rule, Rules: []string{rule},
		Reason: reason, Model: bc.Model,
		RequestID: requestID, Ts: s.opts.Now().UnixMilli(),
		ContentHashes: []string{hash},
	}
	if bc.Kind != "" && bc.Model != "" {
		bl.CacheKeys = []string{CacheKey(Job{Kind: bc.Kind, Rule: bc.Rule, Hit: hit}, bc.Model)}
	}
	s.blocks.Block(sessionID, bl)
}

// AllowedSnapshot lists the operator overrides newest first (audit surface).
func (s *Service) AllowedSnapshot() []AllowedEntry { return s.allowedContent.Snapshot() }

// Disallow revokes one operator override. Enforcement falls back to fresh
// adjudication on the content's next occurrence (the repeat index entry was
// already consumed by the cascade); false when the hash was not allowed.
func (s *Service) Disallow(hash string) bool { return s.allowedContent.Remove(hash) }

// Blocks snapshots the block table (with session ids), newest first.
func (s *Service) Blocks() []BlockEntry { return s.blocks.Snapshot() }

// Recent snapshots the result ring, newest first.
func (s *Service) Recent() []Result { return s.ring.Snapshot() }

// ContentBlocked reports whether these raw hit bytes were already adjudicated
// high (secret-kind hits only — see apply). The forward path uses it to
// intercept repeats verbatim. Operator-allowed bytes are never blocked.
func (s *Service) ContentBlocked(hit string) (BlockedContent, bool) {
	if _, ok := s.allowedContent.Allowed(hit); ok {
		return BlockedContent{}, false
	}
	return s.blockedContent.Blocked(hit)
}

// Stats reports the LLM adjudication usage and the cumulative suppressed-low
// count: real model calls and their
// token totals (input+output). Cache hits are deliberately absent — the
// point of the cache is that they cost nothing.
func (s *Service) Stats() (calls, inputTokens, outputTokens, lowVerdicts int64) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.statsCalls, s.statsInTok, s.statsOutTok, s.statsLows
}

// flushStatsLocked persists the current stats counters if they have advanced
// beyond the last written snapshot. Caller must NOT hold statsMu.
func (s *Service) flushStatsLocked() {
	s.statsFlushMu.Lock()
	defer s.statsFlushMu.Unlock()
	calls, inTok, outTok, lows := s.Stats()
	if calls <= s.statsLastCalls && inTok <= s.statsLastIn && outTok <= s.statsLastOut && lows <= s.statsLastLows {
		return
	}
	if s.statsPath != "" {
		writeUsageStats(s.statsPath, calls, inTok, outTok, lows)
	}
	s.statsLastCalls, s.statsLastIn, s.statsLastOut, s.statsLastLows = calls, inTok, outTok, lows
}

// CacheLen reports the verdict-cache size (diagnostics/tests).
func (s *Service) CacheLen() int { return s.cache.Len() }

// scrub makes a model-provided (or error) string safe for persistence:
// control characters collapse to spaces and any occurrence of the matched
// hit (or a long-enough fragment of it) is replaced, so a reason echoing
// the payload can never carry it into logs. Masking is maskSecretBytes-
// aligned: short hits (<8 bytes, e.g. from a short custom pattern) are
// masked wholesale too — a short hit must never land on disk verbatim.
//
// Callers (the Caller implementation) must still never put request content
// into error details; the fragment mask here is defense-in-depth against
// future callers that might quote a portion of the matched bytes.
func scrub(hit, s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if hit == "" {
		return out
	}
	// Full hit first.
	out = strings.ReplaceAll(out, hit, "[MASKED]")
	// Defense in depth: mask any substring of the hit that is long enough
	// to be a recognizable fragment (>= 8 bytes). Shorter substrings are
	// either the full short hit (handled above) or too generic to attribute.
	if len(hit) >= 8 {
		const minFragment = 8
		seen := make(map[string]bool)
		for i := 0; i < len(hit); i++ {
			for j := i + minFragment; j <= len(hit); j++ {
				frag := hit[i:j]
				if seen[frag] {
					continue
				}
				seen[frag] = true
				out = strings.ReplaceAll(out, frag, "[MASKED]")
			}
		}
	}
	return out
}

// truncate caps s at n runes.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
