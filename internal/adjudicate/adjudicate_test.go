package adjudicate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCaller records calls and answers from a scripted verdict/error.
type fakeCaller struct {
	mu     sync.Mutex
	calls  []Job
	answer func(j Job) (string, string, string, error)
}

func (f *fakeCaller) setAnswer(fn func(Job) (string, string, string, error)) {
	f.mu.Lock()
	f.answer = fn
	f.mu.Unlock()
}

func (f *fakeCaller) Adjudicate(_ context.Context, _ string, j Job) (string, string, string, Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, j)
	v, r, e, err := f.answer(j)
	return v, r, e, Usage{InputTokens: 100, OutputTokens: 20}, err
}

func (f *fakeCaller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeSink records sink side effects in order.
type fakeSink struct {
	mu           sync.Mutex
	high         []Job
	medium       []Job
	low          []Job
	fails        []Job
	mediumCached int
	lowCached    int
	lastEvidence string
}

func (f *fakeSink) High(j Job, _, _, _ string) {
	f.mu.Lock()
	f.high = append(f.high, j)
	f.mu.Unlock()
}
func (f *fakeSink) Medium(j Job, _, evidence, _ string) {
	f.mu.Lock()
	f.medium = append(f.medium, j)
	f.lastEvidence = evidence
	f.mu.Unlock()
}
func (f *fakeSink) Low(j Job, _, evidence, _ string) {
	f.mu.Lock()
	f.low = append(f.low, j)
	f.lastEvidence = evidence
	f.mu.Unlock()
}
func (f *fakeSink) Failed(j Job, _, _ string) {
	f.mu.Lock()
	f.fails = append(f.fails, j)
	f.mu.Unlock()
}

func (f *fakeSink) counts() (high, low, fails int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.high), len(f.low), len(f.fails)
}

// staticConfig is a fixed RuntimeConfig for tests.
type staticConfig struct {
	model        string
	timeout      time.Duration
	blockSession bool
	enabled      bool
}

func (c staticConfig) AdjudicationConfig() (string, time.Duration, bool, bool) {
	return c.model, c.timeout, c.blockSession, c.enabled
}

func testJob(hit string) Job {
	return Job{
		Kind: KindSecret, Rule: "openai_api_key", Hit: hit,
		RequestID: "req-1", SessionID: "sess-1", Agent: "pi",
		Proto: "anthropic", Exposed: "glm-5.3", Action: "log",
		Ts: 1700000000000,
	}
}

func newTestService(t *testing.T, opts Options) (*Service, *fakeCaller, *fakeSink) {
	t.Helper()
	opts.StateDir = t.TempDir()
	s := New(opts)
	caller := &fakeCaller{}
	sink := &fakeSink{}
	s.Start(staticConfig{model: "judge", timeout: 5 * time.Second, blockSession: true, enabled: true}, caller, sink)
	t.Cleanup(func() { s.Close(time.Second) })
	return s, caller, sink
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached within deadline")
}

func TestHighVerdict_RecordsAndBlocksSession(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) { return VerdictHigh, "real key shape", "", nil })

	if !s.Enqueue(testJob("sk-proj-abcdefghij1234567890")) {
		t.Fatal("Enqueue refused a fresh job")
	}
	waitFor(t, func() bool { h, _, _ := sink.counts(); return h == 1 })
	if len(caller.calls) != 1 {
		t.Fatalf("caller calls = %d, want 1", len(caller.calls))
	}
	// Session blocked with the verdict attribution.
	bl, ok := s.Blocked("sess-1")
	if !ok {
		t.Fatal("session not blocked after high verdict")
	}
	if bl.Rule != "openai_api_key" || bl.RequestID != "req-1" {
		t.Errorf("block = %+v, want rule=openai_api_key request=req-1", bl)
	}
	// Ring carries the high verdict.
	recent := s.Recent()
	if len(recent) != 1 || recent[0].Verdict != VerdictHigh || recent[0].Rule != "openai_api_key" {
		t.Errorf("recent = %+v, want one high openai_api_key entry", recent)
	}
	if _, l, f := sink.counts(); l != 0 || f != 0 {
		t.Errorf("low = %d fails = %d, want none", l, f)
	}
}

