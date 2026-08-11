// Package requestlog owns the asynchronous JSONL request-log data plane.
//
// Application routing, protocol conversion, lifecycle ordering, HTTP handlers,
// and CLI replay policy remain in the composition root. This package receives
// only detached values after those decisions have been made.
package requestlog
