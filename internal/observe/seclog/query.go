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
// many unreadable lines were skipped while scanning.
type Result struct {
	Records []*Record
	Skipped int
}

// Query streams every audit-log file in dir (active and rotated) and returns
// matching records newest first. A positive limit retains only a timestamp
// top-K in memory. Unparseable lines are skipped and counted in Skipped, as
// are files that cannot be opened at all. One exception: a final line with no
// trailing newline that ends at a clean EOF is a torn tail — the daemon is
// mid-write into the active file — and is ignored silently instead of being
// counted as unreadable.
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
		if isLogFile(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	result := &Result{}
	var newest *timestampMinHeap
	if filter.Limit > 0 {
		newest = &timestampMinHeap{}
	}
	for i := len(names) - 1; i >= 0; i-- {
		file, err := os.Open(filepath.Join(dir, names[i]))
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
	sort.SliceStable(result.Records, func(i, j int) bool { return result.Records[i].Ts > result.Records[j].Ts })
	return result, nil
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
