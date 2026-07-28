package main

import (
	"strconv"
	"time"
)

// normalizeBucket parses an API display-granularity spec into seconds. Storage
// remains one-minute; wider values only affect the query projection.
func normalizeBucket(value string) int64 {
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
