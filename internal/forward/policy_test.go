package forward

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
)

func bandPolicy(when configdomain.BandWhen, provider, model string) RoutePolicy {
	return RoutePolicy{Bands: []configdomain.RouteBand{{
		When:   when,
		Target: RouteTarget{Provider: provider, Model: model},
	}}}
}

func boolp(v bool) *bool { return &v }

func TestApplyRoutePolicyPrefersBandTarget(t *testing.T) {
	ordered := []RouteTarget{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}
	policy := bandPolicy(configdomain.BandWhen{FollowUp: boolp(true)}, "b", "m")

	got, decision := applyRoutePolicy(ordered, policy, routing.Profile{FollowUp: true}, nil)
	if len(got) != 2 || got[0].Provider != "b" || got[1].Provider != "a" {
		t.Fatalf("order = %+v, want b first with a as failover", got)
	}
	if decision == nil || decision.Source != "band" || decision.Target != "b/m" {
		t.Fatalf("band decision = %+v, want source=band target=b/m", decision)
	}
}

func TestApplyRoutePolicyNoMatchKeepsOrder(t *testing.T) {
	ordered := []RouteTarget{{Provider: "a", Model: "m"}, {Provider: "b", Model: "m"}}
	policy := bandPolicy(configdomain.BandWhen{FollowUp: boolp(true)}, "b", "m")

	got, decision := applyRoutePolicy(ordered, policy, routing.Profile{FollowUp: false}, nil)
	if got[0].Provider != "a" || len(got) != 2 {
		t.Fatalf("order = %+v, want the scheduled order unchanged", got)
	}
	if decision != nil {
		t.Fatalf("no-match decision = %+v, want nil", decision)
	}
}

func TestApplyRoutePolicyAbsentTargetKeepsOrder(t *testing.T) {
	ordered := []RouteTarget{{Provider: "a", Model: "m"}}
	policy := bandPolicy(configdomain.BandWhen{FollowUp: boolp(true)}, "ghost", "m")

	got, decision := applyRoutePolicy(ordered, policy, routing.Profile{FollowUp: true}, nil)
	if len(got) != 1 || got[0].Provider != "a" {
		t.Fatalf("order = %+v, want the scheduled order unchanged (never zero targets)", got)
	}
	if decision == nil || decision.Source != "band" || decision.Target != "ghost/m" {
		t.Fatalf("absent-target band decision = %+v, want source=band target=ghost/m", decision)
	}
}

// routePolicySnapshot builds a two-target route whose second target is the one
// the band prefers for follow-up turns.
func routePolicySnapshot() (Snapshot, *harness) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"a": {}, "b": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a", Model: "ma"}, {Provider: "b", Model: "mb"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": bandPolicy(configdomain.BandWhen{FollowUp: boolp(true)}, "b", "mb"),
		},
	}
	return h.snapshot(cfg), h
}

func serveOnceForPolicyTest(t *testing.T, snap Snapshot, h *harness, body string, force bool, forcedProvider string) (serveResult, *serveState) {
	t.Helper()
	p := pipeline{svc: h.svc, state: h.state}
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	st := &serveState{}
	res := p.serveOnce(serveRequest{
		runtime: snap,
		proto:   "openai", upPath: "/chat/completions", exposed: "m", calledModel: "m",
		targets: snap.Cfg.Routes["m"], routeKeys: map[string]bool{"m": true},
		force: force, forcedProvider: forcedProvider,
		origBody: []byte(body),
		writer:   httptest.NewRecorder(), request: r, requestID: "req-policy",
	}, st)
	return res, st
}

