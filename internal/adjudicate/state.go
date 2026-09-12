// state.go — the persisted pieces of the adjudication service: the verdict
// cache (hash → verdict, LRU, survives restarts), the session block table
// (persists until explicitly unblocked), and the in-memory result ring.
package adjudicate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// statePath derives <stateDir>/<name>; an empty dir keeps the state in
// memory only (tests, degenerate setups).
func statePath(dir, name string) string {
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, name)
}

// writeStateFile persists v atomically (unique temp file + fsync + rename,
// the quota_state.json recipe) with owner-only permissions. An empty path is
// a no-op (in-memory mode).
func writeStateFile(path string, v any) error {
	if path == "" {
		return nil
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".guard-adjudicate-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	remove := func() { os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		remove()
		return err
	}
	return nil
}

// ---- verdict cache ----------------------------------------------------

// verdictEntry is one cached verdict.
type verdictEntry struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason,omitempty"`
	Model   string `json:"model,omitempty"`
	Ts      int64  `json:"ts"`
}

// verdictFile is the on-disk form of guard_verdicts.json.
type verdictFile struct {
	Version int                     `json:"version"`
	Entries map[string]verdictEntry `json:"entries"`
}

// verdictCache is an LRU over the persisted verdicts. Keys are content
// hashes (CacheKey); values never contain the matched bytes.
type verdictCache struct {
	mu    sync.Mutex
	path  string
	max   int
	order []string // LRU: oldest first
	ents  map[string]verdictEntry
}

// loadVerdictCache reads the persisted cache; missing/corrupt files start
// empty (a cache, never a correctness dependency).
func loadVerdictCache(path string, max int) *verdictCache {
	c := &verdictCache{path: path, max: max, ents: map[string]verdictEntry{}}
	if path == "" {
		return c
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return c
	}
	var f verdictFile
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return c
	}
	for k, v := range f.Entries {
		if v.Verdict != VerdictHigh && v.Verdict != VerdictLow {
			continue
		}
		c.ents[k] = v
	}
	c.reorder()
	c.evict()
	return c
}

// reorder rebuilds the LRU order oldest-first by Ts (stable for ties).
func (c *verdictCache) reorder() {
	c.order = c.order[:0]
	for k := range c.ents {
		c.order = append(c.order, k)
	}
	sort.Slice(c.order, func(i, j int) bool {
		if c.ents[c.order[i]].Ts != c.ents[c.order[j]].Ts {
			return c.ents[c.order[i]].Ts < c.ents[c.order[j]].Ts
		}
		return c.order[i] < c.order[j]
	})
}

// evict drops the oldest entries past max.
func (c *verdictCache) evict() {
	for len(c.order) > c.max {
		delete(c.ents, c.order[0])
		c.order = c.order[1:]
	}
}

func (c *verdictCache) get(key string) (verdictEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.ents[key]
	if !ok {
		return verdictEntry{}, false
	}
	// refresh LRU position
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, key)
	return v, true
}

func (c *verdictCache) put(key string, v verdictEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.ents[key]; !ok {
		c.order = append(c.order, key)
	}
	c.ents[key] = v
	c.evict()
	_ = writeStateFile(c.path, verdictFile{Version: 1, Entries: c.ents})
}

func (c *verdictCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ents)
}

// ---- session blocks ---------------------------------------------------

// Block is one blocked session (high verdict + block_session). It persists
// across restarts and clears only via Unblock.
type Block struct {
	Kind      string `json:"kind"`
	Rule      string `json:"rule"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Ts        int64  `json:"ts"`
}

// blockFile is the on-disk form of guard_blocks.json.
type blockFile struct {
	Version int              `json:"version"`
	Blocks  map[string]Block `json:"blocks"`
}

// blockStore is the persisted session block table.
type blockStore struct {
	mu     sync.Mutex
	path   string
	blocks map[string]Block
}

// loadBlockStore reads the persisted blocks; missing/corrupt files start
// empty (fail-open: a lost block table degrades to no interception, never
// to a permanent lockout).
func loadBlockStore(path string) *blockStore {
	b := &blockStore{path: path, blocks: map[string]Block{}}
	if path == "" {
		return b
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return b
	}
	var f blockFile
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return b
	}
	b.blocks = f.Blocks
	return b
}

func (b *blockStore) Blocked(sessionID string) (Block, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bl, ok := b.blocks[sessionID]
	return bl, ok
}

func (b *blockStore) Block(sessionID string, bl Block) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.blocks[sessionID] = bl
	_ = writeStateFile(b.path, blockFile{Version: 1, Blocks: b.blocks})
}

func (b *blockStore) Unblock(sessionID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.blocks[sessionID]; !ok {
		return false
	}
	delete(b.blocks, sessionID)
	_ = writeStateFile(b.path, blockFile{Version: 1, Blocks: b.blocks})
	return true
}

// BlockEntry is one block plus its session key (the map key re-attached for
// the list surfaces). Block is embedded so the JSON form stays flat — this
// struct is the admin/API DTO shape (aliased by appapi.SecurityBlock), not
// just an internal pairing.
type BlockEntry struct {
	SessionID string `json:"session_id"`
	Block
}

func (b *blockStore) Snapshot() []BlockEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]BlockEntry, 0, len(b.blocks))
	for sid, bl := range b.blocks {
		out = append(out, BlockEntry{SessionID: sid, Block: bl})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Block.Ts != out[j].Block.Ts {
			return out[i].Block.Ts > out[j].Block.Ts
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// ---- result ring --------------------------------------------------------

// resultRing is a fixed-size newest-last ring of recent adjudications.
type resultRing struct {
	mu   sync.Mutex
	buf  []Result
	size int
}

func newResultRing(size int) *resultRing {
	return &resultRing{buf: make([]Result, 0, size), size: size}
}

func (r *resultRing) add(res Result) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.buf) >= r.size {
		r.buf = r.buf[len(r.buf)-r.size+1:]
	}
	r.buf = append(r.buf, res)
}

func (r *resultRing) Snapshot() []Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Result, len(r.buf))
	for i, res := range r.buf {
		out[i] = res
	}
	// newest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
