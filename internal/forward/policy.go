package forward

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/routing"
)

// policy.go applies the route-tier policy (route_policy: bands + escalation
// latch + selector) to the scheduled target order. Bands are pure functions
// over the request profile; the escalation latch is session-scoped state owned
// by the runtime Manager; the selector makes a per-request decisions-model
// choice. See docs/architecture/request-routing.md.

// applyRoutePolicy returns ordered with the band-selected target preferred. It
// is a no-op when no band matches or when the preferred target is not part of
// the (possibly capability-filtered) order: the policy must never leave the
// pipeline with zero targets. When a band matches, the returned decision
// records the source and the target that was moved to the front.
func applyRoutePolicy(
	ordered []RouteTarget,
	policy RoutePolicy,
	profile routing.Profile,
	parentOf map[string]string,
) ([]RouteTarget, *configdomain.RoutingDecision) {
	want, ok := routing.PickBand(policy, profile)
	if !ok {
		return ordered, nil
	}
	decision := &configdomain.RoutingDecision{
		Source: "band",
		Target: fmt.Sprintf("%s/%s", want.Provider, want.Model),
	}
	return routing.PreferTarget(ordered, want, parentOf), decision
}

// applyRouteLatch returns ordered with an active session latch target moved to
// the front. An expired latch is ignored (the next outcome recording will
// overwrite it). The latch is keyed by (sessionKey, route) and its target must
// actually be present in the current ordered set; when capability narrowing or
// an operator disable has filtered it out, the latch does not apply
// (latched=false, no decision) so bands and the selector still run — the same
// strictness resolveLatchGrade uses for unresolvable targets. Latch wins over
// bands and selector: when a latch applies the later policy steps are skipped
// so the escalated target stays first. The returned decision records the latch
// source and value.
func applyRouteLatch(
	state RouteState,
	sessionKey, route string,
	ordered []RouteTarget,
	policy RoutePolicy,
	parentOf map[string]string,
	now time.Time,
) ([]RouteTarget, *configdomain.RoutingDecision, bool) {
	if sessionKey == "" || policy.Escalation == nil {
		return ordered, nil, false
	}
	latch, ok := state.LatchValue(sessionKey, route)
	if !ok {
		return ordered, nil, false
	}
	if now.Sub(latch.Since) > policy.Escalation.DwellDuration() {
		return ordered, nil, false
	}
	parts := strings.SplitN(latch.Target, "/", 2)
	if len(parts) != 2 {
		return ordered, nil, false
	}
	want := RouteTarget{Provider: parts[0], Model: parts[1]}
	if routing.TargetIndex(ordered, want, parentOf) < 0 {
		return ordered, nil, false
	}
	decision := &configdomain.RoutingDecision{
		Source: "latch",
		Target: latch.Target,
		Latch:  latch.Target,
	}
	return routing.PreferTarget(ordered, want, parentOf), decision, true
}

// routeSelectorResult carries the outcome of a route-level selector decision
// for observability; the ordered slice is always usable (fail-open).
type routeSelectorResult struct {
	ordered    []RouteTarget
	preferred  RouteTarget // the enforce-mode pick, valid when action == "enforce"
	mode       string
	action     string
	choice     string
	confidence float64
	difficulty float64
	latencyMs  int64
	err        string
}

// reapply projects a cached selector outcome onto a later wait-retry round's
// ordered set (which may have been re-scheduled since the decisions call): the
// enforce preference is re-applied when the preferred target is still present;
// when it was scheduled out (e.g. cooling), the order is kept and the action
// downgrades to fallback so the decision record stays honest about which step
// determined the order. Non-enforce outcomes pass the order through.
func (r routeSelectorResult) reapply(ordered []RouteTarget, parentOf map[string]string) routeSelectorResult {
	r.ordered = ordered
	if r.action != "enforce" {
		return r
	}
	if routing.TargetIndex(ordered, r.preferred, parentOf) < 0 {
		r.action = "fallback"
		return r
	}
	r.ordered = routing.PreferTarget(ordered, r.preferred, parentOf)
	return r
}

