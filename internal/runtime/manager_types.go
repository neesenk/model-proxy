package runtime

import (
	"strings"
	"time"

	"model-proxy/internal/provider"
)

// RateLimitKind classifies what an upstream 429 says is exhausted.
type RateLimitKind int

const (
	Transient RateLimitKind = iota
	Quota
	Daily
)

// Descriptive aliases are useful at call sites that already use quota as a
// noun. The short names above are the canonical persisted categories.
const (
	RateLimitTransient = Transient
	RateLimitQuota     = Quota
	RateLimitDaily     = Daily
)

func (k RateLimitKind) String() string {
	switch k {
	case Quota:
		return "quota"
	case Daily:
		return "daily"
	default:
		return "transient"
	}
}

// ParseRateLimitKind parses the persisted category. Unknown and legacy values
// conservatively retain the historical transient behavior.
func ParseRateLimitKind(s string) RateLimitKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "quota":
		return Quota
	case "daily":
		return Daily
	default:
		return Transient
	}
}

// Sticky records where a route or session is parked and when it was selected.
type Sticky struct {
	Provider string    `json:"provider"`
	Since    time.Time `json:"since"`
}

// Pin is an in-memory hard route constraint. A zero ExpiresAt never expires.
type Pin struct {
	Provider  string    `json:"provider"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func (p Pin) Active(now time.Time) bool {
	return p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt)
}

func (p Pin) ExpiresLabel(now time.Time) string {
	if p.ExpiresAt.IsZero() {
		return ""
	}
	d := p.ExpiresAt.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "expires in " + d.Round(time.Second).String()
}

// Latch is a session-scoped route-tier escalation: after enough consecutive
// bad runs the session is pinned to Target for the Dwell window. It is memory-
// only, not persisted, and cleared on ReplaceGeneration — same lifecycle as
// session sticky. Latches are keyed by latchKey (session + route), like the
// repeat_turn window: one route's bad runs never reset or escalate another's.
type Latch struct {
	Target  string    `json:"target"`
	Since   time.Time `json:"since"`
	BadRuns int       `json:"bad_runs"`
}

// latchKey identifies one session's escalation latch on one exposed route.
type latchKey struct {
	SessionKey string
	Route      string
}

// LatchOutcome is one request outcome applied to the (session, route) latch by
// Manager.RecordLatchOutcome in a single critical section.
type LatchOutcome struct {
	SessionKey string
	Route      string
	Now        time.Time
	// Dwell is the latch expiry window (escalation.dwell): a latch whose Since
	// is older than Dwell is treated as absent.
	Dwell time.Duration
	// Consecutive is the escalation threshold: when the bad-run streak reaches
	// it, the latch escalates to Target, Since refreshes and the streak resets.
	Consecutive int
	// Target is the latch target applied on escalation ("provider/model" or
	// "grade:<name>"). It is only written when the streak reaches Consecutive.
	Target string
	// BadSignals is the number of bad-run increments this outcome contributes
	// (multiple configured bad signals may fire on one outcome).
	BadSignals int
	// Good marks a committed run with no bad signals: it resets the bad-run
	// streak of an existing latch but keeps Target/Since (hysteresis).
	Good bool
}

// repeatTurnKey identifies one session's routing window on one exposed route.
// The window is memory-only, generation-scoped, and bounded.
type repeatTurnKey struct {
	SessionKey string
	Route      string
}

// repeatTurnEntry is one observed turn fingerprint with its observation time.
type repeatTurnEntry struct {
	TurnKey string
	At      time.Time
}

// repeatTurnWindow holds the recent turn keys for one (session, route). It is
// bounded by maxRepeatTurnWindowEntries to prevent unbounded growth.
type repeatTurnWindow struct {
	Entries []repeatTurnEntry
}

// maxRepeatTurnWindowEntries caps the per-(session,route) window. The value
// covers many agentic sub-requests within one dwell window without unbounded
// memory growth.
const maxRepeatTurnWindowEntries = 64

// ModelKey scopes model failures and learned parameter incompatibilities to one
// provider/model pair.
type ModelKey struct {
	Provider string
	Model    string
}

// PersistedHealth is the on-disk representation of one provider's frozen
// runtime state. Its JSON shape intentionally matches the legacy root type.
type PersistedHealth struct {
	RateLimitedUntil time.Time            `json:"rate_limited_until,omitempty"`
	RateLimitKind    string               `json:"rate_limit_kind,omitempty"`
	CircuitOpenUntil time.Time            `json:"circuit_open_until,omitempty"`
	Frozen           bool                 `json:"frozen,omitempty"`
	ModelLocks       map[string]time.Time `json:"model_locks,omitempty"`
	ParamBlock       map[string][]string  `json:"param_block,omitempty"`
}

// PersistSnapshot is an atomic, detached view suitable for durable encoding.
type PersistSnapshot struct {
	Generation uint64
	Quotas     map[string]*provider.QuotaSnapshot
	Sticky     map[string]Sticky
	Health     map[string]PersistedHealth
}

// ProviderStatus is the detached dashboard state of one provider.
type ProviderStatus struct {
	ConsecutiveFailures int
	CircuitState        string
	Available           bool
	CircuitOpenUntil    time.Time
	RateLimitedUntil    time.Time
	RateLimitKind       RateLimitKind
	HalfOpenInFlight    bool
	Frozen              bool
}

// ModelLockStatus is the detached dashboard state of one model lock.
type ModelLockStatus struct {
	Provider    string
	Model       string
	Failures    int
	LockedUntil time.Time
}

// QualityStatus is the detached per-provider quality EWMA state, decayed to
// the snapshot's capture time.
type QualityStatus struct {
	ErrorRate        float64 // 0..1
	TTFTMilliseconds int64
}

// DashboardSnapshot is an atomic, detached view for status presentation.
type DashboardSnapshot struct {
	Generation uint64
	Providers  map[string]ProviderStatus
	ModelLocks map[string][]ModelLockStatus
	Sticky     map[string]Sticky
	Pins       map[string]Pin
	Quotas     map[string]*provider.QuotaSnapshot
	Quality    map[string]QualityStatus

	capturedAt time.Time
	spread     map[string]uint64
	// disabled carries the operator disabled-model override into the
	// read-only PreviewOrder path (same detachment discipline as spread).
	disabled disabledModelSet
}

// Target contains immutable config-derived scheduling inputs. Quota-derived
// billing and surplus deliberately do not cross the Manager boundary: they are
// projected from the Manager-owned quota snapshot in the same critical section
// that reads health, pin, sticky, model locks, and spread.
type Target struct {
	Provider       string
	Parent         string
	Model          string
	Priority       int
	PeakMultiplier float64
	// Billing is the config-DECLARED class (an explicit `billing:` label
	// only; BillingUnknown = undeclared). It never overrides a measured
	// snapshot — it fills the tier gap when no fresh measurement exists, so
	// an unmeasured but declared plan/payg ranks with its real class instead
	// of a middling "unknown" tier. Facts.Billing (the display/measurement
	// view) stays measured-only.
	Billing provider.BillingClass
}

type ScheduleInput struct {
	Exposed      string
	SessionKey   string
	Targets      []Target
	RouteKeys    map[string]bool
	Dwell        time.Duration
	SwitchMargin float64
	Now          time.Time
	QuotaMaxAge  time.Duration
	// Quality weights (surplus-units penalty per unit of decayed EWMA). Zero
	// weights mean "no data / disabled" and reproduce the pre-quality ordering
	// exactly.
	QualityErrWeight  float64
	QualityTTFTWeight float64
	Commit            bool
	Generation        uint64
	// IgnorePins renders the schedule as if no operator pin existed. Only
	// meaningful for read-only previews (Commit=false): the dashboard overlays
	// the pin on the DEFAULT chain instead of showing the pin-narrowed chain,
	// so operators can see what the pin overrides.
	IgnorePins bool
}

// ScheduleFacts is the quota projection used for one input target. Facts stays
// aligned with ScheduleInput.Targets, allowing a dashboard preview to render
// the exact billing/surplus values used by its ordering decision.
type ScheduleFacts struct {
	Billing provider.BillingClass
	Surplus float64
	// QualityPenalty is subtracted from Surplus for ordering (and the sticky
	// switch-margin comparison); Surplus itself stays the pure quota pace score.
	QualityPenalty float64
}

type ScheduleResult struct {
	Order          []int
	StickyProvider string
	Facts          []ScheduleFacts
}