func TestLowVerdict_SuppressesAndNeverBlocks(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) {
		return VerdictLow, "fixture text", "plain path mention", nil
	})

	s.Enqueue(testJob("sk-capture-dummy-not-a-real-key"))
	waitFor(t, func() bool { _, l, _ := sink.counts(); return l == 1 })
	if _, ok := s.Blocked("sess-1"); ok {
		t.Error("low verdict must never block the session")
	}
	if h, _, f := sink.counts(); h != 0 || f != 0 {
		t.Errorf("high = %d fails = %d, want none", h, f)
	}
	// The ring carries the evidence the model cited.
	recent := s.Recent()
	if len(recent) != 1 || recent[0].Evidence != "plain path mention" {
		t.Errorf("recent = %+v, want the low entry with its evidence", recent)
	}
}

// Medium is the record-only tier: the sink sees it, the session is never
// blocked, and cached occurrences do not re-emit the record.
func TestMediumVerdict_RecordsWithoutBlocking(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) {
		return VerdictMedium, "tool call touches key path", "bash argument references ~/.ssh/id_rsa", nil
	})

	s.Enqueue(testJob("sk-medium-ambiguous-aaaaaaaaaaaa"))
	waitFor(t, func() bool {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		return len(sink.medium) == 1
	})
	if _, ok := s.Blocked("sess-1"); ok {
		t.Error("medium verdict must never block the session")
	}
	if h, l, f := sink.counts(); h != 0 || l != 0 || f != 0 {
		t.Errorf("high = %d low = %d fails = %d, want none", h, l, f)
	}
	sink.mu.Lock()
	evidence := sink.lastEvidence
	sink.mu.Unlock()
	if evidence != "bash argument references ~/.ssh/id_rsa" {
		t.Errorf("medium evidence = %q, want the model-cited basis", evidence)
	}

	// History echo of the same content replays the cached verdict: the sink
	// is not called again at all, still no block.
	s.Enqueue(testJob("sk-medium-ambiguous-aaaaaaaaaaaa"))
	time.Sleep(50 * time.Millisecond)
	sink.mu.Lock()
	mediumCalls := len(sink.medium)
	sink.mu.Unlock()
	if mediumCalls != 1 {
		t.Errorf("cached medium re-emitted the record: %d Medium calls, want 1", mediumCalls)
	}
	if _, ok := s.Blocked("sess-1"); ok {
		t.Error("cached medium must not block the session")
	}
}

func TestCallerError_FailsOpenThroughSink(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) { return "", "", "", errors.New("dial timeout") })

	s.Enqueue(testJob("sk-proj-abcdefghij1234567890"))
	waitFor(t, func() bool { _, _, f := sink.counts(); return f == 1 })
	if _, ok := s.Blocked("sess-1"); ok {
		t.Error("an errored adjudication must not block the session")
	}
	recent := s.Recent()
	if len(recent) != 1 || recent[0].Verdict != VerdictError {
		t.Errorf("recent = %+v, want one error entry", recent)
	}
	// Errors are not cached: the same content re-adjudicates.
	s.Enqueue(testJob("sk-proj-abcdefghij1234567890"))
	waitFor(t, func() bool { return caller.count() == 2 })
}

