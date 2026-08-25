//go:build race

package app

import "time"

// guardScanBudget is the clean-body scan budget under the race detector:
// instrumented scanning is ~10-15x slower, so the perf gate uses a looser
// ceiling here than in race_budget_off_test.go.
const guardScanBudget = 500 * time.Millisecond
