package runtime

import (
	"reflect"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// loadBalanceInput builds a committed load_balance ScheduleInput over the
// given providers (input order = rotation order).
func loadBalanceInput(exposed string, now time.Time, providers ...string) ScheduleInput {
	targets := make([]Target, 0, len(providers))
	for _, name := range providers {
		targets = append(targets, Target{Provider: name, Model: "m"})
	}
	return ScheduleInput{
		Exposed:     exposed,
		Strategy:    configdomain.RouteStrategyLoadBalance,
		Targets:     targets,
		RouteKeys:   map[string]bool{exposed: true},
		Dwell:       time.Hour,
		Now:         now,
		QuotaMaxAge: time.Hour,
		Commit:      true,
		Generation:  1,
	}
}

// TestDecideOrderLoadBalanceRotatesEvenly — SESSIONLESS requests rotate per
// commit in input order; no sticky is written for them.
func TestDecideOrderLoadBalanceRotatesEvenly(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	input := loadBalanceInput("m", now, "a", "b", "c")

	// Four committed schedules over three targets: a, b, c, a — even
	// rotation in input order, regardless of quota/priority/tier.
	for round, want := range []string{"a", "b", "c", "a"} {
		result := m.DecideOrder(input)
		if len(result.Order) == 0 {
			t.Fatalf("round %d: empty order", round)
		}
		head := input.Targets[result.Order[0]].Provider
		if head != want {
			t.Fatalf("round %d head = %q, want %q (order=%v)", round, head, want, result.Order)
		}
		// The full failover chain is the remaining rotation, not a ranking.
		if !reflect.DeepEqual(result.Order, []int{round % 3, (round%3 + 1) % 3, (round%3 + 2) % 3}) {
			t.Fatalf("round %d order = %v, want rotation", round, result.Order)
		}
		// Sessionless: no sticky write.
		if result.StickyProvider != "" {
			t.Fatalf("round %d StickyProvider = %q, want empty", round, result.StickyProvider)
		}
	}
}

// TestDecideOrderLoadBalanceIgnoresQuotaRanking: tier/priority/surplus facts
// that would rank c >> b >> a under quota change nothing — the rotation walks
// the input order (sessionless, so per request).
func TestDecideOrderLoadBalanceIgnoresQuotaRanking(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	setScheduleQuota(t, m, "a", scheduleQuota(provider.BillingPayG, .01, now), 1)
	setScheduleQuota(t, m, "b", scheduleQuota(provider.BillingPlan, .5, now), 1)
	setScheduleQuota(t, m, "c", scheduleQuota(provider.BillingPlan, 1, now), 1)

	input := loadBalanceInput("m", now, "a", "b", "c")
	var heads []string
	for i := 0; i < 3; i++ {
		result := m.DecideOrder(input)
		heads = append(heads, input.Targets[result.Order[0]].Provider)
	}
	if !reflect.DeepEqual(heads, []string{"a", "b", "c"}) {
		t.Fatalf("heads = %v, want input-order rotation [a b c]", heads)
	}
}

// TestDecideOrderLoadBalanceSessionAssignsByRotation: each NEW session takes
// the next rotation slot (the counter advances once per assignment) and then
// parks on it — prompt-cache affinity — while later sessions keep spreading.
func TestDecideOrderLoadBalanceSessionAssignsByRotation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	input := loadBalanceInput("m", now, "a", "b", "c")

	for i, session := range []string{"s1", "s2", "s3"} {
		want := []string{"a", "b", "c"}[i]
		first := input
		first.SessionKey = session
		result := m.DecideOrder(first)
		if head := input.Targets[result.Order[0]].Provider; head != want {
			t.Fatalf("session %s first head = %q, want %q", session, head, want)
		}
		if result.StickyProvider != want {
			t.Fatalf("session %s StickyProvider = %q, want %q (caller commits sticky)", session, result.StickyProvider, want)
		}
		// The session's next request keeps its provider (sticky was committed
		// by the caller — mirror the production SetSticky refresh here).
		m.SetSticky(session, Sticky{Provider: want, Since: now}, 1)
		result = m.DecideOrder(first)
		if head := input.Targets[result.Order[0]].Provider; head != want {
			t.Fatalf("session %s second head = %q, want sticky %q", session, head, want)
		}
		if result.StickyProvider != want {
			t.Fatalf("session %s second StickyProvider = %q, want %q (refresh)", session, result.StickyProvider, want)
		}
	}
}

