// state.go — the persisted pieces of the adjudication service: the verdict
// cache (hash → verdict, LRU, survives restarts), the session block table
// (persists until explicitly unblocked), the cumulative LLM usage counters,
// and the in-memory result ring.
package adjudicate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
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
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Model    string `json:"model,omitempty"`
	Ts       int64  `json:"ts"`
}

// verdictFile is the on-disk form of guard_verdicts.json.
type verdictFile struct {
	Version int                     `json:"version"`
	Entries map[string]verdictEntry `json:"entries"`
}

// verdictCache is an LRU over the persisted verdicts. Keys are content
// hashes (CacheKey); values never contain the matched bytes.
type verdictCache struct {
	mu      sync.Mutex
	flushMu sync.Mutex // serializes disk writes so persistence never regresses
	path    string
	max     int
	order   []string // LRU: oldest first
	ents    map[string]verdictEntry
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
		if v.Verdict != VerdictHigh && v.Verdict != VerdictMedium && v.Verdict != VerdictLow {
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
	if _, ok := c.ents[key]; !ok {
		c.order = append(c.order, key)
	}
	c.ents[key] = v
	c.evict()
	c.mu.Unlock()
	c.flush()
}

// flush writes the current memory state to disk. Callers must have just
// mutated the cache; the actual I/O happens outside c.mu so readers are not
// blocked by fsync.
func (c *verdictCache) flush() {
	if c.path == "" {
		return
	}
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	c.mu.Lock()
	f := verdictFile{Version: 1, Entries: make(map[string]verdictEntry, len(c.ents))}
	for k, v := range c.ents {
		f.Entries[k] = v
	}
	c.mu.Unlock()
	_ = writeStateFile(c.path, f)
}

func (c *verdictCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ents)
}

// remove drops cache entries by key (the operator Unblock cascade) and
// persists; absent keys are no-ops.
func (c *verdictCache) remove(keys ...string) {
	c.mu.Lock()
	changed := false
	for _, k := range keys {
		if _, ok := c.ents[k]; ok {
			delete(c.ents, k)
			changed = true
		}
	}
	if !changed {
		c.mu.Unlock()
		return
	}
	c.reorder()
	c.mu.Unlock()
	c.flush()
}

// ---- session blocks ---------------------------------------------------

// Block is one blocked session (high verdict + block_session). It persists
// across restarts and clears only via Unblock. ContentHashes/CacheKeys
// record the verdict's enforcement artifacts (repeat-index entries +
// verdict-cache keys) so an operator Unblock can cascade: releasing the
// session also releases the same content from re-interception (the operator
// unblock IS the final risk judgment — the judge never re-litigates it).
type Block struct {
	Kind      string `json:"kind"`
	Rule      string `json:"rule"`
	Reason    string `json:"reason,omitempty"`
	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	Ts        int64  `json:"ts"`
	// Rules records every exact-match rule that contributed to the block.
	// Rule holds the first (display) rule for backward compatibility; Rules
	// is the full set used by the unblock audit trail.
	Rules []string `json:"rules,omitempty"`
	// ContentHashes: sha256(hit) per secret hit behind the verdict (empty
	// for channels without hit bytes, e.g. exact known-secret matches).
	ContentHashes []string `json:"content_hashes,omitempty"`
	// CacheKeys: the verdict-cache keys behind the verdict.
	CacheKeys []string `json:"cache_keys,omitempty"`
}

// blockFile is the on-disk form of guard_blocks.json.
type blockFile struct {
	Version int              `json:"version"`
	Blocks  map[string]Block `json:"blocks"`
}

// blockStore is the persisted session block table.
type blockStore struct {
	mu      sync.Mutex
	flushMu sync.Mutex // serializes disk writes; I/O never happens under mu
	path    string
	blocks  map[string]Block
	// loadedAt/lastDisk fence the restart drain race: a SIGINT'd process
	// stops its listener (the port frees and a successor may boot and load
	// the file) BEFORE its workers drain, so a late high verdict can hit the
	// file after this store already loaded it. flush adopts disk entries that
	// appeared SINCE the last state we knew (absent here AND absent from
	// lastDisk, Ts newer than our load) instead of clobbering them. Entries
	// that were in lastDisk but not in memory are OUR OWN removals (Unblock)
	// and must stay removed.
	loadedAt time.Time
	lastDisk map[string]Block
}

// loadBlockStore reads the persisted blocks; missing/corrupt files start
// empty (fail-open: a lost block table degrades to no interception, never
// to a permanent lockout).
func loadBlockStore(path string) *blockStore {
	b := &blockStore{path: path, blocks: map[string]Block{}, loadedAt: time.Now(), lastDisk: map[string]Block{}}
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
	b.lastDisk = make(map[string]Block, len(f.Blocks))
	for k, v := range f.Blocks {
		b.lastDisk[k] = v
	}
	return b
}

