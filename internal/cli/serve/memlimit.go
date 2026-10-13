package serve

import (
	"os"
	"runtime/debug"
)

// DefaultMemoryLimitBytes is the serve process's default soft GC ceiling:
// live heap sits in the tens of MB (bounded caches, rings and counters), so
// 1GiB is invisible to the GC in normal operation.
const DefaultMemoryLimitBytes int64 = 1 << 30

// ApplyDefaultMemoryLimit sets the soft GOMEMLIMIT for a serve process when
// the operator has not set one via the GOMEMLIMIT env var (which always wins,
// including through the supervisor's inherited environment). The limit is a
// backstop against heap-balloon regressions — the request-log indexer once
// held a whole 500-row batch of request bodies and per-row analytics timezone
// loads once allocated hundreds of MB per query, together pushing RSS to
// multi-GB — trading bounded extra GC effort near the ceiling for unbounded
// heap growth. It is a soft limit: the runtime GCs harder approaching it and
// never OOMs on its own.
func ApplyDefaultMemoryLimit() bool {
	return applyDefaultMemoryLimit(os.Getenv, debug.SetMemoryLimit)
}

func applyDefaultMemoryLimit(getenv func(string) string, setLimit func(int64) int64) bool {
	if getenv("GOMEMLIMIT") != "" {
		return false
	}
	setLimit(DefaultMemoryLimitBytes)
	return true
}
