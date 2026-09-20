package catalog

import (
	"os"
	"sync"
	"time"
)

// DiskCache is a memoized disk-only loader for the models.dev cache file. It
// serves read-only status surfaces (the /api/models projection polls every 5s)
// where a full decode of the cache per hit is MB-scale waste. Load is keyed to
// the file's (mtime, size): the atomic-write refresh always lands a new file
// image, so the poll after `models pull` picks the change up. A missing,
// unreadable, or corrupt cache yields nil — the caller's zero status. Safe for
// concurrent use; the returned *Catalog is immutable with copy-safe accessors,
// so callers may share it across requests.
type DiskCache struct {
	mu      sync.Mutex
	filled  bool
	modTime time.Time
	size    int64
	cat     *Catalog
}

// NewDiskCache creates an empty memo. One instance per long-lived read surface
// (the daemon's admin service); a fresh instance per test keeps cases
// independent.
func NewDiskCache() *DiskCache { return &DiskCache{} }

// Load reads the cache file at path — no network, no TTL check — and returns
// the memoized catalog while the file's (mtime, size) identity is unchanged.
func (d *DiskCache) Load(path string) *Catalog {
	info, err := os.Stat(path)
	if err != nil {
		// Absent (or unreadable) cache: nothing worth memoizing — the stat
		// already was the whole cost.
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.filled && d.modTime.Equal(info.ModTime()) && d.size == info.Size() {
		return d.cat
	}
	cat, loadErr := load(path)
	if loadErr != nil {
		cat = nil
	}
	d.filled = true
	d.modTime, d.size = info.ModTime(), info.Size()
	d.cat = cat
	return cat
}