// routingDecision merges the selector result with the base decision from
// bands/latch. The selector object is always recorded when the selector ran
// (including shadow mode) so downstream reports can measure shadow accuracy.
// Source reflects the step that actually determined the target order: selector
// when enforce won, otherwise the base source (band/latch) or "fallback" when
// no earlier step matched.
func (r routeSelectorResult) routingDecision(base *configdomain.RoutingDecision) *configdomain.RoutingDecision {
	if r.mode == "off" {
		return base
	}
	selector := &configdomain.SelectorChoice{
		Choice:     r.choice,
		Confidence: r.confidence,
		Difficulty: r.difficulty,
		Enforced:   r.action == "enforce",
	}
	if r.err != "" {
		selector.Err = r.err
	}
	if r.action == "enforce" {
		target := ""
		if len(r.ordered) > 0 {
			target = fmt.Sprintf("%s/%s", r.ordered[0].Provider, r.ordered[0].Model)
		}
		return &configdomain.RoutingDecision{
			Source:   "selector",
			Target:   target,
			Selector: selector,
		}
	}
	if base != nil {
		base.Selector = selector
		return base
	}
	return &configdomain.RoutingDecision{
		Source:   "fallback",
		Selector: selector,
	}
}

// gradeSelectorResult carries the outcome of a grade-level selector decision.
type gradeSelectorResult struct {
	choiceID    string // raw candidate id from the decisions response (g0..gn)
	choiceGrade string // resolved grade name, empty if the id is unknown
	mode        string
	action      string
	confidence  float64
	difficulty  float64
	latencyMs   int64
	err         string
}

// routingDecision merges the grade selector result with the base decision from
// bands/latch. The selector object is always recorded when the selector ran.
func (r gradeSelectorResult) routingDecision(base *configdomain.RoutingDecision, selectedGrade string) *configdomain.RoutingDecision {
	if r.mode == "off" {
		return base
	}
	selector := &configdomain.SelectorChoice{
		Choice:     r.choiceID,
		Confidence: r.confidence,
		Difficulty: r.difficulty,
		Enforced:   r.action == "enforce",
	}
	if r.err != "" {
		selector.Err = r.err
	}
	if r.action == "enforce" {
		return &configdomain.RoutingDecision{
			Source:   "selector",
			Grade:    selectedGrade,
			Selector: selector,
		}
	}
	if base != nil {
		base.Selector = selector
		return base
	}
	return &configdomain.RoutingDecision{
		Source:   "fallback",
		Selector: selector,
	}
}

// gradeGroup is one grade's slice of targets after per-grade capability/context
// filtering, plus the representative target used for selector rubrics.
type gradeGroup struct {
	name    string
	targets []RouteTarget
	first   RouteTarget
	index   int
}

