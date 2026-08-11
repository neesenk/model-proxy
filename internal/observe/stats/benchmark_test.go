package stats

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkStatsFlush measures the minute upsert path at 200 provider/model
// streams, a generous upper bound for one proxy minute.
func BenchmarkStatsFlush(b *testing.B) {
	store, err := Open(Options{Path: filepath.Join(b.TempDir(), "stats.db")})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	deltas := make(map[Key]Counters, 200)
	for providerIndex := 0; providerIndex < 10; providerIndex++ {
		for modelIndex := 0; modelIndex < 20; modelIndex++ {
			key := Key{
				Provider: fmt.Sprintf("prov%d", providerIndex),
				Model:    fmt.Sprintf("model%d", modelIndex),
			}
			deltas[key] = Counters{
				Requests:  3,
				Failovers: 1,
				Input:     1200,
				Output:    300,
			}
		}
	}

	minute := time.Now().Unix() / 60 * 60
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		minute += 60
		if err := store.Flush(minute, deltas); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStatsQueryRange measures a one-hour raw query against a
// representative 200-stream, 1,000-minute history.
func BenchmarkStatsQueryRange(b *testing.B) {
	store, err := Open(Options{Path: filepath.Join(b.TempDir(), "stats.db")})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	const (
		keyCount    = 200
		minuteCount = 1000
	)
	base := time.Now().Unix()/60*60 - int64(minuteCount)*60
	for minuteIndex := 0; minuteIndex < minuteCount; minuteIndex++ {
		minute := base + int64(minuteIndex)*60
		deltas := make(map[Key]Counters, keyCount)
		for providerIndex := 0; providerIndex < 10; providerIndex++ {
			for modelIndex := 0; modelIndex < 20; modelIndex++ {
				key := Key{
					Provider: fmt.Sprintf("prov%d", providerIndex),
					Model:    fmt.Sprintf("model%d", modelIndex),
				}
				deltas[key] = Counters{Requests: 2, Input: 800}
			}
		}
		if err := store.Flush(minute, deltas); err != nil {
			b.Fatal(err)
		}
	}

	from := base + int64(minuteCount/2)*60
	to := from + 60*60
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if _, err := store.QueryRange(from, to, "", "", 60); err != nil {
			b.Fatal(err)
		}
	}
}
