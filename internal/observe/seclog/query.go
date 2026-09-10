package seclog

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Filter narrows an audit-log query. From/To are unix milliseconds,
// inclusive; zero means unbounded. Limit <= 0 returns every match.
type Filter struct {
	Kind  string
	From  int64
	To    int64
	Limit int
}

func (f Filter) matches(record *Record) bool {
	if f.Kind != "" && record.Kind != f.Kind {
		return false
	}
	if f.From != 0 && record.Ts < f.From {
		return false
	}
	if f.To != 0 && record.Ts > f.To {
		return false
	}
	return true
}

// Result is the outcome of a Query: matching records newest first, plus how
// many unreadable lines were skipped while scanning. Truncated reports that
// a positive Limit dropped at least one matching record (top-K eviction or a
// file-level skip after proving that file contains a match), so the returned
// set is a newest-first prefix, not the whole filtered set.
type Result struct {
	Records   []*Record
	Skipped   int
	Truncated bool
}

// Query streams every audit-log file in dir (active and rotated) and returns
// matching records newest first. A positive limit retains only a timestamp
// top-K in memory. Unparseable lines are skipped and counted in Skipped, as
// are files that cannot be opened at all. One exception: a final line with no
// trailing newline that ends at a clean EOF is a torn tail — the daemon is
// mid-write into the active file — and is ignored silently instead of being
// counted as unreadable.
//
// Early termination (file granularity): a single writer appends records to
// one file in non-decreasing Ts order, so a file's last complete record
// bounds everything in it. Once the top-K heap is full, a record strictly
// older than the heap floor is pushed and immediately evicted — a no-op — so
// a file whose newest record is older than the floor cannot change the
// result and is skipped without streaming; likewise a file whose newest
// record is older than the From bound holds no match. The ordering premise
// is VERIFIED per file, not assumed: both the first and the last complete
// record are peeked, and any anomaly — unopenable file, oversized record,
// unparseable JSON, or firstTs > lastTs (hand-built or corrupted file) —
// falls back to streaming the file as before. A skipped file's corrupt lines
// are NOT counted in Skipped (the file was provably irrelevant); files that
// cannot even be opened still count, because the peek falls back to the
// streaming path on any anomaly. Cross-file disorder stays tolerated: every
// file is peeked independently.
func Query(dir string, filter Filter) (*Result, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if isAuditFile(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	result := &Result{}
	var newest *timestampMinHeap
	if filter.Limit > 0 {
		newest = &timestampMinHeap{}
	}
	matched := 0
	for i := len(names) - 1; i >= 0; i-- {
		path := filepath.Join(dir, names[i])
		lastTs, lastOK := peekLastCompleteTs(path)
		firstTs, firstOK := peekFirstCompleteTs(path)
		if lastOK && firstOK && firstTs <= lastTs {
			// Ordering verified: the file's NEWEST complete record bounds
			// everything in it. A full heap makes every strictly-older
			// record a push-then-evict no-op. Time bounds that exclude the
			// whole file are checked first and do not imply truncation.
			if filter.From != 0 && lastTs < filter.From {
				continue
			}
			if filter.To != 0 && firstTs > filter.To {
				continue
			}
			if newest != nil && newest.Len() >= filter.Limit && lastTs < (*newest)[0].Ts {
				// With no kind filter, the last record itself proves that the
				// skipped file contains a match when it is also within To (or
				// To is unbounded). Record that dropped match before skipping.
				// A kind filter, or a To bound below lastTs, needs a real scan
				// to keep Truncated exact rather than merely conservative.
				if filter.Kind == "" && (filter.To == 0 || lastTs <= filter.To) {
					result.Truncated = true
					continue
				}
			}
		}
		file, err := os.Open(path)
		if err != nil {
			// An unreadable rotated file (e.g. damaged permissions) must
			// surface in Skipped, not vanish silently.
			result.Skipped++
			continue
		}
		reader := bufio.NewReaderSize(file, 64*1024)
		for {
			line, readErr := reader.ReadBytes('\n')
			// A non-newline-terminated last line read at a clean EOF is a
			// torn tail: the daemon is mid-write into the active file. Drop
			// it without counting Skipped — it will parse on the next run.
			tornTail := readErr == io.EOF && len(line) > 0 && line[len(line)-1] != '\n'
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && !tornTail {
				var record Record
				if err := json.Unmarshal(trimmed, &record); err != nil {
					result.Skipped++
				} else if filter.matches(&record) {
					matched++
					if newest == nil {
						result.Records = append(result.Records, &record)
					} else {
						heap.Push(newest, &record)
						if newest.Len() > filter.Limit {
							heap.Pop(newest)
						}
					}
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = file.Close()
	}
	if newest != nil {
		result.Records = *newest
	}
	// Truncated is exact: it is set either when a skipped file was proven to
	// contain a matching record, or when streaming observed more matches than
	// the top-K limit retains.
	result.Truncated = result.Truncated || filter.Limit > 0 && matched > filter.Limit
	sort.SliceStable(result.Records, func(i, j int) bool { return result.Records[i].Ts > result.Records[j].Ts })
	return result, nil
}

// peekLastCompleteTs reads only the trailing chunk of the JSONL file at path
// and returns the Ts of the last COMPLETE (newline-terminated) record — a
// torn tail is not a record yet and must not bound the file. ok=false on any
// anomaly — unreadable or empty file, a final record longer than the peek
// chunk, unparseable JSON — in which case the caller must stream the file as
// before.
func peekLastCompleteTs(path string) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return 0, false
	}
	const chunk = 64 << 10
	size := info.Size()
	offset := int64(0)
	if size > chunk {
		offset = size - chunk
	}
	buf := make([]byte, size-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return 0, false
	}
	// Walk back over complete lines (anything after the last '\n' is a torn
	// tail) until a non-empty one parses.
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return 0, false
	}
	line := buf[:end]
	for {
		start := bytes.LastIndexByte(line, '\n')
		candidate := line[start+1:]
		if len(bytes.TrimSpace(candidate)) > 0 {
			if start < 0 && offset > 0 {
				// The record begins before the chunk: do not trust it as a
				// bound.
				return 0, false
			}
			var record struct {
				Ts int64 `json:"ts"`
			}
			if json.Unmarshal(candidate, &record) != nil {
				return 0, false
			}
			return record.Ts, true
		}
		if start < 0 {
			return 0, false
		}
		line = line[:start]
	}
}

// peekFirstCompleteTs reads only the leading chunk of the JSONL file at path
// and returns the Ts of the FIRST complete (newline-terminated) record.
// Together with peekLastCompleteTs it verifies the single-writer append-order
// premise the file-level early termination relies on: firstTs > lastTs means
// a hand-built or corrupted file, and the caller must stream it. ok=false on
// any anomaly — unreadable or empty file, a first record longer than the
// peek chunk (or still unterminated — e.g. the daemon is mid-write into a
// fresh active file), unparseable JSON — in which case the caller must
// stream the file as before.
func peekFirstCompleteTs(path string) (int64, bool) {
	file, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return 0, false
	}
	const chunk = 64 << 10
	size := info.Size()
	if size > chunk {
		size = chunk
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, 0); err != nil {
		return 0, false
	}
	end := bytes.IndexByte(buf, '\n')
	if end < 0 {
		// No complete line within the chunk: the first record is longer
		// than the chunk or still being written — unverifiable.
		return 0, false
	}
	line := bytes.TrimSpace(buf[:end])
	if len(line) == 0 {
		return 0, false
	}
	var record struct {
		Ts int64 `json:"ts"`
	}
	if json.Unmarshal(line, &record) != nil {
		return 0, false
	}
	return record.Ts, true
}

type timestampMinHeap []*Record

func (h timestampMinHeap) Len() int           { return len(h) }
func (h timestampMinHeap) Less(i, j int) bool { return h[i].Ts < h[j].Ts }
func (h timestampMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timestampMinHeap) Push(value any)    { *h = append(*h, value.(*Record)) }
func (h *timestampMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}
