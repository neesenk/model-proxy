// harness_test.go — test fakes for the forward pipeline's consumer-owned
// ports (RouteState, targetexec.HealthGate/Effects, resolver state) and the
// Services/Snapshot builders. These are minimal in-memory fakes; no
// production wiring is copied here.
package forward

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/provider"
	runtimedomain "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
)

// fakeProv implements provider.Provider as a static-key passthrough.
type fakeProv struct {
	key     string
	authErr error
}

func (f *fakeProv) AuthHeaders(req *http.Request) error {
	if f.authErr != nil {
		return f.authErr
	}
	req.Header.Set("Authorization", "Bearer "+f.key)
	return nil
}
func (f *fakeProv) Refresh() error { return nil }
func (f *fakeProv) RewriteRequest(url string, body []byte, path string) (string, []byte) {
	return url, body
}
func (f *fakeProv) Logout() error                           { return nil }
func (f *fakeProv) Usage() error                            { return nil }
func (f *fakeProv) FetchModels() ([]string, error)          { return nil, nil }
func (f *fakeProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (f *fakeProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (f *fakeProv) ExtraHeaders(req *http.Request, _ []byte, sessionID string, path string) {
	// Test probe of the widened seam: echo the session id the executor handed
	// to the provider so harness tests can assert the forward-resolved client
	// session survives protocol conversion.
	if sessionID != "" {
		req.Header.Set("x-test-resolved-session", sessionID)
	}
}
func (f *fakeProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

var _ provider.Provider = (*fakeProv)(nil)

// fakeHealthGate implements targetexec.HealthGate with in-memory records.
type fakeHealthGate struct {
	mu             sync.Mutex
	halfOpenOpen   map[string]bool // providers whose circuit is open (slot refused)
	failures       map[string]int
	modelFailures  map[string]int
	successes      map[string]int
	rateLimits     map[string]time.Time
	learned        []string
	blockedParams  map[string][]string
	wireMisses     []string
	modelLockedSet map[string]bool
}

func newFakeHealthGate() *fakeHealthGate {
	return &fakeHealthGate{
		halfOpenOpen:   map[string]bool{},
		failures:       map[string]int{},
		modelFailures:  map[string]int{},
		successes:      map[string]int{},
		rateLimits:     map[string]time.Time{},
		blockedParams:  map[string][]string{},
		modelLockedSet: map[string]bool{},
	}
}

func (g *fakeHealthGate) ModelLocked(provider, model string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.modelLockedSet[provider+"/"+model]
}
func (g *fakeHealthGate) TakeHalfOpenSlot(provider string, generation uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.halfOpenOpen[provider]
}
func (g *fakeHealthGate) ReleaseHalfOpenSlot(provider string, generation uint64) {}
func (g *fakeHealthGate) RecordSuccess(provider, model string, generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.successes[provider]++
}
func (g *fakeHealthGate) RecordFailure(provider string, scheduling Scheduling, generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failures[provider]++
}
func (g *fakeHealthGate) RecordModelFailure(provider, model string, scheduling Scheduling, generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.modelFailures[provider+"/"+model]++
}
func (g *fakeHealthGate) RecordRateLimit(provider string, until time.Time, kind string, generation uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rateLimits[provider] = until
}
func (g *fakeHealthGate) LearnParamBlock(provider, model, parameter string, generation uint64) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.learned = append(g.learned, provider+"/"+model+"/"+parameter)
	return true
}
func (g *fakeHealthGate) ApplyParamBlock(provider, model string, body []byte) []byte { return body }
func (g *fakeHealthGate) NoteWireResponsesMiss(provider, model string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.wireMisses = append(g.wireMisses, provider+"/"+model)
}

var _ targetexec.HealthGate = (*fakeHealthGate)(nil)

// fakeEffects implements targetexec.Effects with counters only.
type fakeEffects struct {
	mu          sync.Mutex
	failovers   int
	failures    int
	rateLimited int
	committed   int
	logged      int
	// routing captures the request-log routing decision from each committed
	// attempt, enabling tests to verify the policy outcome reached the log.
	routing []*configdomain.RoutingDecision
}

func (e *fakeEffects) Failover(target RouteTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failovers++
}
func (e *fakeEffects) Failure(target RouteTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.failures++
}
func (e *fakeEffects) RateLimited(target RouteTarget) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rateLimited++
}
func (e *fakeEffects) LogAttempt(attempt targetexec.AttemptDTO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logged++
}
func (e *fakeEffects) CaptureResponse(body io.ReadCloser, attempt targetexec.AttemptDTO) io.ReadCloser {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.routing = append(e.routing, attempt.Scope.Log.Routing)
	return body
}
func (e *fakeEffects) CaptureUsage(body io.ReadCloser, attempt targetexec.AttemptDTO, observe func(targetexec.Usage)) io.ReadCloser {
	return body
}
func (e *fakeEffects) Committed(attempt targetexec.AttemptDTO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.committed++
}

func (e *fakeEffects) capturedRouting() []*configdomain.RoutingDecision {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]*configdomain.RoutingDecision, len(e.routing))
	copy(out, e.routing)
	return out
}

