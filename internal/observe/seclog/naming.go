package seclog

import "strings"

// Permissions enforced by the shared logfile sink (observe/logfile) for this
// package's audit directory and files. Kept as named constants so tests can
// assert the owner-only contract symbolically.
const (
	dirMode     = 0o700
	logFileMode = 0o600
)

// isAuditFile reports whether name matches the audit-log naming scheme
// (filePrefix + day stamp + optional rotation suffix + .log).
func isAuditFile(name string) bool {
	return strings.HasPrefix(name, filePrefix) && strings.HasSuffix(name, ".log")
}
