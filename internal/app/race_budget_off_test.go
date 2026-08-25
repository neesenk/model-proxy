//go:build !race

package app

import "time"

// guardScanBudget is the clean-body scan budget without the race detector.
// The guard package measures ~0.3ms per 64KB body; 100ms for 20 scans is
// >10x headroom so a loaded CI machine cannot flake.
const guardScanBudget = 100 * time.Millisecond
