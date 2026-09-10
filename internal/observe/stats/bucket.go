package stats

import (
	"strconv"
	"time"
)

// ParseWindow parses a GET /api/tokens window spec into seconds (0 = all-time
// cumulative counters, the default when the param is absent). The contract is
// enumerated — 1h, 24h, 7d, all — and anything else is rejected so a typo
// can't silently widen or narrow the reported usage (the handler maps !ok to
// 400, mirroring the security kind validation).
func ParseWindow(value string) (seconds int64, ok bool) {
	switch value {
	case "", "all":
		return 0, true
	case "1h":
		return 3600, true
	case "24h":
		return 24 * 3600, true
	case "7d":
		return 7 * 24 * 3600, true
	}
	return 0, false
}

// NormalizeBucket parses an API display-granularity spec into seconds. Storage
// remains one-minute; wider values only affect the query projection.
func NormalizeBucket(value string) int64 {
	if value == "" || value == "0" {
		return 60
	}
	var seconds int64
	if duration, err := time.ParseDuration(value); err == nil {
		seconds = int64(duration.Seconds())
	} else if number, err := strconv.ParseInt(value, 10, 64); err == nil {
		seconds = number
	} else {
		return 60
	}
	if seconds <= 60 {
		return 60
	}
	if remainder := seconds % 60; remainder != 0 {
		seconds += 60 - remainder
	}
	return seconds
}
