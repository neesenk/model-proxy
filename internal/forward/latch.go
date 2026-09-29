package forward

import "time"

// Latch is the forward package's consumer-owned view of a session-scoped
// route-tier escalation. internal/app adapts it to/from runtime.Latch so that
// forward does not depend on internal/runtime.
type Latch struct {
	Target  string
	Since   time.Time
	BadRuns int
}

// LatchOutcome is one request outcome applied to the (sessionKey, route)
// latch by RouteState.RecordLatchOutcome — the runtime Manager applies it in
// ONE critical section (expiry check, streak increment/reset, escalation), so
// concurrent requests of the same session cannot lose updates. internal/app
// adapts it to runtime.LatchOutcome.
type LatchOutcome struct {
	SessionKey string
	// Route is the exposed route name the outcome belongs to; latches are
	// keyed by (SessionKey, Route), like the repeat_turn window.
	Route string
	Now   time.Time
	// Dwell is the latch expiry window (escalation.dwell).
	Dwell time.Duration
	// Consecutive is the escalation threshold.
	Consecutive int
	// Target is the latch target applied when the streak reaches Consecutive
	// ("provider/model" or "grade:<name>").
	Target string
	// BadSignals is the number of bad-run increments this outcome contributes.
	BadSignals int
	// Good marks a committed run with no bad signals: reset the streak of an
	// existing latch, keep Target/Since (hysteresis).
	Good bool
}