func TestVerdictCache_DedupesHistoryEcho(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) { return VerdictLow, "fixture", "", nil })

	for i := 0; i < 5; i++ {
		j := testJob("sk-capture-dummy-not-a-real-key")
		j.RequestID = strings.Repeat("x", i) // distinct requests, same content
		s.Enqueue(j)
	}
	waitFor(t, func() bool { return caller.count() == 1 })
	waitFor(t, func() bool { _, l, _ := sink.counts(); return l >= 1 })

	// Cached HIGH verdicts refresh the block but do not re-emit the sink
	// record (once per unique content).
	highSink := &fakeSink{}
	highCaller := &fakeCaller{}
	s3 := New(Options{StateDir: t.TempDir()})
	s3.Start(staticConfig{model: "judge", timeout: time.Second, blockSession: true, enabled: true}, highCaller, highSink)
	defer s3.Close(time.Second)
	highCaller.setAnswer(func(Job) (string, string, string, error) { return VerdictHigh, "real", "", nil })
	s3.Enqueue(testJob("sk-realkey-abcdefghij12345678"))
	waitFor(t, func() bool { h, _, _ := highSink.counts(); return h == 1 })
	j2 := testJob("sk-realkey-abcdefghij1234567890123")
	j2.Hit = "sk-realkey-abcdefghij12345678"
	j2.SessionID = "sess-2"
	s3.Enqueue(j2)
	waitFor(t, func() bool { _, ok := s3.Blocked("sess-2"); return ok })
	if h, _, _ := highSink.counts(); h != 1 {
		t.Errorf("cached high re-emitted the record: %d High calls, want 1", h)
	}

	// A persisted cache survives a rebuild of the service over the same home.
	dir := s.opts.StateDir
	s.Close(time.Second)
	s2 := New(Options{StateDir: dir})
	caller2 := &fakeCaller{answer: func(Job) (string, string, string, error) {
		return VerdictHigh, "", "", nil // would block if actually called
	}}
	sink2 := &fakeSink{}
	s2.Start(staticConfig{model: "judge", timeout: time.Second, blockSession: true, enabled: true}, caller2, sink2)
	defer s2.Close(time.Second)
	if !s2.Enqueue(testJob("sk-capture-dummy-not-a-real-key")) {
		t.Fatal("Enqueue refused")
	}
	// The cached LOW verdict consumes the job without a model call and
	// without a sink re-emit (once per unique content); give the async
	// apply a beat to prove nothing fires either.
	time.Sleep(50 * time.Millisecond)
	if caller2.count() != 0 {
		t.Errorf("caller called %d times on a cached verdict, want 0", caller2.count())
	}
	if h, l, f := sink2.counts(); h != 0 || l != 0 || f != 0 {
		t.Errorf("cached low re-emitted: high = %d low = %d fails = %d, want 0/0/0", h, l, f)
	}
	// The replay is visible in the ring as a cached entry.
	recent := s2.Recent()
	if len(recent) != 1 || recent[0].Verdict != VerdictLow || !recent[0].Cached {
		t.Errorf("recent = %+v, want one cached low entry", recent)
	}
}

func TestBlockPersistence_AndUnblock(t *testing.T) {
	s, _, _ := newTestService(t, Options{})
	s.Block("sess-9", Block{Kind: KindSecret, Rule: "jwt", RequestID: "r9", Ts: 42})

	dir := s.opts.StateDir
	s.Close(time.Second)
	s2 := New(Options{StateDir: dir})
	if _, ok := s2.Blocked("sess-9"); !ok {
		t.Fatal("block did not survive the restart")
	}
	if !s2.Unblock("sess-9") {
		t.Fatal("Unblock reported not-blocked")
	}
	if _, ok := s2.Blocked("sess-9"); ok {
		t.Fatal("session still blocked after Unblock")
	}
	if s2.Unblock("sess-9") {
		t.Error("second Unblock must report false")
	}
	// The unblocked state is persisted too.
	s2.Close(time.Second)
	s3 := New(Options{StateDir: dir})
	if _, ok := s3.Blocked("sess-9"); ok {
		t.Fatal("unblock did not persist")
	}
}

func TestQueueFull_FailsOpen(t *testing.T) {
	// Workers: 1, queue: 1. Block the single worker inside the caller so the
	// queue fills, then prove Enqueue reports false (fail-open contract).
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	s := New(Options{Workers: 1, MaxQueue: 1, StateDir: t.TempDir()})
	caller := &fakeCaller{answer: func(Job) (string, string, string, error) {
		once.Do(func() { close(started) })
		<-release
		return VerdictLow, "", "", nil
	}}
	sink := &fakeSink{}
	s.Start(staticConfig{model: "m", timeout: time.Second, enabled: true}, caller, sink)
	t.Cleanup(func() { close(release); s.Close(time.Second) })

	s.Enqueue(testJob("sk-firstpayload-aaaaaaaaaaaa"))
	<-started // worker busy now
	if !s.Enqueue(testJob("sk-secondpayload-bbbbbbbbbbbb")) {
		t.Fatal("first queued job must be accepted (queue cap 1)")
	}
	if s.Enqueue(testJob("sk-thirdpayload-cccccccccccc")) {
		t.Fatal("queue overflow must return false so the caller fail-opens")
	}
}