// resolveLatchGrade returns the grade name latched for this session on this
// route and the raw latch value, if any. For grade-based escalation the latch
// target may be encoded as "grade:<name>" (see recordLatchOutcome); otherwise
// it is resolved through the policy's grade declarations. An expired or
// missing latch returns ("", "", false).
func resolveLatchGrade(state RouteState, sessionKey, route string, policy RoutePolicy, now time.Time) (grade string, latchValue string, ok bool) {
	if sessionKey == "" || policy.Escalation == nil || !policy.HasGrades() {
		return "", "", false
	}
	latch, found := state.LatchValue(sessionKey, route)
	if !found {
		return "", "", false
	}
	if now.Sub(latch.Since) > policy.Escalation.DwellDuration() {
		return "", "", false
	}
	if strings.HasPrefix(latch.Target, "grade:") {
		g := strings.TrimPrefix(latch.Target, "grade:")
		if _, exists := policy.Grades[g]; exists {
			return g, latch.Target, true
		}
		return "", "", false
	}
	parts := strings.SplitN(latch.Target, "/", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	want := RouteTarget{Provider: parts[0], Model: parts[1]}
	g, ambiguous := routing.GradeForTarget(want, policy.Grades)
	if ambiguous || g == "" {
		return "", "", false
	}
	return g, latch.Target, true
}

// gradeOfTarget returns the first grade in policy.Grades that contains t,
// considering pooled virtual targets through parentOf. Targets that belong to
// no grade are ungraded. Ambiguous targets (present in multiple grades) are
// assigned to the first grade encountered in the caller's grade order.
func gradeOfTarget(t RouteTarget, grades map[string][]RouteTarget, parentOf map[string]string) string {
	for name, targets := range grades {
		for _, gt := range targets {
			if t.Model != gt.Model {
				continue
			}
			if t.Provider == gt.Provider || parentOf[t.Provider] == gt.Provider {
				return name
			}
		}
	}
	return ""
}

// GradeOrder returns a route's effective grade order: the first appearance of
// each grade when scanning its ordered targets. Graded routes use this order
// for "next_grade" fallback, and eval pairing ("opposite") follows it too, so
// selection and measurement agree on which grade is "next". Targets not declared
// in any grade are ignored (they are returned separately as a safety net).
//
// The order is derived from the route's target list, not from the grades map:
// Go maps do not preserve YAML declaration order, so scanning a map would be
// nondeterministic (and could change between calls in one process).
func GradeOrder(ordered []RouteTarget, grades map[string][]RouteTarget, parentOf map[string]string) []string {
	if len(grades) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(grades))
	var order []string
	for _, t := range ordered {
		g := gradeOfTarget(t, grades, parentOf)
		if g == "" || seen[g] {
			continue
		}
		seen[g] = true
		order = append(order, g)
	}
	return order
}

// groupTargetsByGrade partitions ordered into grade groups. Grade order is
// derived from the first appearance of each grade in ordered; ungraded targets
// (not declared in any grade) keep their relative order and are returned
// separately so the caller can append them as a final safety net.
func groupTargetsByGrade(ordered []RouteTarget, grades map[string][]RouteTarget, parentOf map[string]string) (groups []gradeGroup, ungraded []RouteTarget) {
	if len(grades) == 0 {
		return nil, ordered
	}
	targetsByGrade := make(map[string][]RouteTarget, len(grades))
	for _, t := range ordered {
		g := gradeOfTarget(t, grades, parentOf)
		if g == "" {
			ungraded = append(ungraded, t)
			continue
		}
		targetsByGrade[g] = append(targetsByGrade[g], t)
	}
	order := GradeOrder(ordered, grades, parentOf)
	groups = make([]gradeGroup, 0, len(order))
	for i, name := range order {
		gt := targetsByGrade[name]
		groups = append(groups, gradeGroup{
			name:    name,
			targets: gt,
			first:   gt[0],
			index:   i,
		})
	}
	return groups, ungraded
}

// buildGradeOrdered assembles the final target order for a graded route.
// selected is the grade chosen by latch/selector/band; an empty selected means
// "no explicit preference" and the route's natural (filtered) order is kept.
// Fallback modes only apply after an explicit selection:
//   - "any": append every other grade in grade order, then ungraded targets.
//   - "next_grade": append only the grade immediately following the selected
//     grade in grade order, then ungraded targets.
//   - "strict": append nothing (selected grade only).
func buildGradeOrdered(selected string, groups []gradeGroup, ungraded []RouteTarget, fallback string) []RouteTarget {
	if selected == "" {
		out := make([]RouteTarget, 0, len(ungraded))
		for _, g := range groups {
			out = append(out, g.targets...)
		}
		out = append(out, ungraded...)
		return out
	}
	var selectedIdx = -1
	var selectedTargets []RouteTarget
	for i, g := range groups {
		if g.name == selected {
			selectedIdx = i
			selectedTargets = g.targets
			break
		}
	}
	// Selected grade has no representatives in the current ordered set (e.g.
	// disabled by operator or narrowed away by force-provider). Treat it like
	// "no explicit preference" so the route still has viable failover targets.
	if selectedIdx < 0 {
		out := make([]RouteTarget, 0, len(ungraded))
		for _, g := range groups {
			out = append(out, g.targets...)
		}
		out = append(out, ungraded...)
		return out
	}
	out := make([]RouteTarget, 0, len(selectedTargets))
	out = append(out, selectedTargets...)
	switch fallback {
	case "next_grade":
		if selectedIdx+1 < len(groups) {
			out = append(out, groups[selectedIdx+1].targets...)
		}
		out = append(out, ungraded...)
	case "strict":
		// no fallback
	default: // "any"
		for i := selectedIdx + 1; i < len(groups); i++ {
			out = append(out, groups[i].targets...)
		}
		out = append(out, ungraded...)
	}
	return out
}

