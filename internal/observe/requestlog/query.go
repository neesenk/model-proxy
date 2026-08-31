package requestlog

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Filter narrows a request-log query. Shadow is "", "only", or "exclude".
// UsageOnly strips the request/response bodies and the header blob as records
// are read, after parsing each response body's usage into ParsedUsage — a
// top-K of N records then holds metadata-sized records instead of N full
// bodies (max_body_bytes is 5MiB per side by default, so N=2000 full records
// could transiently pin gigabytes).
type Filter struct {
	Model      string
	Provider   string
	Status     int
	ErrorsOnly bool
	RequestID  string
	Session    string
	Shadow     string
	From       time.Time
	To         time.Time
	Limit      int
	UsageOnly  bool
}

func (f Filter) matches(record Record) bool {
	if f.RequestID != "" && record.RequestID != f.RequestID {
		return false
	}
	if f.Session != "" && record.SessionID != f.Session {
		return false
	}
	if f.Model != "" &&
		!containsFold(record.CalledModel, f.Model) &&
		!containsFold(record.UpstreamModel, f.Model) &&
		!containsFold(record.Exposed, f.Model) {
		return false
	}
	if f.Provider != "" && !containsFold(record.Provider, f.Provider) {
		return false
	}
	if f.Status != 0 && record.Status != f.Status {
		return false
	}
	if f.ErrorsOnly && record.Status < 400 {
		return false
	}
	switch f.Shadow {
	case "only":
		if !record.Shadow {
			return false
		}
	case "exclude":
		if record.Shadow {
			return false
		}
	}
	if !f.From.IsZero() || !f.To.IsZero() {
		timestamp, err := time.Parse(time.RFC3339, record.Ts)
		if err != nil {
			return false
		}
		if !f.From.IsZero() && timestamp.Before(f.From) {
			return false
		}
		if !f.To.IsZero() && timestamp.After(f.To) {
			return false
		}
	}
	return true
}

func containsFold(value, substring string) bool {
	return strings.Contains(strings.ToLower(value), strings.ToLower(substring))
}

// QueryRecords streams every request-log file and returns matching full records
// newest first. A positive limit retains only a timestamp top-K in memory.
//
// Early termination (file granularity): a single writer appends records to one
// file in non-decreasing Ts order, so a file's last admissible record bounds
// everything in it. Once the top-K heap is full, a record strictly older than
// the heap floor is pushed and immediately evicted — a no-op — so a file whose
// newest record is older than the floor cannot change the result and is
// skipped without streaming; likewise a file whose newest record is older than
// the From bound holds no match. The ordering premise is VERIFIED per file,
// not assumed: both the first and the last record are peeked, and any anomaly
// — unopenable file, oversized record, unparseable JSON, or firstTs after
// lastTs (hand-built or corrupted file) — falls back to streaming the file as
// before. Cross-file disorder (an orphan active file or a clock correction
// letting an older-named file hold newer records) stays tolerated: every file
// is peeked independently.
func QueryRecords(dir string, filter Filter) ([]Record, error) {
	return query(dir, filter, false)
}

// peekLastRecordTs reads only the trailing chunk of the JSONL file at path and
// returns the Ts of the last record the streaming scan would admit (the last
// non-empty line, newline-terminated or not — the reader parses an
// unterminated final line too). ok=false on any anomaly — unreadable or empty
// file, a final line longer than the peek chunk, unparseable JSON — in which
// case the caller must stream the file as before.
func peekLastRecordTs(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false
	}
	const chunk = 128 << 10
	size := info.Size()
	offset := int64(0)
	if size > chunk {
		offset = size - chunk
	}
	buf := make([]byte, size-offset)
	if _, err := file.ReadAt(buf, offset); err != nil {
		return "", false
	}
	buf = bytes.TrimRight(buf, "\n")
	if len(buf) == 0 {
		return "", false
	}
	start := bytes.LastIndexByte(buf, '\n')
	if start < 0 && offset > 0 {
		// The final line begins before the chunk (an up-to-max_body_bytes
		// record): its start is unverifiable, so do not trust it as a bound.
		return "", false
	}
	var record struct {
		Ts string `json:"ts"`
	}
	if json.Unmarshal(buf[start+1:], &record) != nil {
		return "", false
	}
	return record.Ts, true
}

