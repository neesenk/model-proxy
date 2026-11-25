package stats

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

// TestRebuildTokenLedgerReportsSkipOnEmptyIndex: the boot-time rebuild reads
// the request-log index, and an index that holds no records yet — the
// request log was enabled but nothing has been logged (the index file exists
// with its schema and no row), or retention swept every row — leaves every
// historical bucket as committed. That used to happen without a trace: the
// operator saw neither a rebuild line nor a failure while the pre-cutoff
// mis-ledgers stayed in place. The skip must be reported as one readable
// line naming the index, and only when it happens.
//
// An index file that is missing or unreadable is a different path: the
// rebuild returns the error and Bootstrap already warns with it.
func TestRebuildTokenLedgerReportsSkipOnEmptyIndex(t *testing.T) {
	t.Run("empty index: one skip line naming the index, committed values stand", func(t *testing.T) {
		store, idxPath := openLedgerRebuildFixture(t)
		minute := int64(1791520380) // 2026-10-09T12:33+08:00, pre-cutoff
		seedLedgerBucket(t, store, minute,
			Counters{Requests: 3, Input: 500, Output: 90, CacheRead: 200, TokenRequests: 3},
			AgentCounters{Requests: 3, Input: 500, Output: 90, CacheRead: 200})

		logs, rebuilt, skipped, err := rebuildCapturingLogs(t, store, idxPath)
		if err != nil || rebuilt != 0 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (0, 0, nil)", rebuilt, skipped, err)
		}
		assertMinuteLedger(t, store, minute, 3, ledgerTotals{input: 500, output: 90, cacheRead: 200})
		assertAgentLedger(t, store, minute, ledgerTotals{input: 500, output: 90, cacheRead: 200})
		if got := strings.Count(logs, "usage ledger rebuild skipped"); got != 1 {
			t.Fatalf("skip line count = %d, want 1; log output:\n%s", got, logs)
		}
		if !strings.Contains(logs, idxPath) {
			t.Errorf("skip line does not name the index %s; log output:\n%s", idxPath, logs)
		}
	})

	t.Run("populated index: the rebuild runs and no skip line is printed", func(t *testing.T) {
		store, idxPath := seedLedgerFixture(t)

		logs, rebuilt, skipped, err := rebuildCapturingLogs(t, store, idxPath)
		if err != nil || rebuilt != 2 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
		}
		if strings.Contains(logs, "rebuild skipped") {
			t.Errorf("a completed rebuild must not report a skip; log output:\n%s", logs)
		}
	})
}

// rebuildCapturingLogs runs one rebuild with the process log redirected into
// a buffer and returns what was logged. The redirect is undone before the
// buffer is read: log.SetOutput hands over under the logger's mutex, so no
// writer can still be inside the buffer when it is returned.
func rebuildCapturingLogs(t *testing.T, store *Store, idxPath string) (logs string, rebuilt, skipped int64, err error) {
	t.Helper()
	var buf bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(previous) }) // safety net if the rebuild panics
	rebuilt, skipped, err = store.RebuildTokenLedger(idxPath)
	log.SetOutput(previous)
	return buf.String(), rebuilt, skipped, err
}
