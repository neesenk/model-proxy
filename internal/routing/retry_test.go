package routing

import (
	"testing"
	"time"
)

func TestDecideFailure(t *testing.T) {
	now := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		input FailureInput
		want  FailureDecision
	}{
		{
			name: "short all-down cooldown waits",
			input: FailureInput{
				AllDown: true, AllDownRateLimited: true,
				Round: 0, MaxRetryRounds: 2, RetryWait: 10 * time.Second,
				EarliestRecovery: now.Add(time.Second), Now: now,
			},
			want: FailureDecision{Action: FailureWait, Wait: time.Second},
		},
		{
			name: "recovered untried target retries immediately",
			input: FailureInput{
				RecoveredUntried: true,
				Round:            1, MaxRetryRounds: 2, RetryWait: 10 * time.Second,
				Now: now,
			},
			want: FailureDecision{Action: FailureRetryNow},
		},
		{
			name: "one-shot override bypasses wait",
			input: FailureInput{
				SawRateLimit: true, AllDown: true, AllDownRateLimited: true,
				BypassWait: true, Round: 0, MaxRetryRounds: 2,
				RetryWait: 10 * time.Second, RateLimitBackoff: time.Minute,
				EarliestRecovery: now.Add(1500 * time.Millisecond), Now: now,
			},
			want: FailureDecision{
				Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 2,
			},
		},
		{
			name: "retry budget exhausted",
			input: FailureInput{
				SawRateLimit: true, AllDown: true, AllDownRateLimited: true,
				Round: 2, MaxRetryRounds: 2, RetryWait: 10 * time.Second,
				RateLimitBackoff: time.Minute,
				EarliestRecovery: now.Add(time.Second), Now: now,
			},
			want: FailureDecision{
				Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 1,
			},
		},
		{
			name: "hard failure dominates rate limit",
			input: FailureInput{
				SawHard: true, SawRateLimit: true,
				AllDown: true, AllDownRateLimited: false,
				RateLimitBackoff: time.Minute, Now: now,
			},
			want: FailureDecision{Action: FailureTerminal},
		},
		{
			name: "pure rate limit uses default after horizon lapses",
			input: FailureInput{
				SawRateLimit: true, RateLimitBackoff: 1500 * time.Millisecond,
				EarliestRecovery: now.Add(-time.Second), Now: now,
			},
			want: FailureDecision{
				Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 2,
			},
		},
		{
			name: "runtime pure-rate snapshot is sufficient",
			input: FailureInput{
				AllDown: true, AllDownRateLimited: true,
				RateLimitBackoff: time.Minute,
				EarliestRecovery: now.Add(30 * time.Second), Now: now,
			},
			want: FailureDecision{
				Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 30,
			},
		},
		{
			name:  "no targets is a hard terminal",
			input: FailureInput{Now: now},
			want:  FailureDecision{Action: FailureTerminal},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := DecideFailure(test.input); got != test.want {
				t.Fatalf("decision = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestDecideFailureSaturation: every target at its concurrency limit is
// all-down in the rate-limit class with NO time horizon — a slot frees when
// a request completes, not at a dated moment. The wait is therefore for a
// slot (up to retry_wait, inside the same round budget), and once the budget
// is spent the terminal is 429 with a one-second Retry-After rather than the
// whole transient backoff.
func TestDecideFailureSaturation(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	saturated := func(round int) FailureInput {
		return FailureInput{
			AllDown: true, AllDownRateLimited: true, AllDownSaturated: true,
			Round: round, MaxRetryRounds: 2, RetryWait: 10 * time.Second, RateLimitBackoff: time.Minute,
			EarliestRecovery: now, Now: now,
		}
	}
	for _, round := range []int{0, 1} {
		if got, want := DecideFailure(saturated(round)), (FailureDecision{Action: FailureWait, Wait: 10 * time.Second, WaitForSlot: true}); got != want {
			t.Errorf("saturated round %d = %+v, want %+v", round, got, want)
		}
	}
	if got, want := DecideFailure(saturated(2)), (FailureDecision{Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 1}); got != want {
		t.Errorf("budget spent = %+v, want %+v", got, want)
	}

	// A long cooldown elsewhere does not hold the request back: the
	// saturated sibling may free a slot long before that horizon.
	mixed := saturated(0)
	mixed.EarliestRecovery = now.Add(5 * time.Minute)
	if got, want := DecideFailure(mixed), (FailureDecision{Action: FailureWait, Wait: 10 * time.Second, WaitForSlot: true}); got != want {
		t.Errorf("saturated beside a long cooldown = %+v, want %+v", got, want)
	}

	bypass := saturated(0)
	bypass.BypassWait = true
	if got, want := DecideFailure(bypass), (FailureDecision{Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 1}); got != want {
		t.Errorf("one-shot override = %+v, want %+v", got, want)
	}

	off := saturated(0)
	off.RetryWait = 0
	if got, want := DecideFailure(off), (FailureDecision{Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 1}); got != want {
		t.Errorf("retry_wait disabled = %+v, want %+v", got, want)
	}

	hard := saturated(2)
	hard.SawHard, hard.AllDownRateLimited = true, false
	if got, want := DecideFailure(hard), (FailureDecision{Action: FailureTerminal}); got != want {
		t.Errorf("hard failure in the chain = %+v, want %+v", got, want)
	}

	// Without saturation the dated-horizon rules stand: no horizon at all
	// keeps the transient backoff as the Retry-After.
	plain := saturated(2)
	plain.AllDownSaturated, plain.SawRateLimit = false, true
	if got, want := DecideFailure(plain), (FailureDecision{Action: FailureTerminal, RateLimited: true, RetryAfterSeconds: 60}); got != want {
		t.Errorf("all-down on a lapsed horizon = %+v, want %+v", got, want)
	}
}