// peekFirstRecordTs reads only the leading chunk of the JSONL file at path
// and returns the Ts of the FIRST non-empty line (newline-terminated or not —
// the streaming reader parses an unterminated line too). Together with
// peekLastRecordTs it verifies the single-writer append-order premise the
// file-level early termination relies on: a first record newer than the last
// means a hand-built or corrupted file, and the caller must stream it.
// ok=false on any anomaly — unreadable or empty file, a first line longer
// than the peek chunk, unparseable JSON — in which case the caller must
// stream the file as before.
func peekFirstRecordTs(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return "", false
	}
	const chunk = 128 << 10
	size := info.Size()
	if size > chunk {
		size = chunk
	}
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, 0); err != nil {
		return "", false
	}
	end := bytes.IndexByte(buf, '\n')
	if end < 0 {
		// The first line fills the whole chunk (an up-to-max_body_bytes
		// record): its end is unverifiable, so do not trust it as a bound.
		return "", false
	}
	line := bytes.TrimSpace(buf[:end])
	if len(line) == 0 {
		return "", false
	}
	var record struct {
		Ts string `json:"ts"`
	}
	if json.Unmarshal(line, &record) != nil {
		return "", false
	}
	return record.Ts, true
}

func query(dir string, filter Filter, metadataOnly bool) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "requests-") && strings.HasSuffix(name, ".log") {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	var records []Record
	var newest *timestampMinHeap
	if filter.Limit > 0 {
		newest = &timestampMinHeap{}
	}
	for i := len(names) - 1; i >= 0; i-- {
		path := filepath.Join(dir, names[i])
		lastTs, lastOK := peekLastRecordTs(path)
		firstTs, firstOK := peekFirstRecordTs(path)
		// RFC3339 offsets make lexicographic order unreliable; compare
		// parsed instants. Any parse failure is an anomaly → stream.
		ordered := false
		if lastOK && firstOK {
			if first, err1 := time.Parse(time.RFC3339, firstTs); err1 == nil {
				if last, err2 := time.Parse(time.RFC3339, lastTs); err2 == nil {
					ordered = !first.After(last)
				}
			}
		}
		if ordered {
			// Ordering verified: the file's NEWEST record bounds everything
			// in it. A full heap makes every strictly-older record a
			// push-then-evict no-op, and From is an inclusive lower bound
			// (matches drops only timestamps before From) — either way the
			// whole file is provably irrelevant once that record qualifies.
			if newest != nil && newest.Len() >= filter.Limit && lastTs < (*newest)[0].Ts {
				continue
			}
			if !filter.From.IsZero() {
				if t, err := time.Parse(time.RFC3339, lastTs); err == nil && t.Before(filter.From) {
					continue
				}
			}
		}
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		reader := bufio.NewReaderSize(file, 64*1024)
		for {
			line, readErr := reader.ReadBytes('\n')
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				var record Record
				if json.Unmarshal(trimmed, &record) == nil && filter.matches(record) {
					if metadataOnly || filter.UsageOnly {
						if filter.UsageOnly {
							record.ParsedUsage = ExtractUsage(record.ResponseBody)
						}
						record.RequestBody = ""
						record.ResponseBody = ""
						record.ResponseHeaders = ""
					}
					if newest == nil {
						records = append(records, record)
					} else {
						heap.Push(newest, record)
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
		records = *newest
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Ts > records[j].Ts })
	return records, nil
}

type timestampMinHeap []Record

func (h timestampMinHeap) Len() int           { return len(h) }
func (h timestampMinHeap) Less(i, j int) bool { return h[i].Ts < h[j].Ts }
func (h timestampMinHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *timestampMinHeap) Push(value any)    { *h = append(*h, value.(Record)) }
func (h *timestampMinHeap) Pop() any {
	old := *h
	last := len(old) - 1
	value := old[last]
	*h = old[:last]
	return value
}
