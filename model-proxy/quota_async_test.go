package main

import (
	runtimestate "model-proxy/internal/runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/provider"
)

// blockQuotaProv embeds testProv but blocks in Quota until release is closed,
// and records that Quota was entered. Used to prove pollAsync/refreshAsync are
// tracked by the poller WaitGroup (stop waits) and stop-aware (no-op after stop).
type blockQuotaProv struct {
	testProv
	release   chan struct{}
	entered   chan struct{}
	enterOnce sync.Once
	ran       atomic.Bool
	done      atomic.Bool
}

// TestQuotaLaunch_StopWaitsForAdmittedTask establishes the lifecycle contract
// without scheduler timing: launch returns only after the task is admitted and
// counted; once the callback has started, stop must remain blocked until it is
// released and completes.
func TestQuotaLaunch_StopWaitsForAdmittedTask(t *testing.T) {
	tr := newBlockTracker(t, &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})})
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	if admitted := tr.Launch(func() {
		close(started)
		<-release
		close(finished)
	}); !admitted {
		t.Fatal("task was rejected before stop")
	}
	<-started

	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop returned while an admitted task was blocked")
	default:
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted task did not finish after release")
	}
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop did not return after the admitted task finished")
	}
}

func TestQuotaLaunch_RejectsAfterStop(t *testing.T) {
	tr := newBlockTracker(t, &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})})
	tr.Stop()
	called := false
	if admitted := tr.Launch(func() { called = true }); admitted {
		t.Fatal("task admitted after stop")
	}
	if called {
		t.Fatal("callback ran after stop")
	}
}

// TestAsyncDispatch_ConcurrentStop supplements the deterministic launch tests
// with repeated contention at the public async-dispatch boundary.
func TestAsyncDispatch_ConcurrentStop(t *testing.T) {
	for i := 0; i < 200; i++ {
		release := make(chan struct{})
		prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: release}
		tr := newBlockTracker(t, prov)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); tr.PollAsync(time.Now()) }()
		go func() { defer wg.Done(); tr.Stop() }()
		close(release)
		wg.Wait()
		if prov.ran.Load() && !prov.done.Load() {
			t.Fatal("stop returned before an admitted callback completed")
		}
	}
}

// TestPollAll_DiscardsStaleGeneration proves a slow quota response from the old
// config cannot overwrite the new generation after reload.
func TestPollAll_DiscardsStaleGeneration(t *testing.T) {
	var generation atomic.Uint64
	generation.Store(1)
	release := make(chan struct{})
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: release}
	tr := newBlockTracker(t, prov)
	tr.Generation = generation.Load
	done := make(chan struct{})
	go func() {
		tr.PollAllGeneration(time.Now(), 1)
		close(done)
	}()
	pollFor(t, prov.ran.Load, time.Second, "old-generation quota poll did not start")
	generation.Store(2)
	tr.Runtime().ReplaceGeneration(2)
	tr.ClearForGeneration(2)
	sentinel := &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.75}
	if !tr.CommitSnapshot(2, "x", sentinel) {
		t.Fatal("current-generation sentinel was rejected")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("old-generation quota poll did not finish after release")
	}
	if got := tr.Snapshot("x"); got == nil || got.Billing != sentinel.Billing || got.RemainingPct != sentinel.RemainingPct {
		t.Fatalf("stale generation replaced current snapshot: got=%+v want=%+v", got, sentinel)
	}
}

func (b *blockQuotaProv) Quota() (*provider.QuotaSnapshot, error) {
	b.ran.Store(true)
	b.enterOnce.Do(func() { close(b.entered) })
	<-b.release
	b.done.Store(true)
	return &provider.QuotaSnapshot{Billing: provider.BillingUnknown}, nil
}

func newBlockTracker(t *testing.T, prov *blockQuotaProv) *runtimestate.QuotaTracker {
	t.Helper()
	if prov.entered == nil {
		prov.entered = make(chan struct{})
	}
	tr := newStandaloneQuotaTracker(t.TempDir()+"/q.json",
		func() *Config { return &Config{} },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	t.Cleanup(tr.Stop)
	return tr
}

// pollFor waits up to timeout for cond, failing the test if it never holds.
func pollFor(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal(msg)
}

func waitForTrackerAdmissionClosed(t *testing.T, tr *runtimestate.QuotaTracker) {
	t.Helper()
	pollFor(t, func() bool {
		return !tr.AdmissionOpen()
	}, time.Second, "stop did not close task admission")
}

// TestPollAsync_TrackedByStop: pollAsync's goroutine is tracked by the poller
// WaitGroup, so stop() must WAIT for an in-flight poll rather than racing it —
// otherwise Close()'s final persist wouldn't be the last write (the P1a bug).
func TestPollAsync_TrackedByStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.PollAsync(time.Now())
	pollFor(t, prov.ran.Load, time.Second, "pollAsync did not enter the provider's Quota")

	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop() returned while pollAsync is still in-flight (not tracked by poller)")
	default:
	}
	close(prov.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after the in-flight poll completed")
	}
}

// TestPollAsync_NoopAfterStop: a poll dispatched after stop is a no-op — it must
// NOT run pollAll (which would persist after Close's final flush).
func TestPollAsync_NoopAfterStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.Stop()
	tr.PollAsync(time.Now())
	if prov.ran.Load() {
		t.Errorf("pollAsync dispatched a poll after stop; should be a no-op")
	}
}

// TestRefreshAsync_TrackedByStop: the 429 refreshAsync path is tracked too.
func TestRefreshAsync_TrackedByStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.RefreshAsync("x")
	select {
	case <-prov.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refreshAsync did not start")
	}
	stopped := make(chan struct{})
	go func() { tr.Stop(); close(stopped) }()
	waitForTrackerAdmissionClosed(t, tr)
	select {
	case <-stopped:
		t.Fatal("stop() returned while refreshAsync is still in-flight (not tracked)")
	default:
	}
	close(prov.release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return after the in-flight refresh completed")
	}
}

// TestRefreshAsync_NoopAfterStop: a 429 refresh dispatched after stop is a no-op.
func TestRefreshAsync_NoopAfterStop(t *testing.T) {
	prov := &blockQuotaProv{testProv: testProv{key: "x"}, release: make(chan struct{})}
	tr := newBlockTracker(t, prov)
	tr.Stop()
	tr.RefreshAsync("x")
	if prov.ran.Load() {
		t.Errorf("refreshAsync dispatched after stop; should be a no-op")
	}
}