// TestDecideOrderLoadBalanceSessionKeepsStickyDespiteBetterQuota: session
// affinity beats quota ranking — a session parked on the WORST-ranked provider
// (payg, least surplus) stays there while better providers sit idle.
func TestDecideOrderLoadBalanceSessionKeepsStickyDespiteBetterQuota(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	setScheduleQuota(t, m, "a", scheduleQuota(provider.BillingPayG, .01, now), 1)
	setScheduleQuota(t, m, "b", scheduleQuota(provider.BillingPlan, .5, now), 1)
	setScheduleQuota(t, m, "c", scheduleQuota(provider.BillingPlan, 1, now), 1)
	m.SetSticky("session", Sticky{Provider: "a", Since: now}, 1)

	input := loadBalanceInput("m", now, "a", "b", "c")
	input.SessionKey = "session"
	for i := 0; i < 3; i++ {
		result := m.DecideOrder(input)
		if head := input.Targets[result.Order[0]].Provider; head != "a" {
			t.Fatalf("round %d head = %q, want sticky a", i, head)
		}
		if result.StickyProvider != "a" {
			t.Fatalf("round %d StickyProvider = %q, want a", i, result.StickyProvider)
		}
	}
	// The sticky entry itself is untouched by scheduling (only the caller's
	// SetSticky refreshes it) and is never deleted while the session is active.
	if current, ok := m.Sticky("session"); !ok || current.Provider != "a" {
		t.Fatalf("sticky entry = %+v (ok=%v), want provider a", current, ok)
	}
}

// TestDecideOrderLoadBalanceSessionReassignsWhenStickyUnavailable: the parked
// provider leaving the available set (here: rate-limited) re-assigns the
// session to the next rotation slot instead of parking on a dead target.
func TestDecideOrderLoadBalanceSessionReassignsWhenStickyUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	m.SetSticky("session", Sticky{Provider: "b", Since: now}, 1)
	// b goes down after the session parked on it.
	m.RecordRateLimit("b", now.Add(time.Hour), RateLimitTransient, 1)

	input := loadBalanceInput("m", now, "a", "b", "c")
	input.SessionKey = "session"
	result := m.DecideOrder(input)
	// b is filtered from the available set {a, c}; the rotation counter is at
	// 0, so the session is re-assigned to a (and the caller re-parks it).
	if head := input.Targets[result.Order[0]].Provider; head != "a" {
		t.Fatalf("re-assignment head = %q, want a", head)
	}
	if result.StickyProvider != "a" {
		t.Fatalf("re-assignment StickyProvider = %q, want a", result.StickyProvider)
	}
}

// TestDecideOrderLoadBalanceSessionIdlePastDwellReRotates: the eviction sweep
// removes an idle session's sticky entry (idle > dwell) BEFORE the affinity
// lookup, so a returning session is re-assigned by the rotation instead of
// resurrecting a stale park. The rotation counter is primed past a so the
// re-assignment provably comes from the counter, not the stale entry.
func TestDecideOrderLoadBalanceSessionIdlePastDwellReRotates(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	// Session parked on a long ago; idle for 2h with dwell 1h.
	m.SetSticky("session", Sticky{Provider: "a", Since: now.Add(-2 * time.Hour)}, 1)
	// Prime the rotation counter to 1 (next slot b) with a sessionless call.
	if result := m.DecideOrder(loadBalanceInput("m", now, "a", "b", "c")); result.StickyProvider != "" {
		t.Fatalf("sessionless prime StickyProvider = %q, want empty", result.StickyProvider)
	}

	input := loadBalanceInput("m", now, "a", "b", "c")
	input.SessionKey = "session"
	result := m.DecideOrder(input)
	if head := input.Targets[result.Order[0]].Provider; head != "b" {
		t.Fatalf("returning idle session head = %q, want rotation slot b (stale sticky must not apply)", head)
	}
	if result.StickyProvider != "b" {
		t.Fatalf("returning idle session StickyProvider = %q, want b", result.StickyProvider)
	}
	if _, still := m.Sticky("session"); still {
		t.Fatal("idle sticky entry must be evicted by the sweep")
	}
}