func TestScrub_MasksHitEchoAndControlChars(t *testing.T) {
	got := scrub("sk-proj-abcdefghij1234567890", "echoes sk-proj-abcdefghij1234567890 and \x1b[31mcolor\n")
	if strings.Contains(got, "sk-proj-abcdefghij1234567890") {
		t.Errorf("scrub leaked the hit: %q", got)
	}
	if !strings.Contains(got, "[MASKED]") {
		t.Errorf("scrub did not mask the hit: %q", got)
	}
	if strings.ContainsAny(got, "\x1b\n") {
		t.Errorf("scrub left control characters: %q", got)
	}
	// Short hits (<8 bytes) are not masked (maskSecretBytes parity).
	if got := scrub("abc", "abc"); got != "abc" {
		t.Errorf("short hit must not be masked: %q", got)
	}
}

func TestDisabledConfig_DropsSilently(t *testing.T) {
	s := New(Options{StateDir: t.TempDir()})
	caller := &fakeCaller{answer: func(Job) (string, string, string, error) {
		return VerdictHigh, "", "", nil
	}}
	sink := &fakeSink{}
	s.Start(staticConfig{enabled: false}, caller, sink)
	t.Cleanup(func() { s.Close(time.Second) })
	if !s.Enqueue(testJob("sk-proj-abcdefghij1234567890")) {
		t.Fatal("disabled Enqueue must consume (true), not fail-open")
	}
	waitFor(t, func() bool { h, _, _ := sink.counts(); return caller.count() == 0 && h == 0 })
	time.Sleep(20 * time.Millisecond) // nothing asynchronous may fire either
	if h, _, _ := sink.counts(); caller.count() != 0 || h != 0 {
		t.Errorf("disabled channel must drop jobs silently")
	}
}

// TestLLMUsageStats_CountsCallsNotCacheHits: the usage accounting bills
// real model calls only — cached verdicts and in-flight dedup are free.
func TestLLMUsageStats_CountsCallsNotCacheHits(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, string, error) { return VerdictLow, "fixture", "", nil })
	for i := 0; i < 4; i++ {
		s.Enqueue(testJob("sk-usage-dummy-not-a-real-key-000" + string(rune('a'+i))))
	}
	waitFor(t, func() bool { _, low, _ := sink.counts(); return low >= 1 })
	waitFor(t, func() bool { calls, _, _, _ := s.Stats(); return calls >= 1 })
	// Cache hit on identical content adds no call and no tokens.
	j := testJob("sk-usage-dummy-not-a-real-key-000a")
	s.Enqueue(j)
	time.Sleep(50 * time.Millisecond)
	calls, in, out, _ := s.Stats()
	if calls != int64(caller.count()) {
		t.Errorf("stats calls = %d, caller calls = %d — must match 1:1", calls, caller.count())
	}
	if in <= 0 || out <= 0 {
		t.Errorf("token accounting empty: in=%d out=%d (fake reports 100/20)", in, out)
	}
	// Low verdicts accumulate per occurrence (the first pass ran 4 unique
	// lows + 1 cached echo = 5 suppressed occurrences by now).
	waitFor(t, func() bool { _, _, _, lows := s.Stats(); return lows >= 5 })
}

func TestVerdictCacheLRU_EvictsOldest(t *testing.T) {
	c := loadVerdictCache(filepath.Join(t.TempDir(), "guard_verdicts.json"), 2)
	c.put("a", verdictEntry{Verdict: VerdictLow, Ts: 1})
	c.put("b", verdictEntry{Verdict: VerdictLow, Ts: 2})
	if _, ok := c.get("a"); !ok {
		t.Fatal("a missing before eviction")
	}
	c.put("c", verdictEntry{Verdict: VerdictLow, Ts: 3}) // evicts b (LRU: a refreshed)
	if c.Len() != 2 {
		t.Fatalf("cache len = %d, want 2", c.Len())
	}
	if _, ok := c.get("b"); ok {
		t.Error("b should have been evicted")
	}
	if _, ok := c.get("a"); !ok {
		t.Error("a was refreshed and must survive")
	}
}

