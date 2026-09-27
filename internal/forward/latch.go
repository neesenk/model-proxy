package forward

import "time"

// Latch is the forward package's consumer-owned view of a session-scoped
// route-tier escalation. internal/app adapts it to/from runtime.Latch so that
// forward does not depend on internal/runtime.
type Latch struct {
	Target  string
	Since   time.Time
	BadRuns int
}
