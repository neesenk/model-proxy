package app

import (
	"encoding/json"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"model-proxy/internal/runtime"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- quota_test.go ----

// The Parse*Quota parser tests moved to the provider package in Phase 1
// (provider/quota_parse_test.go) so go test ./provider covers the parsers.
// This file keeps runtimestate.QuotaTracker and root scheduling-adapter tests.

func TestQuotaTracker_PersistAndLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota_state.json")
	cfg := func() *configdomain.Config {
		return &configdomain.
			// Production order is BuildProviders → Start → Load, so the provider set
			// is populated when Load filters persisted keys; model that here.
			Config{}
	}

	provs := func() map[string]provider.Provider {
		return map[string]provider.Provider{"zhipu": &snapshotProv{rem: 0}}
	}
	tr := newStandaloneQuotaTracker(path, cfg, provs)
	tr.SetSnapshot("zhipu", &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.42, AsOf: time.Now()})
	if err := tr.Persist(); err != nil {
		t.Fatal(err)
	}

	tr2 := newStandaloneQuotaTracker(path, cfg, provs)
	tr2.Load()
	if s := tr2.Snapshot("zhipu"); s == nil || s.RemainingPct != 0.42 {
		t.Fatalf("after reload: %+v", s)
	}
}

func TestQuotaTracker_PollAllCallsQuota(t *testing.T) {
	dir := t.TempDir()
	tr := newStandaloneQuotaTracker(filepath.Join(dir, "q.json"),
		func() *configdomain.Config {
			return &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "zhipu"}}}
		},
		func() map[string]provider.Provider {
			return map[string]provider.Provider{"x": &snapshotProv{rem: 0.77}}
		},
	)
	tr.PollAll(time.Now())
	if s := tr.Snapshot("x"); s == nil || s.RemainingPct != 0.77 {
		t.Fatalf("pollAll did not populate: %+v", s)
	}
}

// snapshotProv is a test Provider returning a fixed snapshot.
type snapshotProv struct{ rem float64 }

func (s *snapshotProv) AuthHeaders(*http.Request) error                        { return nil }
func (s *snapshotProv) Refresh() error                                         { return nil }
func (s *snapshotProv) RewriteRequest(string, []byte, string) (string, []byte) { return "", nil }
func (s *snapshotProv) Logout() error                                          { return nil }
func (s *snapshotProv) Usage() error                                           { return nil }
func (s *snapshotProv) FetchModels() ([]string, error)                         { return nil, nil }
func (s *snapshotProv) Quota() (*provider.QuotaSnapshot, error) {
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: s.rem, AsOf: time.Now()}, nil
}
func (s *snapshotProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (s *snapshotProv) ExtraHeaders(*http.Request, string)                   {}
func (s *snapshotProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

// quotaCallProv wraps snapshotProv, counting Quota() calls (with an optional
// delay so concurrent refreshOne calls overlap and hit the in-flight guard).
type quotaCallProv struct {
	snapshotProv
	calls *atomic.Int32
	delay time.Duration
}

type blockingQuotaProv struct {
	snapshotProv
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (q *blockingQuotaProv) AuthHeaders(*http.Request) error { return nil }
func (q *blockingQuotaProv) Refresh() error                  { return nil }
func (q *blockingQuotaProv) Quota() (*provider.QuotaSnapshot, error) {
	if q.calls.Add(1) == 1 {
		close(q.started)
	}
	<-q.release
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now()}, nil
}

func (q *quotaCallProv) Quota() (*provider.QuotaSnapshot, error) {
	q.calls.Add(1)
	if q.delay > 0 {
		time.Sleep(q.delay)
	}
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now()}, nil
}

// flakyProv fails Quota() the first `fails` calls with err (a transient or
// permanent error string), then returns a good snapshot. Counts calls. Used to
// exercise fetchQuota's transient-error retry + the no-retry-for-permanent rule.
type flakyProv struct {
	snapshotProv
	fails int
	err   string
	calls *atomic.Int32
}

func (f *flakyProv) Quota() (*provider.QuotaSnapshot, error) {
	n := int(f.calls.Add(1))
	if n <= f.fails {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: f.err, AsOf: time.Now()}, nil
	}
	return &provider.QuotaSnapshot{Billing: provider.BillingPlan, RemainingPct: 0.5, AsOf: time.Now()}, nil
}