// applyRoutePolicyGrades runs the grade-based route_policy path: group targets
// by grade, filter each grade independently, resolve latch/selector/band to a
// preferred grade, and assemble the final order according to the fallback mode.
func (p pipeline) applyRoutePolicyGrades(
	ordered []RouteTarget,
	policy RoutePolicy,
	st *serveState,
	origBody []byte,
	sessionKey string,
	ctx context.Context,
	proto string,
	runtime Snapshot,
	clientSession string,
	requestID string,
	agent string,
	exposed string,
	calledModel string,
) ([]RouteTarget, *configdomain.RoutingDecision) {
	parentOf := runtime.ParentOf
	profile := p.rawProfile(st, origBody)
	now := time.Now()

	groups, ungraded := groupTargetsByGrade(ordered, policy.Grades, parentOf)
	if len(groups) == 0 {
		return ordered, nil
	}

	filtered := make([]gradeGroup, len(groups))
	for i, g := range groups {
		ft := filterGradeTargets(g.targets, runtime.Cfg, parentOf, runtime.Catalog, profile)
		filtered[i] = gradeGroup{
			name:    g.name,
			targets: ft,
			first:   g.first,
			index:   g.index,
		}
		if len(ft) > 0 {
			filtered[i].first = ft[0]
		}
	}

	latchedGrade, latchValue, latched := resolveLatchGrade(p.state, sessionKey, exposed, policy, now)
	// Same strictness as the non-graded latch (applyRouteLatch's TargetIndex
	// check): a session latched to a grade whose every target left the current
	// filtered set (operator disable, capability/profile filtering) is inert —
	// buildGradeOrdered already falls back to the natural order, and an inert
	// latch must not suppress the selector or claim source=latch for an order
	// it did not produce. All three values clear together: SelectGrade ranks a
	// non-empty latchedGrade above the selector choice.
	if latched && !gradeGroupHasTargets(filtered, latchedGrade) {
		latched, latchedGrade, latchValue = false, "", ""
	}

	selectorRes := gradeSelectorResult{mode: "off", action: "none"}
	var selectorChoice string
	if policy.Selector != nil && !latched {
		// At most one decisions call per request: later wait-retry rounds reuse
		// the cached grade choice. buildGradeOrdered already tolerates a
		// selected grade with no representatives in the re-scheduled set, so the
		// cached choice is safe to re-apply as-is.
		if st.selectorComputed {
			selectorRes = st.gradeSel
		} else {
			selectorRes = p.applyRouteSelectorForGrades(ctx, filtered, policy, profile, origBody, proto, runtime, sessionKey, clientSession, requestID, agent, exposed, calledModel)
			st.gradeSel = selectorRes
			st.selectorComputed = true
		}
		if selectorRes.action == "enforce" {
			selectorChoice = selectorRes.choiceGrade
		}
	}

	selected, _ := routing.SelectGrade(policy, profile, latchedGrade, selectorChoice)

	decision := buildGradeRoutingDecision(policy, profile, filtered, latchedGrade, latchValue, latched, selectorRes, selected)

	return buildGradeOrdered(selected, filtered, ungraded, policy.FallbackMode()), decision
}

