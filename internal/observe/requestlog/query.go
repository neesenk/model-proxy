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
func QueryRecords(dir string, filter Filter) ([]Record, error) {
	return query(dir, filter, false)
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
		file, err := os.Open(filepath.Join(dir, names[i]))
		if err != nil {
			continue
		}
		reader := bufio.NewReaderSize(file, 64*1024)
		for {
			line, readErr := reader.ReadBytes('\n')
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				var record Record
				if json.Unmarshal(trimmed, &record) == nil && filter.matches(record) {
					if metadataOnly {
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
