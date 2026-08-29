package provider

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