// The cumulative LLM usage counters persist (guard_stats.json) and a service
// rebuild over the same state dir RESUMES from them — the Security page's
// llm tiles must not reset on every restart.
func TestLLMUsageStats_PersistAcrossServiceRebuild(t *testing.T) {
	dir := t.TempDir()
	newLife := func() (*Service, *fakeCaller) {
		s := New(Options{StateDir: dir})
		caller := &fakeCaller{}
		caller.setAnswer(func(Job) (string, string, string, error) { return VerdictLow, "fixture", "", nil })
		s.Start(staticConfig{model: "judge", timeout: 5 * time.Second, blockSession: true, enabled: true}, caller, &fakeSink{})
		t.Cleanup(func() { s.Close(time.Second) })
		return s, caller
	}

	// Two unique contents on the first service lifetime → 2 real calls.
	s1, _ := newLife()
	if !s1.Enqueue(testJob("sk-proj-abcdefghij1234567890")) || !s1.Enqueue(testJob("sk-proj-qrstuvwxyz0987654321")) {
		t.Fatal("Enqueue refused fresh jobs")
	}
	waitFor(t, func() bool { _, _, _, lows := s1.Stats(); return lows == 2 })
	if calls, in, out, lows := s1.Stats(); calls != 2 || in != 200 || out != 40 || lows != 2 {
		t.Fatalf("lifetime stats = %d calls / %d in / %d out / %d lows, want 2/200/40/2 (both jobs answered low)", calls, in, out, lows)
	}
	// The write-through lands right after the stats block; poll for the file
	// rather than assuming it beats this line.
	statsPath := filepath.Join(dir, "guard_stats.json")
	waitFor(t, func() bool { _, err := os.Stat(statsPath); return err == nil })
	data, err := os.ReadFile(statsPath)
	if err != nil {
		t.Fatalf("guard_stats.json not written: %v", err)
	}
	if !strings.Contains(string(data), `"calls": 2`) {
		t.Errorf("guard_stats.json = %s, want calls 2 persisted", data)
	}

	// A rebuild over the same dir resumes the counters before any new call,
	// then extends them with the next real call.
	s2, _ := newLife()
	if calls, in, out, lows := s2.Stats(); calls != 2 || in != 200 || out != 40 || lows != 2 {
		t.Fatalf("resumed stats = %d calls / %d in / %d out / %d lows, want 2/200/40/2 (resumed)", calls, in, out, lows)
	}
	if !s2.Enqueue(testJob("sk-proj-zzzzzzzzzzz0987654321")) {
		t.Fatal("Enqueue refused the third job")
	}
	waitFor(t, func() bool { _, _, _, lows := s2.Stats(); return lows == 3 })
	if calls, in, out, lows := s2.Stats(); calls != 3 || in != 300 || out != 60 || lows != 3 {
		t.Fatalf("extended stats = %d calls / %d in / %d out / %d lows, want 3/300/60/3", calls, in, out, lows)
	}
}

