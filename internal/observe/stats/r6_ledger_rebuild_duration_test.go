package stats

import (
	"strings"
	"testing"
	"time"
)

// TestRebuildTokenLedgerLogsDuration: the boot-time rebuild runs before the
// listener comes up and is paid on every start, so its cost must be visible
// in the one line it already logs. The duration is measured, never asserted
// as a value: the test requires the field to exist and to parse as a
// non-negative duration, and the line to report how many groups the
// aggregation produced. A rebuild that found nothing to rewrite used to log
// nothing at all — the cost stayed invisible exactly when no other line
// pointed at it.
func TestRebuildTokenLedgerLogsDuration(t *testing.T) {
	t.Run("rewritten buckets: duration and group counts on the summary line", func(t *testing.T) {
		store, idxPath := seedLedgerFixture(t)

		logs, rebuilt, skipped, err := rebuildCapturingLogs(t, store, idxPath)
		if err != nil || rebuilt != 2 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (2, 0, nil)", rebuilt, skipped, err)
		}
		line := rebuiltSummaryLine(t, logs)
		rebuildDuration(t, line)
		if !strings.Contains(line, "1 minute + 1 agent group(s) aggregated") {
			t.Errorf("summary line does not report the aggregated group counts: %s", line)
		}
	})

	t.Run("nothing to rewrite: the cost is still reported", func(t *testing.T) {
		// A populated index whose only group (a post-cutoff minute) matches no
		// bucket: nothing is rewritten, skipped or reset.
		store, idxPath := openLedgerRebuildFixture(t)
		insertLedgerRecords(t, idxPath, "2026-11-20T10:00:10Z", 2, ledgerRecordRow{input: 10, output: 5})

		logs, rebuilt, skipped, err := rebuildCapturingLogs(t, store, idxPath)
		if err != nil || rebuilt != 0 || skipped != 0 {
			t.Fatalf("rebuild = (%d, %d, %v), want (0, 0, nil)", rebuilt, skipped, err)
		}
		line := rebuiltSummaryLine(t, logs)
		rebuildDuration(t, line)
		if !strings.Contains(line, "rebuilt 0 historical token bucket(s)") ||
			!strings.Contains(line, "1 minute + 0 agent group(s) aggregated") {
			t.Errorf("summary line does not report the no-op rebuild and its group counts: %s", line)
		}
	})
}

// rebuiltSummaryLine returns the one summary line a rebuild over a populated
// index logs, failing the test when there is none.
func rebuiltSummaryLine(t *testing.T, logs string) string {
	t.Helper()
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "[stats] rebuilt ") {
			return line
		}
	}
	t.Fatalf("the rebuild logged no summary line; log output:\n%s", logs)
	return ""
}

// rebuildDuration reads the duration field of a rebuild summary line and
// requires it to be present and to parse as a non-negative duration. The
// value itself is not asserted: it depends on the machine running the test.
func rebuildDuration(t *testing.T, line string) time.Duration {
	t.Helper()
	const marker = "; took "
	i := strings.LastIndex(line, marker)
	if i < 0 {
		t.Fatalf("summary line has no duration field: %s", line)
	}
	field := strings.TrimSuffix(line[i+len(marker):], ")")
	d, err := time.ParseDuration(field)
	if err != nil {
		t.Fatalf("duration %q is not a valid duration (%v): %s", field, err, line)
	}
	if d < 0 {
		t.Fatalf("duration %q is negative: %s", field, line)
	}
	return d
}