// TestServeOnceRoutePolicyPrefersBandTarget: a route_policy band moves its
// target to the front of the effective order; a request that matches no band
// keeps the scheduled order.
func TestServeOnceRoutePolicyPrefersBandTarget(t *testing.T) {
	snap, h := routePolicySnapshot()
	followUp := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`

	res, st := serveOnceForPolicyTest(t, snap, h, followUp, false, "")
	if res.firstTried.Provider != "b" {
		t.Fatalf("firstTried = %+v, want the band-preferred b/mb", res.firstTried)
	}
	if len(res.effectiveTargets) != 2 {
		t.Fatalf("effectiveTargets = %+v, want both targets kept as failover", res.effectiveTargets)
	}
	if st.routing == nil || st.routing.Source != "band" || st.routing.Target != "b/mb" {
		t.Fatalf("routing decision = %+v, want source=band target=b/mb", st.routing)
	}

	firstTurn := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	res, st = serveOnceForPolicyTest(t, snap, h, firstTurn, false, "")
	if res.firstTried.Provider != "a" {
		t.Fatalf("firstTried = %+v, want the scheduled a/ma for a first-turn request", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("no-match routing = %+v, want nil", st.routing)
	}
}

// TestServeOnceRoutePolicySkippedForHardSelection: a pin (force) or an explicit
// force-provider override is a hard selection — the policy must not reorder
// around it.
func TestServeOnceRoutePolicySkippedForHardSelection(t *testing.T) {
	followUp := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`

	snap, h := routePolicySnapshot()
	res, st := serveOnceForPolicyTest(t, snap, h, followUp, true, "")
	if res.firstTried.Provider != "a" {
		t.Fatalf("pin/force: firstTried = %+v, want the scheduled a/ma", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("pin/force routing = %+v, want nil", st.routing)
	}

	snap, h = routePolicySnapshot()
	res, st = serveOnceForPolicyTest(t, snap, h, followUp, false, "a")
	if res.firstTried.Provider != "a" {
		t.Fatalf("force-provider: firstTried = %+v, want the forced a/ma", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("force-provider routing = %+v, want nil", st.routing)
	}
}

// TestServeOnceRoutePolicyFusionBandTarget (slice 4): a band may point at a
// fusion recipe — the effective order starts with the fusion target, which the
// ordinary target loop dispatches into runFusion. The route's concrete targets
// stay behind it as the fallback path.
func TestServeOnceRoutePolicyFusionBandTarget(t *testing.T) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"a": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a", Model: "ma"}}},
		Fusion: map[string]FusionConfig{
			"hard": {Panel: []RouteTarget{{Provider: "a", Model: "ma"}, {Provider: "a", Model: "mb"}}, Synthesizer: RouteTarget{Provider: "a", Model: "ma"}},
		},
		RoutePolicies: map[string]RoutePolicy{
			"m": bandPolicy(configdomain.BandWhen{FollowUp: boolp(false)}, "fusion", "hard"),
		},
	}
	// The route must actually serve the band's target for it to apply.
	cfg.Routes["m"] = append(cfg.Routes["m"], RouteTarget{Provider: "fusion", Model: "hard"})
	snap := h.snapshot(cfg)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	res, st := serveOnceForPolicyTest(t, snap, h, body, false, "")
	if res.firstTried.Provider != "fusion" || res.firstTried.Model != "hard" {
		t.Fatalf("firstTried = %+v, want the band-selected fusion target first", res.firstTried)
	}
	if len(res.effectiveTargets) != 2 {
		t.Fatalf("effectiveTargets = %+v, want the concrete fallback kept", res.effectiveTargets)
	}
	if st.routing == nil || st.routing.Source != "band" || st.routing.Target != "fusion/hard" {
		t.Fatalf("fusion band routing = %+v, want source=band target=fusion/hard", st.routing)
	}
}

func gradePolicy(fallback string) RoutePolicy {
	return RoutePolicy{
		Grades: map[string][]RouteTarget{
			"fast":   {{Provider: "a", Model: "fast"}},
			"strong": {{Provider: "b", Model: "strong"}},
		},
		Bands: []configdomain.RouteBand{
			{When: configdomain.BandWhen{FollowUp: boolp(true)}, Grade: "fast"},
			{When: configdomain.BandWhen{EstimatedTokensMin: int64Ptr(1000)}, Grade: "strong"},
		},
		Fallback: fallback,
	}
}

func int64Ptr(v int64) *int64 { return &v }

func TestGroupTargetsByGradePreservesRouteOrder(t *testing.T) {
	grades := map[string][]RouteTarget{
		"fast":   {{Provider: "a", Model: "fast"}},
		"strong": {{Provider: "b", Model: "strong"}},
	}
	ordered := []RouteTarget{
		{Provider: "b", Model: "strong"},
		{Provider: "a", Model: "fast"},
	}
	groups, ungraded := groupTargetsByGrade(ordered, grades, nil)
	if len(groups) != 2 || groups[0].name != "strong" || groups[1].name != "fast" {
		t.Fatalf("grade order = %+v, want strong then fast", groups)
	}
	if len(ungraded) != 0 {
		t.Fatalf("unexpected ungraded: %+v", ungraded)
	}
	if len(groups[0].targets) != 1 || groups[0].targets[0].Provider != "b" {
		t.Fatalf("strong group = %+v", groups[0].targets)
	}
}