func TestDecideOrderLoadBalanceSkipsUnavailable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	// b is rate-limited until far in the future: the rotation runs over the
	// remaining a, c without consuming a slot for b.
	m.RecordRateLimit("b", now.Add(time.Hour), RateLimitTransient, 1)

	input := loadBalanceInput("m", now, "a", "b", "c")
	var heads []string
	for i := 0; i < 4; i++ {
		result := m.DecideOrder(input)
		if len(result.Order) != 2 {
			t.Fatalf("round %d order = %v, want the 2 available targets", i, result.Order)
		}
		heads = append(heads, input.Targets[result.Order[0]].Provider)
	}
	if !reflect.DeepEqual(heads, []string{"a", "c", "a", "c"}) {
		t.Fatalf("heads = %v, want [a c a c]", heads)
	}
}

func TestDecideOrderLoadBalanceHonorsPin(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	m.SetPin("m", Pin{Provider: "c"})

	input := loadBalanceInput("m", now, "a", "b", "c")
	for i := 0; i < 3; i++ {
		result := m.DecideOrder(input)
		if !reflect.DeepEqual(result.Order, []int{2}) {
			t.Fatalf("pinned round %d order = %v, want only target c", i, result.Order)
		}
	}
}

func TestPreviewOrderLoadBalanceDoesNotAdvanceRotation(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	input := loadBalanceInput("m", now, "a", "b", "c")

	// Two committed schedules advance a → b; previews then repeatedly show
	// the same head (c) without consuming it; a commit takes that head.
	m.DecideOrder(input)
	m.DecideOrder(input)

	snapshot := m.Dashboard(now)
	for i := 0; i < 3; i++ {
		result := snapshot.PreviewOrder(input)
		if head := input.Targets[result.Order[0]].Provider; head != "c" {
			t.Fatalf("preview %d head = %q, want c", i, head)
		}
	}
	result := m.DecideOrder(input)
	if head := input.Targets[result.Order[0]].Provider; head != "c" {
		t.Fatalf("post-preview commit head = %q, want c", head)
	}
}

// TestPreviewOrderLoadBalanceKeepsSessionAffinity: the dashboard preview
// reproduces a session's parked provider without advancing the rotation.
func TestPreviewOrderLoadBalanceKeepsSessionAffinity(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	input := loadBalanceInput("m", now, "a", "b", "c")

	assigned := input
	assigned.SessionKey = "s1"
	if result := m.DecideOrder(assigned); result.StickyProvider != "a" {
		t.Fatalf("assignment StickyProvider = %q, want a", result.StickyProvider)
	}
	m.SetSticky("s1", Sticky{Provider: "a", Since: now}, 1)
	// A sessionless schedule advances the counter past b — the session's
	// preview must still show its parked provider a.
	m.DecideOrder(input)

	snapshot := m.Dashboard(now)
	result := snapshot.PreviewOrder(assigned)
	if head := input.Targets[result.Order[0]].Provider; head != "a" {
		t.Fatalf("session preview head = %q, want parked a", head)
	}
	if result.StickyProvider != "a" {
		t.Fatalf("session preview StickyProvider = %q, want a", result.StickyProvider)
	}
}

func TestDecideOrderLoadBalanceResetsWithGeneration(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	m := newTestManager(1)
	input := loadBalanceInput("m", now, "a", "b", "c")
	m.DecideOrder(input) // head a, counter → 1

	m.ReplaceGeneration(2, nil)
	input.Generation = 2
	result := m.DecideOrder(input)
	if head := input.Targets[result.Order[0]].Provider; head != "a" {
		t.Fatalf("post-reload head = %q, want a (rotation resets per generation)", head)
	}
}
