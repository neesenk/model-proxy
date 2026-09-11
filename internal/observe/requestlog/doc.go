// Package requestlog owns the asynchronous JSONL request-log data plane: the
// record schema and encoding, the streaming top-K file-scan queries, and the
// tailing SQLite index (index.go) the web read path queries by default.
//
// Application routing, protocol conversion, lifecycle ordering, HTTP handlers,
// and CLI replay policy remain in the composition root. This package receives
// only detached values after those decisions have been made.
package requestlog