// TestQuotaTracker_RefreshOneCoalescesConcurrent: many concurrent refreshOne
// calls for one provider collapse to a single Quota() call (in-flight guard).
func TestQuotaTracker_RefreshOneCoalescesConcurrent(t *testing.T) {
	prov := &blockingQuotaProv{started: make(chan struct{}), release: make(chan struct{})}
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "zhipu"}}}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	var first sync.WaitGroup
	first.Add(1)
	go func() { defer first.Done(); tr.RefreshOne("x") }()
	<-prov.started

	var coalesced sync.WaitGroup
	for i := 0; i < 11; i++ {
		coalesced.Add(1)
		go func() { defer coalesced.Done(); tr.RefreshOne("x") }()
	}
	coalesced.Wait()
	close(prov.release)
	first.Wait()
	if got := prov.calls.Load(); got != 1 {
		t.Fatalf("concurrent refreshOne: Quota() called %d times, want exactly 1", got)
	}
	if got := tr.Snapshot("x"); got == nil || got.RemainingPct != 0.5 {
		t.Fatalf("coalesced refresh snapshot = %+v, want RemainingPct=0.5", got)
	}
}

// TestQuotaTracker_RefreshOneDebouncesSequential: a second refreshOne within
// pollInterval/2 of the first is dropped (debounce).
func TestQuotaTracker_RefreshOneDebouncesSequential(t *testing.T) {
	var calls atomic.Int32
	prov := &quotaCallProv{calls: &calls} // pollInterval default 5m -> half 2.5m
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "zhipu"}}}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config { return cfg },
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.RefreshOne("x") // calls=1, sets last
	tr.RefreshOne("x") // within 2.5m -> debounced
	if got := calls.Load(); got != 1 {
		t.Errorf("sequential refreshOne: Quota() called %d times, want 1 (debounced)", got)
	}
}

// TestQuotaTracker_StickyPersistLoad: persist() writes sticky from the atomic
// full snapshot and load() restores it into loadedSticky, so the proxy resumes
// parking on the same providers after a restart.
func TestQuotaTracker_StickyPersistLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota_state.json")
	tr := newStandaloneQuotaTracker(path, func() *configdomain.Config { return &configdomain.Config{} }, func() map[string]provider.Provider { return nil })
	since := time.Unix(123, 0)
	tr.FullSnapshot = func() runtimestate.PersistedFullSnapshot {
		return runtimestate.PersistedFullSnapshot{
			Sticky: map[string]runtimestate.Sticky{
				"glm-5.2": {Provider: "zhipu", Since: since},
			},
		}
	}
	if err := tr.Persist(); err != nil {
		t.Fatal(err)
	}

	tr2 := newStandaloneQuotaTracker(path, func() *configdomain.Config { return &configdomain.Config{} }, func() map[string]provider.Provider { return nil })
	tr2.Load()
	got := tr2.LoadedSticky["glm-5.2"]
	if got.Provider != "zhipu" || !got.Since.Equal(since) {
		t.Fatalf("sticky not restored: %+v", got)
	}
}

