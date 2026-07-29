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
