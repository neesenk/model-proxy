package routing

import "time"

// FailureAction tells the HTTP orchestration how to proceed after one complete
// scheduling/failover pass. The policy is side-effect free: callers own waits,
// cancellation, logging, metrics, and the final response.
type FailureAction int

const (
	FailureTerminal FailureAction = iota
	FailureWait
	FailureRetryNow
)

// FailureInput combines the failure classes observed across all passes with a
// fresh runtime availability snapshot for the effective target set.
type FailureInput struct {
	SawHard            bool
	SawRateLimit       bool
	AllDown            bool
	AllDownRateLimited bool
	RecoveredUntried   bool
	BypassWait         bool
	Round              int
	MaxRetryRounds     int
	RetryWait          time.Duration
	RateLimitBackoff   time.Duration
	EarliestRecovery   time.Time
	Now                time.Time
}

// FailureDecision describes either another bounded pass or the terminal
// failure class. RetryAfterSeconds is populated only for a rate-limit terminal.
type FailureDecision struct {
	Action            FailureAction
	Wait              time.Duration
	RateLimited       bool
	RetryAfterSeconds int
}

// DecideFailure preserves the proxy's cooldown-aware retry contract:
// short all-down windows wait, a recovered-but-untried target retries
// immediately, and terminal status is 429 only when no hard failure occurred.
func DecideFailure(input FailureInput) FailureDecision {
	if !input.BypassWait &&
		input.RetryWait > 0 &&
		input.Round < input.MaxRetryRounds {
		if input.AllDown {
			wait := input.EarliestRecovery.Sub(input.Now)
			if wait > 0 && wait <= input.RetryWait {
				return FailureDecision{Action: FailureWait, Wait: wait}
			}
		} else if input.RecoveredUntried {
			return FailureDecision{Action: FailureRetryNow}
		}
	}

	rateLimited := !input.SawHard &&
		(input.SawRateLimit || (input.AllDown && input.AllDownRateLimited))
	if !rateLimited {
		return FailureDecision{Action: FailureTerminal}
	}
	retryAfter := input.EarliestRecovery.Sub(input.Now)
	if retryAfter <= 0 {
		retryAfter = input.RateLimitBackoff
	}
	seconds := int(retryAfter / time.Second)
	if retryAfter%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		seconds = 1
	}
	return FailureDecision{
		Action:            FailureTerminal,
		RateLimited:       true,
		RetryAfterSeconds: seconds,
	}
}