// TestQuotaPersist_DaemonNotesRoundTrip (d473cb2 follow-up): the daemon wires
// p.quota.FullSnapshot = p.snapshotPersistedState, so its persist path projects
// quotas through snapshotPersistedState — which dropped Notes. The standalone
// tracker branch was fixed, but the daemon's quota_state.json still carried no
// notes, blanking the console/usage link (the only usage surface of
// console-only providers like mimo) after every restart until the next poll.
// This test exercises the DAEMON shape: a real Proxy commits a snapshot and
// persists through its FullSnapshot branch, the on-disk JSON is parsed (not
// grepped) for the exact notes, and a fresh proxy on the same state file
// restores them on boot.
func TestQuotaPersist_DaemonNotesRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"mimo": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{},
	}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")

	p1 := newTestProxyAt(t, cfg, statePath)
	notes := []string{"console only", "Balance & recharge: https://example.com/console"}
	// The console-only quota shape: BillingUnknown with the console URL in Notes.
	p1.quota.SetSnapshot("mimo", &provider.QuotaSnapshot{
		Billing: provider.BillingUnknown, RemainingPct: -1, Notes: notes, AsOf: time.Now(),
	})
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("daemon persist: %v", err)
	}

	// The daemon-written state file itself must carry the notes.
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Providers map[string]runtimestate.PersistedQuotaSnapshot `json:"providers"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("state file is not valid JSON: %v\n%s", err, raw)
	}
	persisted, ok := onDisk.Providers["mimo"]
	if !ok {
		t.Fatalf("persisted providers missing mimo:\n%s", raw)
	}
	if !reflect.DeepEqual(persisted.Notes, notes) {
		t.Fatalf("persisted notes = %q, want %q (daemon FullSnapshot projection dropped them)", persisted.Notes, notes)
	}

	// Restart: a fresh proxy boots on the same file and restores the notes.
	p2 := newTestProxyAt(t, cfg, statePath)
	s := p2.quota.Snapshot("mimo")
	if s == nil {
		t.Fatal("restored quota snapshot missing after daemon restart")
	}
	if !reflect.DeepEqual(s.Notes, notes) {
		t.Fatalf("restored notes = %q, want %q (console link must survive restart)", s.Notes, notes)
	}
}

// TestIsTransientQuotaErr: network/DNS/timeout/5xx errors are retry-worthy;
// auth/config/retcode/4xx errors are permanent (retry won't help). The DNS
// "no such host" error the user hit on zhipu must classify transient.
func TestIsTransientQuotaErr(t *testing.T) {
	cases := []struct {
		err  string
		want bool
	}{
		{`Get "https://open.bigmodel.cn/x": dial tcp: lookup open.bigmodel.cn: no such host`, true},
		{`Post "https://x": dial tcp 1.2.3.4:443: connect: connection refused`, true},
		{"context deadline exceeded", true},
		{"read tcp 1.2.3.4:443: read: connection reset by peer", true},
		{"EOF", true},
		{"HTTP 500", true},
		{"HTTP 503", true},
		{"HTTP 429", true}, // rate-limited -> retry
		{"session expired (HTTP 401): Session expired - re-login", false},
		{"not logged in", false},
		{"no project_id in store", false},
		{"monthly usage retcode=1 message=denied", false},
		{"AK/SK not configured", false},
		{"HTTP 400", false},
		{"HTTP 403", false},
		{"HTTP 404", false},
	}
	for _, tc := range cases {
		if got := runtimestate.IsTransientQuotaErr(tc.err); got != tc.want {
			t.Errorf("runtimestate.IsTransientQuotaErr(%q)=%v want %v", tc.err, got, tc.want)
		}
	}
}

// TestQuotaTracker_FetchQuotaRetriesTransient: a provider that fails twice
// with a transient DNS error then succeeds must be retried until it recovers -
// the fix for "usage error doesn't retry, keeps showing no such host". Asserts
// the exact call count (2 fails + 1 success = 3) so a regression that drops
// retry turns the test red.
func TestQuotaTracker_FetchQuotaRetriesTransient(t *testing.T) {
	var calls atomic.Int32
	prov := &flakyProv{
		fails: 2,
		err:   `Get "https://x": dial tcp: lookup x: no such host`,
		calls: &calls,
	}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config {
			return &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "zhipu"}}}
		},
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.SetRetryBackoff(time.Millisecond) // fast
	s := tr.FetchQuota(prov, time.Now())
	if s.Err != "" {
		t.Fatalf("expected recovery after retry, got Err=%q", s.Err)
	}
	if s.RemainingPct != 0.5 {
		t.Errorf("RemainingPct=%v want 0.5", s.RemainingPct)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("Quota() called %d times, want 3 (2 transient fails + 1 success)", got)
	}
}

// TestQuotaTracker_FetchQuotaNoRetryPermanent: a permanent error (session
// expired) must NOT be retried - one call, Err preserved. Retrying auth errors
// would just burn time (they won't self-heal).
func TestQuotaTracker_FetchQuotaNoRetryPermanent(t *testing.T) {
	var calls atomic.Int32
	prov := &flakyProv{
		fails: 5, // would "recover" if retried, but permanent err must short-circuit
		err:   "session expired (HTTP 401): re-login",
		calls: &calls,
	}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config {
			return &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "aqp"}}}
		},
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.SetRetryBackoff(time.Millisecond)
	s := tr.FetchQuota(prov, time.Now())
	if s.Err == "" {
		t.Fatalf("expected permanent error preserved, got success")
	}
	if !strings.Contains(s.Err, "session expired") {
		t.Errorf("Err=%q want session expired", s.Err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("Quota() called %d times, want 1 (no retry for permanent error)", got)
	}
}

// TestQuotaTracker_PollAllRetriesTransientError: at the pollAll level, a
// transient flaky provider recovers within one poll cycle (the snapshot stored
// is the good one, not the error). Guards the wiring from pollAll -> fetchQuota.
func TestQuotaTracker_PollAllRetriesTransientError(t *testing.T) {
	var calls atomic.Int32
	prov := &flakyProv{
		fails: 2,
		err:   `dial tcp: lookup x: no such host`,
		calls: &calls,
	}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config {
			return &configdomain.Config{Providers: map[string]configdomain.Provider{"x": {Provider: "zhipu"}}}
		},
		func() map[string]provider.Provider { return map[string]provider.Provider{"x": prov} })
	tr.SetRetryBackoff(time.Millisecond)
	tr.PollAll(time.Now())
	s := tr.Snapshot("x")
	if s == nil || s.Err != "" {
		t.Fatalf("pollAll should have recovered via retry, got %+v", s)
	}
	if s.RemainingPct != 0.5 {
		t.Errorf("RemainingPct=%v want 0.5", s.RemainingPct)
	}
}

// TestQuotaTracker_PollOneSingleAccount: pollOne refreshes ONLY the named key
// (a config name or a pooled-account virtual id "name#<id>"), not every
// provider. This is the per-account "Refresh usage" contract: clicking it on
// one account must not re-poll the others. Asserts the named provider was
// polled and a sibling was not.
func TestQuotaTracker_PollOneSingleAccount(t *testing.T) {
	var aCalls, bCalls atomic.Int32
	aProv := &quotaCallProv{calls: &aCalls}
	bProv := &quotaCallProv{calls: &bCalls}
	provs := map[string]provider.Provider{
		"zhipu":            aProv,
		"zhipu#account-id": bProv, // pooled-account virtual id
	}
	tr := newStandaloneQuotaTracker(filepath.Join(t.TempDir(), "q.json"),
		func() *configdomain.Config {
			return &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
		},
		func() map[string]provider.Provider { return provs })

	// Poll just the virtual-id account.
	if ok := tr.PollOne("zhipu#account-id"); !ok {
		t.Fatal("pollOne returned false for a live virtual id")
	}
	if got := bCalls.Load(); got != 1 {
		t.Errorf("virtual-id Quota() called %d times, want 1", got)
	}
	if got := aCalls.Load(); got != 0 {
		t.Errorf("parent zhipu Quota() called %d times, want 0 (pollOne is single-account)", got)
	}
	// Snapshot stored under the polled key.
	if s := tr.Snapshot("zhipu#account-id"); s == nil || s.RemainingPct != 0.5 {
		t.Errorf("virtual-id snapshot=%+v want 0.5", s)
	}

	// Unknown key -> false, no poll.
	before := bCalls.Load()
	if ok := tr.PollOne("ghost"); ok {
		t.Error("pollOne returned true for an unknown key")
	}
	if got := bCalls.Load(); got != before {
		t.Errorf("unknown-key pollOne polled %d extra times", got-before)
	}
}

// ---- quota_test_support_test.go ----

// newStandaloneQuotaTracker builds a QuotaTracker with an isolated Manager for
// tests (no Proxy lifecycle).
func newStandaloneQuotaTracker(
	path string,
	cfg func() *configdomain.Config,
	provs func() map[string]provider.Provider,
) *runtime.QuotaTracker {
	manager := &runtime.Manager{}
	manager.ReplaceGeneration(0)
	return runtime.NewQuotaTracker(path, cfg, provs, manager)
}

// ---- quota_poll_test.go ----

// TestPollAll_PollsPooledVirtuals (bug 1): a multi-account provider is unrolled
// into "name#<accountID>" virtuals in the RUNTIME map; the parent name is NOT a
// runtime key. pollAll must iterate the runtime map (the runnable instances) —
// iterating cfg.Providers (parent names) looked the parent up and found nil, so
// every pooled account stayed BillingUnknown and was never polled, defeating
// surplus/tier scheduling and persistence for the whole pool.
func TestPollAll_PollsPooledVirtuals(t *testing.T) {
	// cfg.Providers carries only the PARENT name, exactly as a pooled provider
	// appears in config; the runtime map carries the unrolled virtuals.
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{"zhipu": {Provider: "zhipu"}}}
	provs := map[string]provider.Provider{
		"zhipu#a": &testProv{key: "zhipu#a"},
		"zhipu#b": &testProv{key: "zhipu#b"},
	}
	tr := newStandaloneQuotaTracker("", func() *configdomain.Config { return cfg }, func() map[string]provider.Provider { return provs })
	tr.PollAll(time.Now())
	for _, vid := range []string{"zhipu#a", "zhipu#b"} {
		if tr.Snapshot(vid) == nil {
			t.Errorf("%s: pooled virtual has no snapshot after pollAll (poller skipped it)", vid)
		}
	}
	// The parent name is not runnable and must NOT be polled as a key.
	if tr.Snapshot("zhipu") != nil {
		t.Errorf("parent name should not appear as a polled runtime key")
	}
}

// ---- quota_tracker_lifecycle_test.go ----

// --- runtimestate.QuotaTracker.stop / pollAfter ---

func TestQuotaTracker_Stop(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *configdomain.Config { return &configdomain.Config{} }
	provs := func() map[string]provider.Provider { return nil }
	tr := newStandaloneQuotaTracker(dir+"/q.json", cfg, provs)
	tr.Start()
	// stop must be idempotent and not block.
	tr.Stop()
	tr.Stop() // second stop is a no-op (sync.Once)
}

func TestQuotaTracker_PollAfter(t *testing.T) {
	dir := t.TempDir()
	cfg := func() *configdomain.Config { return &configdomain.Config{} }
	var called atomic.Int32
	provs := func() map[string]provider.Provider {
		called.Add(1)
		return nil
	}
	tr := newStandaloneQuotaTracker(dir+"/q.json", cfg, provs)
	tr.Start()
	defer tr.Stop()
	// pollAfter → pollAll → provs(). Measure the delta: start()'s bootstrap poll
	// (10s) and the ticker (5m default) can't fire within this window, so any
	// provs() call after dispatch must come from the pollAfter path.
	before := called.Load()
	tr.PollAfter(50 * time.Millisecond)
	// Deadline poll instead of a fixed sleep: on a slow machine the poll fires
	// after the fixed window and the assertion flakes.
	deadline := time.Now().Add(5 * time.Second)
	for called.Load()-before < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := called.Load() - before; got < 1 {
		t.Errorf("pollAfter did not trigger a poll: provs called %d more times", got)
	}
}
