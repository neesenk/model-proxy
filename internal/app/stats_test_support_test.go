package app

import (
	"path/filepath"
	"testing"
	"time"

	observestats "model-proxy/internal/observe/stats"
)

func openTestStatsStore(path string, retention time.Duration) (*observestats.Store, error) {
	return observestats.Open(observestats.Options{Path: path, Retention: retention})
}

// newTestStatsStore opens a fresh observestats.Store in a temp dir with no retention.
func newTestStatsStore(t *testing.T) *observestats.Store {
	t.Helper()
	ss, err := openTestStatsStore(filepath.Join(t.TempDir(), "stats.db"), 0)
	if err != nil {
		t.Fatalf("openTestStatsStore: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}