func TestGradeOrderFollowsRouteTargetOrder(t *testing.T) {
	// The grades map has no order; GradeOrder must derive it from the route's
	// target list so eval pairing and next_grade fallback are deterministic.
	grades := map[string][]RouteTarget{
		"cheap":  {{Provider: "c", Model: "cheap"}},
		"strong": {{Provider: "b", Model: "strong"}},
		"best":   {{Provider: "a", Model: "best"}},
	}
	ordered := []RouteTarget{
		{Provider: "a", Model: "best"},
		{Provider: "c", Model: "cheap"},
		{Provider: "b", Model: "strong"},
	}
	got := GradeOrder(ordered, grades, nil)
	want := []string{"best", "cheap", "strong"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GradeOrder = %v, want %v", got, want)
	}
	// A target declared in grades but absent from the route must not appear.
	partial := GradeOrder(ordered[:1], grades, nil)
	if !reflect.DeepEqual(partial, []string{"best"}) {
		t.Fatalf("GradeOrder over single-target route = %v, want [best]", partial)
	}
	if GradeOrder(ordered, nil, nil) != nil {
		t.Fatal("GradeOrder with no grades must return nil")
	}
}

func TestGroupTargetsByGradeHandlesUngraded(t *testing.T) {
	grades := map[string][]RouteTarget{
		"fast": {{Provider: "a", Model: "fast"}},
	}
	ordered := []RouteTarget{
		{Provider: "a", Model: "fast"},
		{Provider: "c", Model: "other"},
	}
	groups, ungraded := groupTargetsByGrade(ordered, grades, nil)
	if len(groups) != 1 || len(ungraded) != 1 || ungraded[0].Provider != "c" {
		t.Fatalf("groups=%+v ungraded=%+v", groups, ungraded)
	}
}

func TestBuildGradeOrderedFallbackModes(t *testing.T) {
	fast := gradeGroup{name: "fast", targets: []RouteTarget{{Provider: "a", Model: "fast"}}}
	strong := gradeGroup{name: "strong", targets: []RouteTarget{{Provider: "b", Model: "strong"}}}
	cheap := gradeGroup{name: "cheap", targets: []RouteTarget{{Provider: "c", Model: "cheap"}}}
	ungraded := []RouteTarget{{Provider: "d", Model: "fallback"}}
	groups := []gradeGroup{fast, strong, cheap}

	cases := []struct {
		name     string
		selected string
		fallback string
		want     []string
	}{
		{"no selection keeps order", "", "any", []string{"a/fast", "b/strong", "c/cheap", "d/fallback"}},
		{"any fallback", "fast", "any", []string{"a/fast", "b/strong", "c/cheap", "d/fallback"}},
		{"next_grade", "fast", "next_grade", []string{"a/fast", "b/strong", "d/fallback"}},
		{"strict", "fast", "strict", []string{"a/fast"}},
		{"next_grade last grade", "cheap", "next_grade", []string{"c/cheap", "d/fallback"}},
	}
	// selected grade absent from groups -> behave like no selection
	got := buildGradeOrdered("missing", groups, ungraded, "strict")
	want := []string{"a/fast", "b/strong", "c/cheap", "d/fallback"}
	var keys []string
	for _, x := range got {
		keys = append(keys, x.Provider+"/"+x.Model)
	}
	for i := range keys {
		if keys[i] != want[i] {
			t.Fatalf("missing grade: got %+v, want %+v", keys, want)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildGradeOrdered(tc.selected, groups, ungraded, tc.fallback)
			var keys []string
			for _, x := range got {
				keys = append(keys, x.Provider+"/"+x.Model)
			}
			if len(keys) != len(tc.want) {
				t.Fatalf("got %+v, want %+v", keys, tc.want)
			}
			for i := range keys {
				if keys[i] != tc.want[i] {
					t.Fatalf("got %+v, want %+v", keys, tc.want)
				}
			}
		})
	}
}

// routeGradeSnapshot builds a route with two grades and a band that picks the
// fast grade for follow-up turns.
func routeGradeSnapshot() (Snapshot, *harness) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"a": {}, "b": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a", Model: "fast"}, {Provider: "b", Model: "strong"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": gradePolicy("any"),
		},
	}
	return h.snapshot(cfg), h
}

