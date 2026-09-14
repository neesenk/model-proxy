package seclog

// Permissions enforced by the shared logfile sink (observe/logfile) for this
// package's audit directory and files, and by the SQLite store for
// security.db. Kept as named constants so tests can assert the owner-only
// contract symbolically.
const (
	dirMode     = 0o700
	logFileMode = 0o600
)