// buildGradeRoutingDecision determines the routing decision metadata for a
// graded route. It respects precedence (latch > selector enforce > band) and
// records a selector object whenever the selector ran, even in shadow mode.
func buildGradeRoutingDecision(
	policy RoutePolicy,
	profile routing.Profile,
	filtered []gradeGroup,
	latchedGrade string,
	latchValue string,
	latched bool,
	selectorRes gradeSelectorResult,
	selected string,
) *configdomain.RoutingDecision {
	if latched {
		return &configdomain.RoutingDecision{
			Source: "latch",
			Grade:  latchedGrade,
			Latch:  latchValue,
		}
	}
	if selectorRes.action == "enforce" {
		// The enforced grade may have NO representative in this round's
		// filtered set: the cached choice is re-applied to a re-scheduled
		// ordered set on wait-retry rounds (the grade's targets may all have
		// been scheduled out), and buildGradeOrdered then falls back to the
		// natural order. Report that honestly instead of attributing the
		// served order to a grade nothing came from — the graded counterpart
		// of routeSelectorResult.reapply's fallback downgrade.
		if gradeGroupHasTargets(filtered, selected) {
			return selectorRes.routingDecision(nil, selected)
		}
		selectorRes.action = "fallback"
	}

	// Determine whether a band matched on its own (without latch/selector).
	bandGrade, bandMatched := routing.SelectGrade(policy, profile, "", "")
	var base *configdomain.RoutingDecision
	if bandMatched {
		base = &configdomain.RoutingDecision{
			Source: "band",
			Grade:  bandGrade,
		}
	}

	if selectorRes.mode != "off" {
		return selectorRes.routingDecision(base, selected)
	}
	return base
}

// gradeGroupHasTargets reports whether name is one of the filtered grade
// groups and still holds at least one target (an empty grade group means the
// grade has no representative in the current ordered set).
func gradeGroupHasTargets(groups []gradeGroup, name string) bool {
	for _, g := range groups {
		if g.name == name {
			return len(g.targets) > 0
		}
	}
	return false
}

// filterGradeTargets keeps only targets that fit the request profile, scoped to
// the grade itself. Unlike Planner.ApplyWithProfile it never falls back to a
// cross-route pool — an empty grade stays empty so the fallback mode can decide
// what to try next.
func filterGradeTargets(targets []RouteTarget, cfg *Config, parentOf map[string]string, cat *catalog.Catalog, profile routing.Profile) []RouteTarget {
	out := make([]RouteTarget, 0, len(targets))
	for _, t := range targets {
		if t.Provider == configdomain.FusionProvider {
			out = append(out, t)
			continue
		}
		caps := routing.CapabilitiesFor(cfg, parentOf, t)
		if routing.Fits(cat, caps, t.Model, profile) {
			out = append(out, t)
		}
	}
	return out
}

// routeSelector applies the route-tier selector at most once per request: the
// first serveOnce pass pays the decisions call and caches the outcome in
// serveState; later wait-retry rounds re-apply the cached preference to their
// (possibly re-scheduled) ordered set via reapply instead of calling the
// decisions model again. This keeps the "at most one decisions call per
// request" contract the same way rawProfile caches the body scan.
func (p pipeline) routeSelector(
	st *serveState,
	ctx context.Context,
	ordered []RouteTarget,
	policy RoutePolicy,
	profile routing.Profile,
	parentOf map[string]string,
	body []byte,
	proto string,
	runtime Snapshot,
	sessionKey string,
	clientSession string,
	requestID string,
	agent string,
	exposed string,
	calledModel string,
) routeSelectorResult {
	if st.selectorComputed {
		return st.routeSel.reapply(ordered, parentOf)
	}
	res := p.applyRouteSelector(ctx, ordered, policy, profile, parentOf, body, proto, runtime, sessionKey, clientSession, requestID, agent, exposed, calledModel)
	st.routeSel = res
	st.selectorComputed = true
	return res
}