// The repeat-interception index: secret-kind highs join it (fresh AND cached
// replays), path-kind highs never do, entries persist hash-only and survive
// a service rebuild, and a later low verdict for the same content does NOT
// de-list it (the high stays authoritative until eviction).
func TestBlockedContentIndex_Semantics(t *testing.T) {
	dir := t.TempDir()
	caller := &fakeCaller{}
	caller.setAnswer(func(j Job) (string, string, string, error) {
		if j.Kind == KindPath {
			return VerdictHigh, "path high", "", nil
		}
		return VerdictHigh, "real key shape", "live material", nil
	})
	s := New(Options{StateDir: dir})
	s.Start(staticConfig{model: "judge", timeout: 5 * time.Second, blockSession: true, enabled: true}, caller, &fakeSink{})
	t.Cleanup(func() { s.Close(time.Second) })
	secretKey := "sk-blocked-dummy-not-a-real-key"

	// A secret-kind high lands in the index with its attribution.
	s.Enqueue(Job{Kind: KindSecret, Rule: "openai_api_key", Hit: secretKey, Ts: 1})
	waitFor(t, func() bool { _, ok := s.ContentBlocked(secretKey); return ok })
	if bc, _ := s.ContentBlocked(secretKey); bc.Rule != "openai_api_key" || bc.Reason != "real key shape" || bc.Kind != KindSecret {
		t.Errorf("index attribution = %+v", bc)
	}

	// A path-kind high never joins (path literals repeat legitimately).
	s.Enqueue(Job{Kind: KindPath, Rule: "ssh", Hit: "~/.gnupg", Ts: 2})
	waitFor(t, func() bool { return caller.count() >= 2 })
	time.Sleep(30 * time.Millisecond)
	if _, ok := s.ContentBlocked("~/.gnupg"); ok {
		t.Error("path-kind high joined the repeat-interception index")
	}

	// The persisted file is hash-only (no raw hit bytes) and a rebuild over
	// the same dir still intercepts.
	s.Close(time.Second)
	data, err := os.ReadFile(filepath.Join(dir, "guard_blocked.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secretKey) {
		t.Error("blocked-content index persisted raw hit bytes")
	}
	s2 := New(Options{StateDir: dir})
	if bc, ok := s2.ContentBlocked(secretKey); !ok || bc.Reason != "real key shape" {
		t.Errorf("rebuilt index lookup = %+v ok=%v", bc, ok)
	}
}

// The restart drain race: a SIGINT'd predecessor writes a late block AFTER
// this store already loaded the file. The next persist adopts it instead of
// clobbering it — and an explicit Unblock still stays removed (the entry was
// in the last-known disk state, so it is our own removal, not a late write).
func TestBlockStore_DrainRaceMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "guard_blocks.json")

	// Predecessor's on-disk state before draining.
	if err := writeStateFile(path, blockFile{Version: 1, Blocks: map[string]Block{
		"pre-existing": {Kind: KindSecret, Rule: "r", Ts: 1000},
	}}); err != nil {
		t.Fatal(err)
	}

	b := loadBlockStore(path)
	time.Sleep(5 * time.Millisecond)

	// The predecessor drains and writes a NEW block after our load.
	late := blockFile{Version: 1, Blocks: map[string]Block{
		"pre-existing": {Kind: KindSecret, Rule: "r", Ts: 1000},
		"late-write":   {Kind: KindSecret, Rule: "r2", Ts: time.Now().UnixMilli()},
	}}
	if err := writeStateFile(path, late); err != nil {
		t.Fatal(err)
	}

	// Our own action triggers a persist: the late write must be adopted.
	b.Block("ours", Block{Kind: KindSecret, Rule: "r3", Ts: time.Now().UnixMilli()})
	if _, ok := b.Blocked("late-write"); !ok {
		t.Error("late predecessor block was clobbered by our persist")
	}
	if _, ok := b.Blocked("ours"); !ok {
		t.Error("our own block missing")
	}

	// An explicit unblock of the ADOPTED entry stays removed on the next
	// persist (it is in lastDisk now, so the merge cannot resurrect it).
	if !b.Unblock("late-write") {
		t.Fatal("unblock refused an adopted entry")
	}
	b.Block("another", Block{Kind: KindSecret, Rule: "r4", Ts: time.Now().UnixMilli()})
	if _, ok := b.Blocked("late-write"); ok {
		t.Error("unblocked entry resurrected by the merge")
	}

	// The surviving file reflects the merged truth for a successor.
	s2 := loadBlockStore(path)
	for _, sid := range []string{"late-write"} {
		if _, ok := s2.Blocked(sid); ok {
			t.Errorf("session %s should be unblocked on disk", sid)
		}
	}
	for _, sid := range []string{"pre-existing", "ours", "another"} {
		if _, ok := s2.Blocked(sid); !ok {
			t.Errorf("session %s lost on disk", sid)
		}
	}
}