var _ targetexec.Effects = (*fakeEffects)(nil)

// fakeRouteState implements RouteState with programmable answers. The
// pin/cooldown/disabled answers stay fake (the pipeline tests drive those
// seams directly), but the latch + repeat-turn state is owned by a REAL
// runtime.Manager: an earlier version of this fake carried a line-by-line
// twin of Manager.RecordLatchOutcome/CheckRepeatTurn, which meant the latch
// tests validated the copy instead of the production logic and could drift
// silently. forward's production code must not import internal/runtime (the
// dependency DAG; internal/app adapts between the two) — this TEST-only seam
// is the narrow exception that keeps the tests honest.
type fakeRouteState struct {
	pins           map[string]bool
	disabled       map[string]bool
	allDown        bool
	allRateLimited bool
	// earliestIn is the cooldown's remaining time reported to forward,
	// anchored at the DECISION's now (see CooldownState): DecideFailure
	// computes wait = earliest-now, so a decision-relative window makes the
	// wait-retry branch true by construction. An absolute earliest set at
	// test start instead decays while the in-flight round runs — on a slow,
	// race-instrumented CI runner one round can exceed any fixed margin and
	// flip the branch to the 429 terminal (the flake this seam prevents).
	earliestIn       time.Duration
	recoveredUntried bool
	quotaMaxAge      time.Duration
	// latch owns the (session, route) latch and repeat-turn window exactly
	// like production. Construction must go through newFakeRouteState so the
	// generation gate matches the harness snapshots (Generation 1).
	latch *runtimedomain.Manager
}

// newFakeRouteState builds a fakeRouteState whose real Manager sits at the
// given generation (harness snapshots use Generation 1).
func newFakeRouteState(generation uint64) *fakeRouteState {
	m := &runtimedomain.Manager{}
	m.ReplaceGeneration(generation, nil)
	return &fakeRouteState{pins: map[string]bool{}, latch: m}
}

func (s *fakeRouteState) PinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	return s.pins[exposed]
}
func (s *fakeRouteState) CooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (bool, bool, time.Time) {
	return s.allDown, s.allRateLimited, now.Add(s.earliestIn)
}
func (s *fakeRouteState) HasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	return s.recoveredUntried
}
func (s *fakeRouteState) QuotaFreshnessMaxAge(cfg *Config) time.Duration { return s.quotaMaxAge }
func (s *fakeRouteState) LatchValue(sessionKey, route string) (Latch, bool) {
	v, ok := s.latch.LatchValue(sessionKey, route)
	return Latch{Target: v.Target, Since: v.Since, BadRuns: v.BadRuns}, ok
}

// SetLatch is a test helper seeding a latch directly (not part of the
// RouteState port — production writes go through RecordLatchOutcome). It
// seeds THROUGH the production write path: one bad signal with Consecutive=1
// escalates immediately, landing exactly {Target, Since=Now, BadRuns:0}.
func (s *fakeRouteState) SetLatch(sessionKey, route string, value Latch, generation uint64) bool {
	return s.latch.RecordLatchOutcome(runtimedomain.LatchOutcome{
		SessionKey:  sessionKey,
		Route:       route,
		Now:         value.Since,
		Dwell:       24 * time.Hour,
		Consecutive: 1,
		Target:      value.Target,
		BadSignals:  1,
	}, generation)
}

// RecordLatchOutcome delegates to the real Manager: the expiry check, streak
// increment/reset and escalation happen in the production critical section,
// so concurrent-request tests (TestServeLatchConcurrentBadRunsCountedAtomically)
// exercise the production atomicity guarantee, not a copy of it.
func (s *fakeRouteState) RecordLatchOutcome(o LatchOutcome, generation uint64) bool {
	return s.latch.RecordLatchOutcome(runtimedomain.LatchOutcome{
		SessionKey:  o.SessionKey,
		Route:       o.Route,
		Now:         o.Now,
		Dwell:       o.Dwell,
		Consecutive: o.Consecutive,
		Target:      o.Target,
		BadSignals:  o.BadSignals,
		Good:        o.Good,
	}, generation)
}

