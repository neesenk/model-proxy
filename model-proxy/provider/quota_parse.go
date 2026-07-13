package provider

import "strconv"

// ultimateRemaining returns the RemainingPct of the window marked Ultimate, or
// -1 if there is none (the parser then can't derive a scheduling base). Shared
// by the codex/zhipu/volcengine/aqp quota parsers.
func ultimateRemaining(windows []QuotaWindow) float64 {
	for _, w := range windows {
		if w.Ultimate {
			return w.RemainingPct
		}
	}
	return -1
}

// or returns s if non-empty, else fallback. Provider-package helper for the
// quota parsers; main has its own `or` for the display functions that have not
// migrated yet.
func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// atof parses a float, returning 0 on error. Used by the deepseek balance parser.
func atof(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
