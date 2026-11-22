package requestlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// indexedFiles maps each indexed request id to the file its row points at.
func indexedFiles(t *testing.T, indexer *Indexer) map[string]string {
	t.Helper()
	rows, err := indexer.db.Query(`SELECT request_id, file FROM records`)
	if err != nil {
		t.Fatalf("list indexed rows: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var id, file string
		if err := rows.Scan(&id, &file); err != nil {
			t.Fatalf("scan indexed row: %v", err)
		}
		if previous, dup := got[id]; dup {
			t.Fatalf("request %s indexed twice (%s and %s)", id, previous, file)
		}
		got[id] = file
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("list indexed rows: %v", err)
	}
	return got
}

// damageByte overwrites one byte in place (same size, same head) with a raw
// control character, which makes the JSON line containing it undecodable.
func damageByte(t *testing.T, path string, offset int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer file.Close()
	if _, err := file.WriteAt([]byte{0x01}, offset); err != nil {
		t.Fatalf("damage %s: %v", path, err)
	}
}

func assertIndexedFiles(t *testing.T, indexer *Indexer, want map[string]string) {
	t.Helper()
	got := indexedFiles(t, indexer)
	if len(got) != len(want) {
		t.Fatalf("indexed rows = %v, want %v", got, want)
	}
	for id, file := range want {
		if got[id] != file {
			t.Errorf("request %s indexed under %q, want %q (all rows %v)", id, got[id], file, got)
		}
	}
}

// TestIndexRotationRenameMovesIndexedRows: a size rotation renames the active
// file to its archive name (and the writer reopens a fresh active file). The
// archive holds exactly the bytes already indexed under the active name, so
// the rows move to the archive name instead of the whole (up to 1 GiB) archive
// being re-read and re-parsed from offset zero. The archive's first line is
// damaged past its head window after the rename: a re-parse would now drop
// r1, while moved rows keep it. Covered with and without the recreated active
// file (the reconcile pass may land between the rename and the reopen).
func TestIndexRotationRenameMovesIndexedRows(t *testing.T) {
	for _, recreate := range []bool{true, false} {
		name := "active vanished"
		if recreate {
			name = "active recreated"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			active := filepath.Join(dir, "requests-20260730.log")
			archiveName := "requests-20260730--010203-1.log"
			archive := filepath.Join(dir, archiveName)
			appendRecordLines(t, active,
				Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", SessionID: "s", Provider: "p", Status: 200, ResponseBody: strings.Repeat("x", 4096)},
				Record{Ts: "2026-07-30T01:01:00Z", RequestID: "r2", SessionID: "s", Provider: "p", Status: 200},
			)
			indexer := newTestIndexer(t, dir)
			mustReconcile(t, indexer)
			assertIndexedFiles(t, indexer, map[string]string{"r1": "requests-20260730.log", "r2": "requests-20260730.log"})

			if err := os.Rename(active, archive); err != nil {
				t.Fatal(err)
			}
			damageByte(t, archive, 2048)
			want := map[string]string{"r1": archiveName, "r2": archiveName}
			if recreate {
				appendRecordLines(t, active, Record{Ts: "2026-07-30T01:02:00Z", RequestID: "r3", SessionID: "s", Provider: "p", Status: 200})
				want["r3"] = "requests-20260730.log"
			}
			mustReconcile(t, indexer)
			assertIndexedFiles(t, indexer, want)

			// The moved rows still resolve: Detail seeks r2 in the archive.
			records, err := indexer.Detail("r2")
			if err != nil || len(records) != 1 || records[0].RequestID != "r2" {
				t.Fatalf("Detail(r2) after rename = %+v, %v", records, err)
			}
		})
	}
}

