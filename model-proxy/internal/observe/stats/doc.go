// Package stats owns SQLite persistence and read projections for minute-level
// proxy observability counters.
//
// Runtime counters and lifecycle orchestration deliberately remain in the
// composition root. Callers flush detached deltas into Store; this package has
// no dependency on Proxy, configuration, providers, or HTTP/CLI presentation.
package stats
