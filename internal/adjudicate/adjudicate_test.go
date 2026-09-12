package adjudicate

import (
	"context"
	"errors"
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
	answer func(j Job) (string, string, error)
}

func (f *fakeCaller) setAnswer(fn func(Job) (string, string, error)) {
	f.mu.Lock()
	f.answer = fn
	f.mu.Unlock()
}

func (f *fakeCaller) Adjudicate(_ context.Context, _ string, j Job) (string, string, Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, j)
	v, r, err := f.answer(j)
	return v, r, Usage{InputTokens: 100, OutputTokens: 20}, err
}

func (f *fakeCaller) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeSink records sink side effects in order.
type fakeSink struct {
	mu        sync.Mutex
	high      []Job
	low       []Job
	fails     []Job
	lowCached int
}

func (f *fakeSink) High(j Job, _, _ string) { f.mu.Lock(); f.high = append(f.high, j); f.mu.Unlock() }
func (f *fakeSink) Low(j Job, _, _ string, cached bool) {
	f.mu.Lock()
	f.low = append(f.low, j)
	if cached {
		f.lowCached++
	}
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
	caller.setAnswer(func(Job) (string, string, error) { return VerdictHigh, "real key shape", nil })

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
	caller.setAnswer(func(Job) (string, string, error) { return VerdictLow, "fixture text", nil })

	s.Enqueue(testJob("sk-capture-dummy-not-a-real-key"))
	waitFor(t, func() bool { _, l, _ := sink.counts(); return l == 1 })
	if _, ok := s.Blocked("sess-1"); ok {
		t.Error("low verdict must never block the session")
	}
	if h, _, f := sink.counts(); h != 0 || f != 0 {
		t.Errorf("high = %d fails = %d, want none", h, f)
	}
}

func TestCallerError_FailsOpenThroughSink(t *testing.T) {
	s, caller, sink := newTestService(t, Options{})
	caller.setAnswer(func(Job) (string, string, error) { return "", "", errors.New("dial timeout") })

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
	caller.setAnswer(func(Job) (string, string, error) { return VerdictLow, "fixture", nil })

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
	highCaller.setAnswer(func(Job) (string, string, error) { return VerdictHigh, "real", nil })
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
	caller2 := &fakeCaller{answer: func(Job) (string, string, error) {
		return VerdictHigh, "", nil // would block if actually called
	}}
	sink2 := &fakeSink{}
	s2.Start(staticConfig{model: "judge", timeout: time.Second, blockSession: true, enabled: true}, caller2, sink2)
	defer s2.Close(time.Second)
	if !s2.Enqueue(testJob("sk-capture-dummy-not-a-real-key")) {
		t.Fatal("Enqueue refused")
	}
	waitFor(t, func() bool { _, l, _ := sink2.counts(); return l == 1 })
	if caller2.count() != 0 {
		t.Errorf("caller called %d times on a cached verdict, want 0", caller2.count())
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
	caller := &fakeCaller{answer: func(Job) (string, string, error) {
		once.Do(func() { close(started) })
		<-release
		return VerdictLow, "", nil
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
	caller := &fakeCaller{answer: func(Job) (string, string, error) {
		return VerdictHigh, "", nil
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
	caller.setAnswer(func(Job) (string, string, error) { return VerdictLow, "fixture", nil })
	for i := 0; i < 4; i++ {
		s.Enqueue(testJob("sk-usage-dummy-not-a-real-key-000" + string(rune('a'+i))))
	}
	waitFor(t, func() bool { _, low, _ := sink.counts(); return low >= 1 })
	waitFor(t, func() bool { calls, _, _ := s.Stats(); return calls >= 1 })
	// Cache hit on identical content adds no call and no tokens.
	j := testJob("sk-usage-dummy-not-a-real-key-000a")
	s.Enqueue(j)
	time.Sleep(50 * time.Millisecond)
	calls, in, out := s.Stats()
	if calls != int64(caller.count()) {
		t.Errorf("stats calls = %d, caller calls = %d — must match 1:1", calls, caller.count())
	}
	if in <= 0 || out <= 0 {
		t.Errorf("token accounting empty: in=%d out=%d (fake reports 100/20)", in, out)
	}
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
