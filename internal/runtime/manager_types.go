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
}

// ModelLockStatus is the detached dashboard state of one model lock.
type ModelLockStatus struct {
	Provider    string
	Model       string
	Failures    int
	LockedUntil time.Time
}

// DashboardSnapshot is an atomic, detached view for status presentation.
type DashboardSnapshot struct {
	Generation uint64
	Providers  map[string]ProviderStatus
	ModelLocks map[string][]ModelLockStatus
	Sticky     map[string]Sticky
	Pins       map[string]Pin
	Quotas     map[string]*provider.QuotaSnapshot

	capturedAt time.Time
	spread     map[string]uint64
}

// Target contains immutable config-derived scheduling inputs. Quota-derived
// billing and surplus deliberately do not cross the Manager boundary: they are
// projected from the Manager-owned quota snapshot in the same critical section
// that reads health, pin, sticky, model locks, and spread.
type Target struct {
	Provider        string
	Parent          string
	Model           string
	Priority        int
	BillingOverride provider.BillingClass
	PeakMultiplier  float64
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
	Commit       bool
	Generation   uint64
}

// ScheduleFacts is the quota projection used for one input target. Facts stays
// aligned with ScheduleInput.Targets, allowing a dashboard preview to render
// the exact billing/surplus values used by its ordering decision.
type ScheduleFacts struct {
	Billing provider.BillingClass
	Surplus float64
}

type ScheduleResult struct {
	Order          []int
	StickyProvider string
	Facts          []ScheduleFacts
}
