package protocol

import (
	"io"
	"testing"
)

// readAllChecked reads r fully, failing the test on a read error. A stream
// that fails mid-read returns a truncated prefix from io.ReadAll; swallowing
// that error lets downstream substring assertions pass on truncated output.
func readAllChecked(t *testing.T, r io.Reader) []byte {
	t.Helper()
	body, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read converted stream: %v", err)
	}
	return body
}