// TestIndexReplacedByLargerFileReindexesFromZero: a file replaced by content
// LARGER than its indexed cursor must be re-indexed from zero, never tailed
// from the old cursor into the middle of unrelated bytes. Size alone cannot
// tell a replacement from an append; the head of the file can.
func TestIndexReplacedByLargerFileReindexesFromZero(t *testing.T) {
	dir := t.TempDir()
	name := "requests-20260730.log"
	writeRecordFile(t, dir, name, []Record{{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", Provider: "p", Status: 200}})
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{"r1": name})

	writeRecordFile(t, dir, name, []Record{
		{Ts: "2026-07-30T02:00:00Z", RequestID: "r9", Provider: "p", Status: 200, ResponseBody: strings.Repeat("y", 512)},
		{Ts: "2026-07-30T02:01:00Z", RequestID: "r10", Provider: "p", Status: 200},
	})
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{"r9": name, "r10": name})
}

// TestIndexIdlePassCostsNoPerFileQuery: a reconcile pass over files that did
// not change since the previous pass must not consult the files table per
// file (the 250ms loop used to issue one SELECT per log file, ~750 queries/s
// for a month of rotated files). Observable through an out-of-band rewind of
// every stored cursor: a pass that re-read the cursors would re-tail every
// file from zero and duplicate its rows; a pass trusting its tracked state
// leaves the index as it was.
func TestIndexIdlePassCostsNoPerFileQuery(t *testing.T) {
	dir := t.TempDir()
	writeIndexFixture(t, dir)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	before := indexRowCount(t, indexer)
	if before != len(indexFixtureRecords()) {
		t.Fatalf("rows after first pass = %d, want %d", before, len(indexFixtureRecords()))
	}
	if _, err := indexer.db.Exec(`UPDATE files SET size_indexed = 0`); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	if got := indexRowCount(t, indexer); got != before {
		t.Errorf("rows after an idle pass = %d, want %d (unchanged files re-tailed)", got, before)
	}

	// Nor is an unchanged file opened: a same-size rewrite whose mtime is
	// restored is stat-identical, so the pass must not notice it (a pass that
	// opened every file to re-check its head would re-index it).
	path := filepath.Join(dir, "requests-20260729.log")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Repeat([]byte("z"), len(original)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	if got := indexedFiles(t, indexer); got["r1"] != "requests-20260729.log" || got["r2"] != "requests-20260729.log" {
		t.Errorf("rows after a stat-identical rewrite = %v, want r1/r2 kept (the unchanged file was re-read)", got)
	}
}

// TestIndexRenameDetectedAfterUpgrade: an index built before file heads were
// recorded (files.head NULL) backfills them on its first pass, so a rotation
// right after the daemon upgrade is still carried over instead of re-parsed.
func TestIndexRenameDetectedAfterUpgrade(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "requests-20260730.log")
	archiveName := "requests-20260730--010203-1.log"
	appendRecordLines(t, active,
		Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", Provider: "p", Status: 200, ResponseBody: strings.Repeat("x", 4096)},
	)
	legacy := newTestIndexer(t, dir)
	mustReconcile(t, legacy)
	if _, err := legacy.db.Exec(`UPDATE files SET head = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded := newTestIndexer(t, dir)
	mustReconcile(t, upgraded) // backfills the head of the already-indexed file
	if err := os.Rename(active, filepath.Join(dir, archiveName)); err != nil {
		t.Fatal(err)
	}
	damageByte(t, filepath.Join(dir, archiveName), 2048)
	mustReconcile(t, upgraded)
	assertIndexedFiles(t, upgraded, map[string]string{"r1": archiveName})
}

// TestIndexFileScopedStatementsUseFileIndex: retention deletes, truncation
// re-indexes and rotation moves all address rows by file. Without an index on
// records(file) each of them walks the whole table (a quarter-million rows on
// a month of traffic) once per rotated or deleted file.
func TestIndexFileScopedStatementsUseFileIndex(t *testing.T) {
	indexer := newTestIndexer(t, t.TempDir())
	for _, statement := range []string{
		`DELETE FROM records WHERE file = 'f'`,
		`UPDATE records SET file = 'g' WHERE file = 'f'`,
	} {
		rows, err := indexer.db.Query(`EXPLAIN QUERY PLAN ` + statement)
		if err != nil {
			t.Fatalf("explain %s: %v", statement, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notUsed int
			var detail string
			if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
				t.Fatalf("scan plan: %v", err)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if joined := strings.Join(plan, "; "); !strings.Contains(joined, "idx_records_file") {
			t.Errorf("plan for %s = %q, want a search on idx_records_file", statement, joined)
		}
	}
}

// TestIndexTickReconcilesOnChangeOrIdleBound: the reconcile loop's ticker
// beat runs a pass only when the log writer reported a change since the last
// pass (Notify — the daemon's only writer to this directory), or once every
// idleReconcileTicks beats so external edits are still picked up. An idle
// directory therefore costs one pass per idle bound instead of one per beat,
// while a committed record is still indexed on the next beat.
func TestIndexTickReconcilesOnChangeOrIdleBound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, path, Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", Status: 200})
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer) // Run's boot pass

	appendRecordLines(t, path, Record{Ts: "2026-07-30T01:01:00Z", RequestID: "r2", Status: 200})
	for beat := 1; beat < idleReconcileTicks; beat++ {
		if indexer.tickReconcile() {
			t.Fatalf("beat %d ran a pass with no change notification", beat)
		}
	}
	if got := indexRowCount(t, indexer); got != 1 {
		t.Fatalf("rows before the idle bound = %d, want 1", got)
	}
	if !indexer.tickReconcile() {
		t.Fatal("the idle-bound beat did not run a pass")
	}
	if got := indexRowCount(t, indexer); got != 2 {
		t.Fatalf("rows after the idle-bound pass = %d, want 2 (external append picked up)", got)
	}

	appendRecordLines(t, path, Record{Ts: "2026-07-30T01:02:00Z", RequestID: "r3", Status: 200})
	indexer.Notify()
	if !indexer.tickReconcile() {
		t.Fatal("the beat after Notify did not run a pass")
	}
	if got := indexRowCount(t, indexer); got != 3 {
		t.Fatalf("rows after the notified pass = %d, want 3", got)
	}
	if indexer.tickReconcile() {
		t.Error("a second beat ran a pass although nothing changed since the notified one")
	}
}

// TestReconcileDropsRowsWhenPlainFileZstdArchived: the archive transition
// writes <name>.zst and removes the plain .log. The indexer must never
// claim the .zst (suffix contract) and must evict the vanished plain
// file's rows — the existing unclaimed-file deletion path — leaving the
// archived ids a graceful Detail miss, not an error.
func TestReconcileDropsRowsWhenPlainFileZstdArchived(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "requests-20260730.log")
	appendRecordLines(t, active,
		Record{Ts: "2026-07-30T01:00:00Z", RequestID: "r1", SessionID: "s", Provider: "p", Status: 200},
		Record{Ts: "2026-07-30T01:01:00Z", RequestID: "r2", SessionID: "s", Provider: "p", Status: 200},
	)
	indexer := newTestIndexer(t, dir)
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{
		"r1": "requests-20260730.log",
		"r2": "requests-20260730.log",
	})
	if err := os.WriteFile(active+".zst", []byte("compressed bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	mustReconcile(t, indexer)
	assertIndexedFiles(t, indexer, map[string]string{})
	if records, err := indexer.Detail("r1"); err != nil || len(records) != 0 {
		t.Fatalf("Detail(r1) after archive = %+v, %v; want graceful empty", records, err)
	}
}