// applyRouteSelector asks the route_policy selector (when configured) to pick
// one target from ordered. In enforce mode with a confident choice, the chosen
// target is moved to the front via routing.PreferTarget; shadow mode and every
// failure fall back to the original order. Callers must go through
// routeSelector so the decisions call is made at most once per request.
func (p pipeline) applyRouteSelector(
	ctx context.Context,
	ordered []RouteTarget,
	policy RoutePolicy,
	profile routing.Profile,
	parentOf map[string]string,
	body []byte,
	proto string,
	runtime Snapshot,
	sessionKey string,
	clientSession string,
	requestID string,
	agent string,
	exposed string,
	calledModel string,
) routeSelectorResult {
	res := routeSelectorResult{ordered: ordered, mode: "off", action: "none"}
	sel := policy.Selector
	if sel == nil {
		return res
	}
	res.mode = sel.SelectorMode()

	candidates := make([]fusion.SelectorCandidate, 0, len(ordered))
	for i, t := range ordered {
		candidates = append(candidates, fusion.SelectorCandidate{
			Target: t,
			ID:     "c" + strconv.Itoa(i),
			Rubric: t.Rubric,
		})
	}

	state := fusion.BuildSelectorState(body, proto, fusion.RequestFacts{
		HasImage:        profile.HasImage,
		EstimatedTokens: profile.EstimatedTokens,
	}, profile.HasTools, candidates)

	choiceInstructions := sel.Instruction
	if choiceInstructions == "" {
		choiceInstructions = fusion.DefaultSelectorChoiceInstructions
	}
	difficultyInstructions := sel.DifficultyInstruction
	if difficultyInstructions == "" {
		difficultyInstructions = fusion.DefaultSelectorDifficultyInstructions
	}

	out := p.callDecisions(ctx, decisionsInput{
		Runtime:                runtime,
		Target:                 sel.Target,
		SessionKey:             sessionKey,
		SessionID:              clientSession,
		RequestID:              requestID,
		Kind:                   "route-select",
		Agent:                  agent,
		Proto:                  proto,
		Exposed:                exposed,
		CalledModel:            calledModel,
		Timeout:                sel.TimeoutDuration(),
		State:                  state,
		Candidates:             candidates,
		ChoiceInstructions:     choiceInstructions,
		DifficultyInstructions: difficultyInstructions,
		DifficultyLevels:       fusion.DefaultSelectorDifficultyLevels,
	})

	res.choice = out.ChoiceID
	res.confidence = out.Confidence
	res.difficulty = out.Difficulty
	res.latencyMs = out.LatencyMs
	if out.Err != nil {
		res.err = out.Err.Error()
		res.action = "fallback"
		logx.Debugf("[route_policy selector route=%s] decisions call failed: %v", exposed, out.Err)
		return res
	}

	if p.svc.Metrics != nil {
		p.svc.Metrics.Inc("routing", "selector", counters.EvRoutingSelectorObserved)
	}

	if res.mode == "shadow" {
		res.action = "shadow"
		logx.Debugf("[route_policy selector route=%s] shadow choice=%s confidence=%.2f difficulty=%.1f latency=%dms",
			exposed, res.choice, res.confidence, res.difficulty, res.latencyMs)
		return res
	}

	threshold := sel.ConfidenceThreshold()
	if res.confidence < threshold {
		res.action = "fallback"
		logx.Debugf("[route_policy selector route=%s] confidence %.2f below threshold %.2f", exposed, res.confidence, threshold)
		return res
	}

	idx := -1
	for i, c := range candidates {
		if c.ID == res.choice {
			idx = i
			break
		}
	}
	if idx < 0 {
		res.action = "fallback"
		res.err = "unknown choice " + res.choice
		logx.Debugf("[route_policy selector route=%s] unknown choice %q", exposed, res.choice)
		return res
	}

	res.action = "enforce"
	preferred := ordered[idx]
	res.preferred = preferred
	res.ordered = routing.PreferTarget(ordered, preferred, parentOf)
	logx.Debugf("[route_policy selector route=%s] enforce choice=%s confidence=%.2f difficulty=%.1f latency=%dms -> prefer %s/%s",
		exposed, res.choice, res.confidence, res.difficulty, res.latencyMs, preferred.Provider, preferred.Model)
	return res
}