// flush writes the table, first adopting disk entries that appeared since
// the last state we knew (the drain-race writes of a SIGINT'd predecessor —
// see the struct comment). All disk I/O happens outside b.mu; the lock is
// only held for short memory/fence snapshots and updates.
func (b *blockStore) flush() {
	if b.path == "" {
		return
	}
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	// 1. Snapshot memory and fences under lock.
	b.mu.Lock()
	mem := make(map[string]Block, len(b.blocks))
	for k, v := range b.blocks {
		mem[k] = v
	}
	lastDisk := make(map[string]Block, len(b.lastDisk))
	for k, v := range b.lastDisk {
		lastDisk[k] = v
	}
	loadedMs := b.loadedAt.UnixMilli()
	b.mu.Unlock()

	// 2. Read disk outside lock.
	var disk blockFile
	if data, err := os.ReadFile(b.path); err == nil {
		if json.Unmarshal(data, &disk) != nil || disk.Version != 1 {
			disk.Blocks = map[string]Block{}
		}
	} else {
		disk.Blocks = map[string]Block{}
	}

	// 3. Compute late-predecessor adopts.
	adopted := make(map[string]Block)
	for sid, bl := range disk.Blocks {
		if _, ours := mem[sid]; ours {
			continue
		}
		if _, known := lastDisk[sid]; known {
			continue // we removed it ourselves (Unblock)
		}
		if bl.Ts > loadedMs {
			adopted[sid] = bl
		}
	}

	// 4. Apply adopts to current memory (without overwriting concurrent
	// mutations) and re-snapshot under lock.
	if len(adopted) > 0 {
		b.mu.Lock()
		for sid, bl := range adopted {
			if _, exists := b.blocks[sid]; !exists {
				b.blocks[sid] = bl
				mem[sid] = bl
			}
		}
		// Re-snapshot in case another goroutine added/removed entries while
		// we were reading disk.
		mem = make(map[string]Block, len(b.blocks))
		for k, v := range b.blocks {
			mem[k] = v
		}
		b.mu.Unlock()
	}

	// 5. Write merged snapshot outside lock (copy the map so the marshal
	// cannot race with later mutations).
	snap := make(map[string]Block, len(mem))
	for k, v := range mem {
		snap[k] = v
	}
	if err := writeStateFile(b.path, blockFile{Version: 1, Blocks: snap}); err == nil {
		// 6. Update lastDisk under lock.
		b.mu.Lock()
		b.lastDisk = make(map[string]Block, len(mem))
		for k, v := range mem {
			b.lastDisk[k] = v
		}
		b.mu.Unlock()
	}
}

func (b *blockStore) Blocked(sessionID string) (Block, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bl, ok := b.blocks[sessionID]
	return bl, ok
}

func (b *blockStore) Block(sessionID string, bl Block) {
	b.mu.Lock()
	// A refresh that carries no artifact hashes (e.g. the exact-match
	// channel) must not strip the hashes an earlier verdict recorded —
	// otherwise a later Unblock could no longer cascade.
	if prev, ok := b.blocks[sessionID]; ok {
		bl.ContentHashes = unionStrings(prev.ContentHashes, bl.ContentHashes)
		bl.CacheKeys = unionStrings(prev.CacheKeys, bl.CacheKeys)
		// Preserve the first recorded display attribution; only hashes/keys
		// are refreshed by later verdicts for the same session.
		if bl.Kind == "" {
			bl.Kind = prev.Kind
		}
		if bl.Rule == "" {
			bl.Rule = prev.Rule
		}
		if len(bl.Rules) == 0 {
			bl.Rules = prev.Rules
		}
		if bl.Reason == "" {
			bl.Reason = prev.Reason
		}
		if bl.Model == "" {
			bl.Model = prev.Model
		}
		if bl.RequestID == "" {
			bl.RequestID = prev.RequestID
		}
		if bl.Ts == 0 {
			bl.Ts = prev.Ts
		}
	}
	b.blocks[sessionID] = bl
	b.mu.Unlock()
	b.flush()
}

