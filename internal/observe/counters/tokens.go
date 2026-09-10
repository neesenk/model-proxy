package counters

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// TokenKey aliases the shared (provider, model) key so existing call sites
// (scanner, tests) read naturally. Persistence is handled by
// internal/observe/stats.Store (SQLite); the JSON file format is gone.
type TokenKey = PMKey

type TokenUsage struct {
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

type TokenCounter struct {
	mu sync.Mutex
	m  map[TokenKey]*TokenUsage
}

func NewTokenCounter() *TokenCounter {
	return &TokenCounter{m: map[TokenKey]*TokenUsage{}}
}

// commit records one observed usage payload under the (provider, model) key.
// All reads and writes of a *TokenUsage's fields happen under tc.mu: the
// get-or-create and the read-modify-write are a single critical section so two
// concurrent scanners committing to the same key cannot lose increments.
func (tc *TokenCounter) Commit(k TokenKey, add TokenUsage) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	u := tc.m[k]
	if u == nil {
		u = &TokenUsage{}
		tc.m[k] = u
	}
	u.Input += add.Input
	u.Output += add.Output
	u.CacheCreation += add.CacheCreation
	u.CacheRead += add.CacheRead
	if add.Input > 0 || add.Output > 0 {
		u.Requests++
	}
}

// snapshot returns a detached copy of all counters. Every field is copied under
// tc.mu; callers may read the returned map without holding the lock.
func (tc *TokenCounter) Snapshot() map[TokenKey]TokenUsage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make(map[TokenKey]TokenUsage, len(tc.m))
	for k, v := range tc.m {
		out[k] = *v
	}
	return out
}

// seed sets a (provider, model) entry to a baseline (boot restore from SQLite).
func (tc *TokenCounter) Seed(k TokenKey, u TokenUsage) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	v := u
	tc.m[k] = &v
}

func (tc *TokenCounter) Reset() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.m = map[TokenKey]*TokenUsage{}
}

const scanLineCap = 64 * 1024

// UsageScanner is a pass-through io.ReadCloser: bytes read from src are returned
// verbatim, and observed incrementally to extract SSE usage events. It never
// modifies, buffers the stream, or blocks the client. Failures are silent (no
// usage recorded). commit happens once on EOF.
type UsageScanner struct {
	src     io.ReadCloser
	key     TokenKey
	tc      *TokenCounter
	onAgent func(TokenUsage) // optional: attribute the same usage to an agent (parallel agent pipeline)
	line    []byte           // current incomplete line (bounded by scanLineCap)
	acc     TokenUsage
	done    bool
}

func NewUsageScanner(src io.ReadCloser, key TokenKey, tc *TokenCounter, onAgent func(TokenUsage)) *UsageScanner {
	return &UsageScanner{src: src, key: key, tc: tc, onAgent: onAgent}
}

func (s *UsageScanner) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.observe(p[:n])
	}
	if err != nil && !s.done {
		s.done = true
		s.Commit()
	}
	return n, err
}

func (s *UsageScanner) Close() error {
	if !s.done {
		s.done = true
		s.Commit()
	}
	return s.src.Close()
}

// commit flushes the accumulated usage to the (provider, model) token counter
// and, if an agent sink is wired, to the agent pipeline too (same bytes, so
// per-agent token totals reconcile with the per-model totals).
func (s *UsageScanner) Commit() {
	s.tc.Commit(s.key, s.acc)
	if s.onAgent != nil && (s.acc.Input > 0 || s.acc.Output > 0) {
		s.onAgent(s.acc)
	}
}

// usageMarker is the JSON key every counted event shape carries. Checking it
// first lets plain delta lines skip both probe parses.
var usageMarker = []byte(`"usage"`)

// observe scans a chunk for complete lines, extracting usage. Partial line bytes
// are held in s.line (capped); an over-long line is flushed (skipped) to bound memory.
func (s *UsageScanner) observe(chunk []byte) {
	for len(chunk) > 0 {
		i := bytes.IndexByte(chunk, '\n')
		segment := chunk
		if i >= 0 {
			segment = chunk[:i]
		}
		if room := scanLineCap - len(s.line); room > 0 {
			if len(segment) > room {
				segment = segment[:room]
			}
			s.line = append(s.line, segment...)
		}
		if i < 0 {
			return
		}
		s.parseLine(s.line)
		s.line = s.line[:0]
		chunk = chunk[i+1:]
	}
}