// applyRouteSelectorForGrades asks the route_policy selector to pick one grade
// from the non-empty filtered groups. It mirrors applyRouteSelector but works
// with grade-level candidates (ids g0..gn) and returns the chosen grade name.
func (p pipeline) applyRouteSelectorForGrades(
	ctx context.Context,
	groups []gradeGroup,
	policy RoutePolicy,
	profile routing.Profile,
	body []byte,
	proto string,
	runtime Snapshot,
	sessionKey string,
	clientSession string,
	requestID string,
	agent string,
	exposed string,
	calledModel string,
) gradeSelectorResult {
	res := gradeSelectorResult{mode: "off", action: "none"}
	sel := policy.Selector
	if sel == nil {
		return res
	}
	res.mode = sel.SelectorMode()

	// Only offer grades that still have targets after request-aware filtering.
	viable := make([]gradeGroup, 0, len(groups))
	for _, g := range groups {
		if len(g.targets) > 0 {
			viable = append(viable, g)
		}
	}
	if len(viable) == 0 {
		res.action = "fallback"
		res.err = "no viable grades"
		return res
	}

	candidates := make([]fusion.SelectorCandidate, 0, len(viable))
	idToGrade := make(map[string]string, len(viable))
	for i, g := range viable {
		id := "g" + strconv.Itoa(i)
		candidates = append(candidates, fusion.SelectorCandidate{
			Target: g.first,
			ID:     id,
			Rubric: "grade " + g.name,
		})
		idToGrade[id] = g.name
	}

	state := fusion.BuildSelectorState(body, proto, fusion.RequestFacts{
		HasImage:        profile.HasImage,
		EstimatedTokens: profile.EstimatedTokens,
	}, profile.HasTools, candidates)

	choiceInstructions := sel.Instruction
	if choiceInstructions == "" {
		choiceInstructions = fusion.DefaultSelectorChoiceInstructions
	}
	difficultyInstructions := sel.DifficultyInstruction
	if difficultyInstructions == "" {
		difficultyInstructions = fusion.DefaultSelectorDifficultyInstructions
	}

	out := p.callDecisions(ctx, decisionsInput{
		Runtime:                runtime,
		Target:                 sel.Target,
		SessionKey:             sessionKey,
		SessionID:              clientSession,
		RequestID:              requestID,
		Kind:                   "route-select",
		Agent:                  agent,
		Proto:                  proto,
		Exposed:                exposed,
		CalledModel:            calledModel,
		Timeout:                sel.TimeoutDuration(),
		State:                  state,
		Candidates:             candidates,
		ChoiceInstructions:     choiceInstructions,
		DifficultyInstructions: difficultyInstructions,
		DifficultyLevels:       fusion.DefaultSelectorDifficultyLevels,
	})

	res.choiceID = out.ChoiceID
	res.choiceGrade = idToGrade[out.ChoiceID]
	res.confidence = out.Confidence
	res.difficulty = out.Difficulty
	res.latencyMs = out.LatencyMs
	if out.Err != nil {
		res.err = out.Err.Error()
		res.action = "fallback"
		logx.Debugf("[route_policy selector route=%s grades] decisions call failed: %v", exposed, out.Err)
		return res
	}

	if p.svc.Metrics != nil {
		p.svc.Metrics.Inc("routing", "selector", counters.EvRoutingSelectorObserved)
	}

	if res.mode == "shadow" {
		res.action = "shadow"
		logx.Debugf("[route_policy selector route=%s grades] shadow choice=%s confidence=%.2f difficulty=%.1f latency=%dms",
			exposed, res.choiceGrade, res.confidence, res.difficulty, res.latencyMs)
		return res
	}

	threshold := sel.ConfidenceThreshold()
	if res.confidence < threshold {
		res.action = "fallback"
		logx.Debugf("[route_policy selector route=%s grades] confidence %.2f below threshold %.2f", exposed, res.confidence, threshold)
		return res
	}

	if res.choiceGrade == "" {
		res.action = "fallback"
		res.err = "unknown choice " + out.ChoiceID
		logx.Debugf("[route_policy selector route=%s grades] unknown choice %q", exposed, out.ChoiceID)
		return res
	}

	res.action = "enforce"
	logx.Debugf("[route_policy selector route=%s grades] enforce choice=%s confidence=%.2f difficulty=%.1f latency=%dms -> grade %s",
		exposed, res.choiceGrade, res.confidence, res.difficulty, res.latencyMs, res.choiceGrade)
	return res
}