// unionStrings merges two hash/key sets preserving order (first-seen first).
func unionStrings(a, b []string) []string {
	if len(a) == 0 {
		return b
	}
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range append(append([]string{}, a...), b...) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Unblock removes one session block and returns the removed entry so the
// caller can record the unblock with its original attribution; false when the
// session was not blocked.
func (b *blockStore) Unblock(sessionID string) (Block, bool) {
	b.mu.Lock()
	bl, ok := b.blocks[sessionID]
	if !ok {
		b.mu.Unlock()
		return Block{}, false
	}
	delete(b.blocks, sessionID)
	b.mu.Unlock()
	b.flush()
	return bl, true
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
	out := make([]BlockEntry, 0, len(b.blocks))
	for sid, bl := range b.blocks {
		out = append(out, BlockEntry{SessionID: sid, Block: bl})
	}
	b.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Block.Ts != out[j].Block.Ts {
			return out[i].Block.Ts > out[j].Block.Ts
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// ---- llm usage stats ----------------------------------------------------

// usageStatsFile is the on-disk form of guard_stats.json: the judge channel's
// cumulative LLM usage plus the cumulative suppressed-low count. Loaded at
// start, extended after every real model call / low verdict — the counters
// are operational accounting (the Security page's llm and low tiles), not a
// cache, so losing the file loses history but never behavior.
type usageStatsFile struct {
	Version      int   `json:"version"`
	Calls        int64 `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	LowVerdicts  int64 `json:"low_verdicts"`
}

// loadUsageStats reads the persisted counters; missing/corrupt files start
// at zero.
func loadUsageStats(path string) (calls, inputTokens, outputTokens, lowVerdicts int64) {
	if path == "" {
		return 0, 0, 0, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, 0
	}
	var f usageStatsFile
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return 0, 0, 0, 0
	}
	return f.Calls, f.InputTokens, f.OutputTokens, f.LowVerdicts
}

// writeUsageStats persists the counters. Called under the owner's statsMu so
// concurrent workers' writes stay ordered with their increments (an unlocked
// write could let a slower earlier snapshot overwrite a newer one).
func writeUsageStats(path string, calls, inputTokens, outputTokens, lowVerdicts int64) {
	_ = writeStateFile(path, usageStatsFile{
		Version:      1,
		Calls:        calls,
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		LowVerdicts:  lowVerdicts,
	})
}

// ---- blocked content ----------------------------------------------------

// BlockedContent is the attribution of hit bytes that were adjudicated HIGH:
// enough for the forward path to intercept repeats verbatim (same treatment
// as the known-secret exact channel) and for the audit record to carry the
// original judgment. It never contains the hit bytes themselves — the store
// is keyed by their sha256 (credential red line: matched bytes never persist).
type BlockedContent struct {
	Kind     string `json:"kind"`
	Rule     string `json:"rule"`
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	Model    string `json:"model,omitempty"`
	Ts       int64  `json:"ts"`
}

// blockedContentFile is the on-disk form of guard_blocked.json.
type blockedContentFile struct {
	Version int                       `json:"version"`
	Entries map[string]BlockedContent `json:"entries"`
}

// blockedContentStore is the persisted repeat-interception index. Entries are
// added on every high verdict (fresh or cached replay) and survive restarts;
// the oldest entries evict past max.
type blockedContentStore struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	path    string
	max     int
	ents    map[string]BlockedContent
}

func loadBlockedContentStore(path string, max int) *blockedContentStore {
	s := &blockedContentStore{path: path, max: max, ents: map[string]BlockedContent{}}
	if path == "" || max <= 0 {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f blockedContentFile
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return s
	}
	for k, v := range f.Entries {
		s.ents[k] = v
	}
	s.evictLocked()
	return s
}

func (s *blockedContentStore) evictLocked() {
	if len(s.ents) <= s.max {
		return
	}
	type kt struct {
		k  string
		ts int64
	}
	order := make([]kt, 0, len(s.ents))
	for k, v := range s.ents {
		order = append(order, kt{k, v.Ts})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ts < order[j].ts })
	for len(s.ents) > s.max {
		delete(s.ents, order[0].k)
		order = order[1:]
	}
}

// hashHit derives the store key: sha256 of the raw hit bytes only — the same
// bytes in any future request must intercept regardless of which rule or
// judge model produced the verdict.
func hashHit(hit string) string {
	sum := sha256.Sum256([]byte(hit))
	return hex.EncodeToString(sum[:])
}

// Record adds (or refreshes) one entry and persists.
func (s *blockedContentStore) Record(hit string, v BlockedContent) {
	key := hashHit(hit)
	s.mu.Lock()
	s.ents[key] = v
	s.evictLocked()
	s.mu.Unlock()
	s.flush()
}

// Remove deletes one entry by its sha256 key (the operator Unblock cascade)
// and reports whether it existed.
func (s *blockedContentStore) Remove(hash string) bool {
	s.mu.Lock()
	if _, ok := s.ents[hash]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.ents, hash)
	s.mu.Unlock()
	s.flush()
	return true
}

func (s *blockedContentStore) flush() {
	if s.path == "" {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	f := blockedContentFile{Version: 1, Entries: make(map[string]BlockedContent, len(s.ents))}
	for k, v := range s.ents {
		f.Entries[k] = v
	}
	s.mu.Unlock()
	_ = writeStateFile(s.path, f)
}

// Entry returns one entry by hash (attribution for the repeat re-block path).
func (s *blockedContentStore) Entry(hash string) (BlockedContent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ents[hash]
	return v, ok
}

// Snapshot lists the index newest first (audit surfaces; hashes only).
func (s *blockedContentStore) Snapshot() []BlockedContentEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]BlockedContentEntry, 0, len(s.ents))
	for k, v := range s.ents {
		out = append(out, BlockedContentEntry{Hash: k, BlockedContent: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	return out
}

// BlockedContentEntry is one repeat-interception entry with its key (the
// admin/API DTO shape).
type BlockedContentEntry struct {
	Hash string `json:"hash"`
	BlockedContent
}

// ---- allowed content (operator override) ---------------------------------

// AllowedContent is an operator-reviewed hit: unblocking a session whose
// verdict produced repeat-interception entries cascades the release to the
// content itself. The judge channel never re-litigates allowed bytes — the
// operator unblock is the final risk judgment. Like every persisted guard
// artifact it stores hashes only, never the hit bytes.
type AllowedContent struct {
	Kind   string `json:"kind"`
	Rule   string `json:"rule,omitempty"`
	Reason string `json:"reason,omitempty"`
	Source string `json:"source,omitempty"` // how the allow was granted (e.g. session-unblock)
	Ts     int64  `json:"ts"`
}

type allowedContentFile struct {
	Version int                       `json:"version"`
	Entries map[string]AllowedContent `json:"entries"`
}

// allowedContentStore is the persisted operator-override table, keyed by the
// same sha256(hit) the repeat index uses so a cascade can move entries
// verbatim. Oldest entries evict past max.
type allowedContentStore struct {
	mu      sync.Mutex
	flushMu sync.Mutex
	path    string
	max     int
	ents    map[string]AllowedContent
}

func loadAllowedContentStore(path string, max int) *allowedContentStore {
	s := &allowedContentStore{path: path, max: max, ents: map[string]AllowedContent{}}
	if path == "" || max <= 0 {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var f allowedContentFile
	if json.Unmarshal(data, &f) != nil || f.Version != 1 {
		return s
	}
	for k, v := range f.Entries {
		s.ents[k] = v
	}
	s.evictLocked()
	return s
}

func (s *allowedContentStore) evictLocked() {
	if len(s.ents) <= s.max {
		return
	}
	type kt struct {
		k  string
		ts int64
	}
	order := make([]kt, 0, len(s.ents))
	for k, v := range s.ents {
		order = append(order, kt{k, v.Ts})
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ts < order[j].ts })
	for len(s.ents) > s.max {
		delete(s.ents, order[0].k)
		order = order[1:]
	}
}

// Allowed reports whether these hit bytes carry an operator override.
func (s *allowedContentStore) Allowed(hit string) (AllowedContent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ents[hashHit(hit)]
	return v, ok
}

// AllowHash records an override for an already-hashed entry (the Unblock
// cascade moves repeat-index keys verbatim).
func (s *allowedContentStore) AllowHash(hash string, v AllowedContent) {
	if hash == "" {
		return
	}
	s.mu.Lock()
	s.ents[hash] = v
	s.evictLocked()
	s.mu.Unlock()
	s.flush()
}

// Remove revokes one override (hash key); enforcement falls back to fresh
// adjudication on the content's next occurrence.
func (s *allowedContentStore) Remove(hash string) bool {
	s.mu.Lock()
	if _, ok := s.ents[hash]; !ok {
		s.mu.Unlock()
		return false
	}
	delete(s.ents, hash)
	s.mu.Unlock()
	s.flush()
	return true
}

func (s *allowedContentStore) flush() {
	if s.path == "" {
		return
	}
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	f := allowedContentFile{Version: 1, Entries: make(map[string]AllowedContent, len(s.ents))}
	for k, v := range s.ents {
		f.Entries[k] = v
	}
	s.mu.Unlock()
	_ = writeStateFile(s.path, f)
}

// AllowedEntry is one override plus its key (the admin/API DTO shape).
type AllowedEntry struct {
	Hash string `json:"hash"`
	AllowedContent
}

func (s *allowedContentStore) Snapshot() []AllowedEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]AllowedEntry, 0, len(s.ents))
	for k, v := range s.ents {
		out = append(out, AllowedEntry{Hash: k, AllowedContent: v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	return out
}

// Blocked looks the raw hit bytes up.
func (s *blockedContentStore) Blocked(hit string) (BlockedContent, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.ents[hashHit(hit)]
	return v, ok
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