func (s *fakeRouteState) FilterDisabledTargets(targets []RouteTarget, parentOf map[string]string) []RouteTarget {
	if len(s.disabled) == 0 {
		return targets
	}
	kept := make([]RouteTarget, 0, len(targets))
	for _, t := range targets {
		if s.disabled[t.Provider] {
			continue
		}
		kept = append(kept, t)
	}
	return kept
}

func (s *fakeRouteState) CheckRepeatTurn(sessionKey, route, turnKey string, now time.Time, window time.Duration, generation uint64) bool {
	return s.latch.CheckRepeatTurn(sessionKey, route, turnKey, now, window, generation)
}

var _ RouteState = (*fakeRouteState)(nil)

// fakeResolverState implements routing.ResolverState: everything healthy,
// spread starts at zero.
type fakeResolverState struct{}

func (fakeResolverState) ResolverSpreadStart(parent string, n int, generation uint64) int { return 0 }
func (fakeResolverState) TargetHealthy(virtual, model string, now time.Time) bool         { return true }
func (fakeResolverState) ModelDisabled(provider, model string) bool                       { return false }

// fakeUpstream records requests and answers with a programmable responder.
type fakeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

// harness bundles one Services set, one RouteState and the fakes behind them.
type harness struct {
	svc    Services
	state  *fakeRouteState
	gate   *fakeHealthGate
	fx     *fakeEffects
	events *observeevents.Hub
	shadow []shadowCall
}

type shadowCall struct {
	exposed  string
	provider string
}

func newHarness() *harness {
	gate := newFakeHealthGate()
	fx := &fakeEffects{}
	events := observeevents.NewHub()
	h := &harness{state: newFakeRouteState(1), gate: gate, fx: fx, events: events}
	h.state.quotaMaxAge = time.Minute
	h.svc = Services{
		Client:        &http.Client{Timeout: 30 * time.Second},
		Metrics:       counters.NewMetricsStore(),
		Tokens:        counters.NewTokenCounter(),
		Agents:        counters.NewAgentCounter(),
		Events:        events,
		FusionReg:     fusion.NewRegistry(),
		NewHealthGate: func(_ *configdomain.Config, parentOf map[string]string) targetexec.HealthGate { return gate },
		NewEffects:    func(cfg *Config, generation uint64) targetexec.Effects { return fx },
		Schedule:      passthroughSchedule,
		ShadowDispatch: func(runtime Snapshot, proto, backendProto, calledModel, exposed string, primary RouteTarget, primaryRequestID, primaryAgent, primarySession string, commit *targetexec.Commit, routingDecision *RoutingDecision) {
			h.shadow = append(h.shadow, shadowCall{exposed: exposed, provider: primary.Provider})
		},
		ResolveBackendProto: func(declared, provName string, provCfg Provider, model, clientProto string, parentOf map[string]string) (string, bool) {
			if declared != "" {
				return declared, false
			}
			return clientProto, false
		},
		ResolverState: fakeResolverState{},
	}
	return h
}

// passthroughSchedule keeps the route order (no health/quota filtering).
func passthroughSchedule(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generation uint64) []RouteTarget {
	return targets
}

// snapshot builds a single-generation Snapshot over the given config; every
// configured provider gets a fakeProv implementation.
func (h *harness) snapshot(cfg *Config) Snapshot {
	providers := map[string]provider.Provider{}
	for name := range cfg.Providers {
		providers[name] = &fakeProv{key: "key-" + name}
	}
	routes := cfg.Routes
	return Snapshot{
		Cfg:            cfg,
		Generation:     1,
		Providers:      providers,
		PoolIndex:      map[string][]string{},
		ParentOf:       map[string]string{},
		ExpandedRoutes: routes,
		RouteKeys:      routeKeySetOf(routes),
	}
}

func routeKeySetOf(routes map[string][]RouteTarget) map[string]bool {
	keys := make(map[string]bool, len(routes))
	for k := range routes {
		keys[k] = true
	}
	return keys
}

// serve drives the pipeline once and returns the recorder.
func (h *harness) serve(snap Snapshot, proto, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, proto, w, r, "req-test")
	return w
}

// endEvents collects the hub's "end" events for one request id.
func (h *harness) endEvents(requestID string) []observeevents.Event {
	var out []observeevents.Event
	for _, e := range h.events.Snapshot() {
		if e.Type == "end" && e.RequestID == requestID {
			out = append(out, e)
		}
	}
	return out
}