func TestServeOnceGradePolicyPrefersBandGrade(t *testing.T) {
	snap, h := routeGradeSnapshot()
	followUp := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`

	res, st := serveOnceForPolicyTest(t, snap, h, followUp, false, "")
	if res.firstTried.Provider != "a" {
		t.Fatalf("firstTried = %+v, want the band-preferred fast grade (a)", res.firstTried)
	}
	if len(res.effectiveTargets) != 2 {
		t.Fatalf("effectiveTargets = %+v, want both grades kept as failover", res.effectiveTargets)
	}
	if st.routing == nil || st.routing.Source != "band" || st.routing.Grade != "fast" {
		t.Fatalf("grade band routing = %+v, want source=band grade=fast", st.routing)
	}

	firstTurn := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	res, st = serveOnceForPolicyTest(t, snap, h, firstTurn, false, "")
	if res.firstTried.Provider != "a" {
		t.Fatalf("firstTried = %+v, want the scheduled a/fast (no band matches)", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("no-match grade routing = %+v, want nil", st.routing)
	}
}

func TestServeOnceGradePolicyStrictFallback(t *testing.T) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"a": {}, "b": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a", Model: "fast"}, {Provider: "b", Model: "strong"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": gradePolicy("strict"),
		},
	}
	snap := h.snapshot(cfg)
	followUp := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`

	res, st := serveOnceForPolicyTest(t, snap, h, followUp, false, "")
	if len(res.effectiveTargets) != 1 || res.effectiveTargets[0].Provider != "a" {
		t.Fatalf("effectiveTargets = %+v, want only the selected grade", res.effectiveTargets)
	}
	if st.routing == nil || st.routing.Source != "band" || st.routing.Grade != "fast" {
		t.Fatalf("strict fallback routing = %+v, want source=band grade=fast", st.routing)
	}
}

func TestServeOnceGradePolicySkippedForHardSelection(t *testing.T) {
	followUp := `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"},{"role":"user","content":"more"}]}`

	snap, h := routeGradeSnapshot()
	res, st := serveOnceForPolicyTest(t, snap, h, followUp, true, "")
	if res.firstTried.Provider != "a" {
		t.Fatalf("pin/force: firstTried = %+v, want the scheduled a/fast", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("pin/force grade routing = %+v, want nil", st.routing)
	}

	snap, h = routeGradeSnapshot()
	res, st = serveOnceForPolicyTest(t, snap, h, followUp, false, "a")
	if res.firstTried.Provider != "a" {
		t.Fatalf("force-provider: firstTried = %+v, want the forced a/fast", res.firstTried)
	}
	if st.routing != nil {
		t.Fatalf("force-provider grade routing = %+v, want nil", st.routing)
	}
}

func TestServeOnceGradeLatchForcesGrade(t *testing.T) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"a": {}, "b": {}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "a", Model: "fast"}, {Provider: "b", Model: "strong"}}},
		RoutePolicies: map[string]RoutePolicy{
			"m": {
				Grades: map[string][]RouteTarget{
					"fast":   {{Provider: "a", Model: "fast"}},
					"strong": {{Provider: "b", Model: "strong"}},
				},
				Bands: []configdomain.RouteBand{
					{When: configdomain.BandWhen{FollowUp: boolp(true)}, Grade: "fast"},
				},
				Escalation: &configdomain.EscalationConfig{
					BadSignals:  []string{"upstream_error"},
					Consecutive: 1,
					Grade:       "strong",
					Dwell:       "30m",
				},
				Fallback: "strict",
			},
		},
	}
	snap := h.snapshot(cfg)
	h.state.SetLatch("sess-grade", "m", Latch{Target: "grade:strong", Since: time.Now(), BadRuns: 0}, snap.Generation)

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("x-claude-code-session-id", "sess-grade")
	p := pipeline{svc: h.svc, state: h.state}
	st := &serveState{}
	res := p.serveOnce(serveRequest{
		runtime: snap, proto: "openai", upPath: "/chat/completions", exposed: "m", calledModel: "m",
		targets: snap.Cfg.Routes["m"], routeKeys: map[string]bool{"m": true},
		sessionKey: "sess-grade",
		origBody:   []byte(body),
		writer:     httptest.NewRecorder(), request: r, requestID: "req-grade",
	}, st)
	if res.firstTried.Provider != "b" {
		t.Fatalf("firstTried = %+v, want latched strong grade (b)", res.firstTried)
	}
	if len(res.effectiveTargets) != 1 {
		t.Fatalf("strict fallback should leave only latched grade: %+v", res.effectiveTargets)
	}
	if st.routing == nil || st.routing.Source != "latch" || st.routing.Grade != "strong" || st.routing.Latch != "grade:strong" {
		t.Fatalf("latched grade routing = %+v, want source=latch grade=strong latch=grade:strong", st.routing)
	}
}

