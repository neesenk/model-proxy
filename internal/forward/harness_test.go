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

	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/provider"
	"model-proxy/internal/targetexec"
)

// fakeProv implements provider.Provider as a static-key passthrough.
type fakeProv struct {
	key          string
	authErr      error
	rewriteCalls int
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
	f.rewriteCalls++
	return url, body
}
func (f *fakeProv) Logout() error                           { return nil }
func (f *fakeProv) Usage() error                            { return nil }
func (f *fakeProv) FetchModels() ([]string, error)          { return nil, nil }
func (f *fakeProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (f *fakeProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (f *fakeProv) ExtraHeaders(req *http.Request, path string)          {}
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

var _ targetexec.Effects = (*fakeEffects)(nil)

// fakeRouteState implements RouteState with programmable answers.
type fakeRouteState struct {
	pins             map[string]bool
	allDown          bool
	allRateLimited   bool
	earliest         time.Time
	recoveredUntried bool
	quotaMaxAge      time.Duration
}

func (s *fakeRouteState) PinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	return s.pins[exposed]
}
func (s *fakeRouteState) CooldownState(targets []RouteTarget, now time.Time, quotaMaxAge time.Duration) (bool, bool, time.Time) {
	return s.allDown, s.allRateLimited, s.earliest
}
func (s *fakeRouteState) HasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time, quotaMaxAge time.Duration) bool {
	return s.recoveredUntried
}
func (s *fakeRouteState) QuotaFreshnessMaxAge(cfg *Config) time.Duration { return s.quotaMaxAge }

var _ RouteState = (*fakeRouteState)(nil)

// fakeResolverState implements routing.ResolverState: everything healthy,
// spread starts at zero.
type fakeResolverState struct{}

func (fakeResolverState) ResolverSpreadStart(parent string, n int, generation uint64) int { return 0 }
func (fakeResolverState) TargetHealthy(virtual, model string, now time.Time) bool         { return true }

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
	h := &harness{state: &fakeRouteState{pins: map[string]bool{}, quotaMaxAge: time.Minute}, gate: gate, fx: fx, events: events}
	h.svc = Services{
		Client:        &http.Client{Timeout: 30 * time.Second},
		Metrics:       counters.NewMetricsStore(),
		Tokens:        counters.NewTokenCounter(),
		Agents:        counters.NewAgentCounter(),
		Events:        events,
		FusionReg:     fusion.NewRegistry(),
		NewHealthGate: func(parentOf map[string]string) targetexec.HealthGate { return gate },
		NewEffects:    func(generation uint64) targetexec.Effects { return fx },
		Schedule:      passthroughSchedule,
		ShadowDispatch: func(runtime Snapshot, proto, backendProto, calledModel, exposed string, primary RouteTarget, primaryRequestID string, commit *targetexec.Commit) {
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
