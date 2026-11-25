package logfile

import (
	"bytes"
	"log"
	"testing"

	"model-proxy/internal/observe/logx"
)

// Two seams shared by the r6 *_test.go files for asserting on logx output.
// Both touch process-global state: tests using them must not run in parallel.

// captureLogs redirects the standard logger (where logx writes) into a buffer
// for the rest of the test, flags stripped so a line compares as formatted.
// Every test that makes the code under test log takes one: what a test has no
// assertion on must not reach the test output either.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	savedWriter, savedFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(savedWriter)
		log.SetFlags(savedFlags)
	})
	return &buf
}

// pinLogLevel holds logx at the given level for the test and restores it
// afterwards. A line's level is part of what the tests assert: a sibling test
// that left the level at warn would silence every info line they look for.
func pinLogLevel(t *testing.T, level string) {
	t.Helper()
	saved := logx.CurrentLevel()
	logx.SetLevel(level)
	t.Cleanup(func() { logx.SetLevel(saved.String()) })
}