// TestBuildGradeRoutingDecisionEnforceGradeAbsentReportsFallback: the graded
// counterpart of the route-level reapply downgrade. When the selector's
// enforced grade has no representative in THIS round's filtered set (a cached
// choice re-applied after re-scheduling dropped the grade's targets),
// buildGradeOrdered cannot serve it — the decision must report fallback with
// the selector choice recorded but not enforced, instead of claiming
// Source=selector for a grade nothing was served from.
func TestBuildGradeRoutingDecisionEnforceGradeAbsentReportsFallback(t *testing.T) {
	policy := gradePolicy("any")
	fast := gradeGroup{name: "fast", targets: []RouteTarget{{Provider: "a", Model: "fast"}}}
	strong := gradeGroup{name: "strong", targets: []RouteTarget{{Provider: "b", Model: "strong"}}}
	enforced := gradeSelectorResult{
		mode: "enforce", action: "enforce", choiceID: "g1", choiceGrade: "strong",
		confidence: 0.9, difficulty: 2,
	}

	// Control: the enforced grade still has targets — honest selector
	// attribution.
	d := buildGradeRoutingDecision(policy, routing.Profile{}, []gradeGroup{fast, strong}, "", "", false, enforced, "strong")
	if d == nil || d.Source != "selector" || d.Grade != "strong" || d.Selector == nil || !d.Selector.Enforced || d.Selector.Choice != "g1" {
		t.Fatalf("served-grade decision = %+v, want source=selector grade=strong enforced choice=g1", d)
	}

	// Retry round: the grade's group is absent from the re-scheduled set.
	d = buildGradeRoutingDecision(policy, routing.Profile{}, []gradeGroup{fast}, "", "", false, enforced, "strong")
	if d == nil || d.Source != "fallback" || d.Grade != "" || d.Selector == nil || d.Selector.Enforced || d.Selector.Choice != "g1" {
		t.Fatalf("absent-grade decision = %+v, want source=fallback, no grade, selector choice recorded but not enforced", d)
	}

	// Same for a group that exists but was filtered down to zero targets.
	strongEmpty := gradeGroup{name: "strong"}
	d = buildGradeRoutingDecision(policy, routing.Profile{}, []gradeGroup{fast, strongEmpty}, "", "", false, enforced, "strong")
	if d == nil || d.Source != "fallback" || d.Grade != "" || d.Selector == nil || d.Selector.Enforced {
		t.Fatalf("empty-grade decision = %+v, want source=fallback selector not enforced", d)
	}
}

// TestRouteSelectorResultReapplyDowngradesWhenPreferredAbsent: re-apply a
// cached enforce choice to a later wait-retry round's ordered set — when the
// preferred target was scheduled out (e.g. cooling), the action downgrades to
// fallback so the decision record stays honest about which step determined
// the order.
func TestRouteSelectorResultReapplyDowngradesWhenPreferredAbsent(t *testing.T) {
	preferred := RouteTarget{Provider: "b", Model: "mb"}
	res := routeSelectorResult{mode: "enforce", action: "enforce", preferred: preferred, choice: "c1", confidence: 0.9}

	// Preferred still present: re-applied to the front, still enforce.
	got := res.reapply([]RouteTarget{{Provider: "a", Model: "ma"}, preferred}, nil)
	if got.action != "enforce" || len(got.ordered) != 2 || got.ordered[0].Provider != "b" {
		t.Fatalf("reapply with preferred present = %+v, want enforce with b first", got)
	}

	// Preferred scheduled out: downgrade to fallback, order kept.
	got = res.reapply([]RouteTarget{{Provider: "a", Model: "ma"}}, nil)
	if got.action != "fallback" || len(got.ordered) != 1 || got.ordered[0].Provider != "a" {
		t.Fatalf("reapply with preferred absent = %+v, want fallback with the natural order kept", got)
	}
	d := got.routingDecision(nil)
	if d == nil || d.Source != "fallback" || d.Selector == nil || d.Selector.Enforced || d.Selector.Choice != "c1" {
		t.Fatalf("downgraded decision = %+v, want source=fallback selector choice recorded but not enforced", d)
	}
}