// parseLine inspects one SSE data line for a usage payload.
func (s *UsageScanner) parseLine(line []byte) {
	trimmed := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	// Pre-filter: every counted event shape carries a "usage" key; a miss
	// (ordinary delta lines) skips both probe parses below.
	if !bytes.Contains(payload, usageMarker) {
		return
	}
	// Try anthropic shapes first, then openai chat, then responses.
	var anth struct {
		Type    string `json:"type"`
		Message struct {
			Usage struct {
				InputTokens         uint64 `json:"input_tokens"`
				CacheCreationTokens uint64 `json:"cache_creation_input_tokens"`
				CacheReadTokens     uint64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Usage struct {
			InputTokens         uint64 `json:"input_tokens"`
			OutputTokens        uint64 `json:"output_tokens"`
			CacheCreationTokens uint64 `json:"cache_creation_input_tokens"`
			CacheReadTokens     uint64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &anth) == nil {
		if anth.Type == "message_start" {
			s.acc.Input += anth.Message.Usage.InputTokens
			s.acc.CacheCreation = max(s.acc.CacheCreation, anth.Message.Usage.CacheCreationTokens)
			s.acc.CacheRead = max(s.acc.CacheRead, anth.Message.Usage.CacheReadTokens)
		}
		if anth.Type == "message_delta" {
			s.acc.Output += anth.Usage.OutputTokens
			// A native anthropic stream carries only output_tokens here, but the
			// protocol-conversion transformer (openai→anthropic) emits input_tokens
			// in message_delta too (openai delivers prompt_tokens at the trailing
			// chunk, after message_start fired). Read it here so converted routes
			// attribute input tokens (otherwise they'd be permanently 0).
			s.acc.Input += anth.Usage.InputTokens
			// Cache buckets merge by per-field MAX, never +=: dialects deliver
			// the real values only here (zhipu/aqp/shopee/kimi-code send zero or
			// null cache usage in message_start), while others repeat the same
			// cumulative value in BOTH frames (deepseek cache_read:256 in
			// message_start and message_delta) — addition would double-count.
			// Same per-field-max rule as requestlog's extractSSEUsage.
			s.acc.CacheCreation = max(s.acc.CacheCreation, anth.Usage.CacheCreationTokens)
			s.acc.CacheRead = max(s.acc.CacheRead, anth.Usage.CacheReadTokens)
		}
	}
	var oai struct {
		Usage *struct {
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
			PromptDetails    *struct {
				CachedTokens uint64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			PromptCacheHit uint64 `json:"prompt_cache_hit_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &oai) == nil && oai.Usage != nil {
		s.acc.Input += oai.Usage.PromptTokens
		s.acc.Output += oai.Usage.CompletionTokens
		// Cached prompt tokens ride in prompt_tokens_details (openai contract);
		// deepseek additionally spells them prompt_cache_hit_tokens — details win
		// when both are present. Max-merge like the anthropic cache buckets: a
		// usage chunk repeats cumulative counters, never deltas.
		cached := oai.Usage.PromptCacheHit
		if oai.Usage.PromptDetails != nil {
			cached = oai.Usage.PromptDetails.CachedTokens
		}
		s.acc.CacheRead = max(s.acc.CacheRead, cached)
	}
	// /v1/responses client streams carry one cumulative snapshot in
	// response.completed (usage is null on in_progress frames); every field
	// merges by max, mirroring extractSSEUsage.
	var resp struct {
		Response *struct {
			Usage *struct {
				InputTokens  uint64 `json:"input_tokens"`
				OutputTokens uint64 `json:"output_tokens"`
				InputDetails *struct {
					CachedTokens uint64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &resp) == nil && resp.Response != nil && resp.Response.Usage != nil {
		s.acc.Input = max(s.acc.Input, resp.Response.Usage.InputTokens)
		s.acc.Output = max(s.acc.Output, resp.Response.Usage.OutputTokens)
		if resp.Response.Usage.InputDetails != nil {
			s.acc.CacheRead = max(s.acc.CacheRead, resp.Response.Usage.InputDetails.CachedTokens)
		}
	}
}
