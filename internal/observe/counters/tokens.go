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

// jsonBodyCap bounds the retained body used by the non-stream JSON usage
// fallback (Commit). Non-stream LLM responses carry usage as the last field,
// so a truncated body never parses — larger bodies simply behave as before
// (no usage recorded), with no retention growth past the cap.
const jsonBodyCap = 4 << 20

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
	// anth/oai carry the per-shape snapshot-vs-delta merge state (see
	// usageShapeState); responses snapshots merge by max inline.
	anth, oai usageShapeState
	done      bool

	// Non-stream fallback: when the first payload byte is '{' the body is a
	// plain JSON response (not SSE), so it is retained (capped) for one whole-
	// body usage parse at Commit. sseUsage marks that an SSE usage payload was
	// already counted — the fallback then stays off (no double count).
	sniffed  bool
	jsonMode bool
	sseUsage bool
	raw      []byte
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
	if !s.sseUsage && s.jsonMode {
		s.parseJSONUsage()
	}
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
	if !s.sniffed {
		s.sniffed = true
		// JSON responses start with '{' (possibly after whitespace/BOM); SSE
		// streams start with data:/event:/id:/retry: or a ':' comment. Only
		// JSON bodies are retained for the Commit-time fallback.
		trimmed := bytes.TrimLeft(chunk, " \t\r\n\xef\xbb\xbf")
		s.jsonMode = len(trimmed) > 0 && trimmed[0] == '{'
	}
	if s.jsonMode && len(s.raw) < jsonBodyCap {
		if room := jsonBodyCap - len(s.raw); room > 0 {
			segment := chunk
			if len(segment) > room {
				segment = segment[:room]
			}
			s.raw = append(s.raw, segment...)
		}
	}
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

// usageShapeState applies one merge rule per wire shape (anthropic frames vs
// openai usage chunks vs responses snapshots): a value that does not decrease
// is a CUMULATIVE snapshot — it replaces the running total; a decrease is a
// DELTA — it adds. Gateways exist that repeat cumulative counters on every
// usage-bearing frame (step-plan emits openai usage per chunk — plain
// addition inflated one request into hundreds of millions of tokens), while
// converted routes emit genuine per-shape deltas. Cross-shape totals still
// sum (an anthropic message_start input plus an openai trailing chunk are
// separate reports).
type usageShapeState struct {
	lastIn, lastOut uint64
}

// mergeField folds one frame's counters into the running accumulators.
// Zero-valued fields are skipped (frames routinely carry only some fields).
func (st *usageShapeState) mergeField(inAcc *uint64, in uint64, outAcc *uint64, out uint64) {
	if in > 0 {
		if in >= st.lastIn {
			*inAcc = in - st.lastIn + *inAcc // snapshot: replace the increment
		} else {
			*inAcc += in // delta
		}
		st.lastIn = in
	}
	if out > 0 {
		if out >= st.lastOut {
			*outAcc = out - st.lastOut + *outAcc
		} else {
			*outAcc += out
		}
		st.lastOut = out
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
			s.sseUsage = true
			s.anth.mergeField(&s.acc.Input, anth.Message.Usage.InputTokens, nil, 0)
			s.acc.CacheCreation = max(s.acc.CacheCreation, anth.Message.Usage.CacheCreationTokens)
			s.acc.CacheRead = max(s.acc.CacheRead, anth.Message.Usage.CacheReadTokens)
		}
		if anth.Type == "message_delta" {
			s.sseUsage = true
			s.anth.mergeField(nil, 0, &s.acc.Output, anth.Usage.OutputTokens)
			// A native anthropic stream carries only output_tokens here, but the
			// protocol-conversion transformer (openai→anthropic) emits input_tokens
			// in message_delta too (openai delivers prompt_tokens at the trailing
			// chunk, after message_start fired). Read it here so converted routes
			// attribute input tokens (otherwise they'd be permanently 0).
			s.anth.mergeField(&s.acc.Input, anth.Usage.InputTokens, nil, 0)
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
				CachedTokens    uint64 `json:"cached_tokens"`
				CacheWriteToken uint64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			PromptCacheHit  uint64 `json:"prompt_cache_hit_tokens"`
			CacheCreationIn uint64 `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &oai) == nil && oai.Usage != nil {
		s.sseUsage = true
		// Cache shares ride inside the inclusive prompt_tokens and are split
		// out before merging — the buckets are anthropic-normalized (input
		// excludes cache; the same ledger as requestlog.ExtractUsage and
		// fusion.ParseUsage). Hit spellings: prompt_tokens_details.cached_tokens
		// (openai contract) and prompt_cache_hit_tokens (deepseek dialect) are
		// max-merged — same source, same value; write spellings:
		// prompt_tokens_details.cache_write_tokens (the converter emission
		// convention, mirrored from convert_stream.go's read side) and the
		// anthropic direct spelling.
		cached, written := uint64(0), uint64(0)
		if oai.Usage.PromptDetails != nil {
			cached = oai.Usage.PromptDetails.CachedTokens
			written = oai.Usage.PromptDetails.CacheWriteToken
		}
		cached = max(cached, oai.Usage.PromptCacheHit)
		written = max(written, oai.Usage.CacheCreationIn)
		s.acc.CacheRead = max(s.acc.CacheRead, cached)
		s.acc.CacheCreation = max(s.acc.CacheCreation, written)
		// Max-merge the shares like the anthropic cache buckets: a usage chunk
		// repeats cumulative counters, never deltas. Landing the shares in both
		// Input and the cache buckets deflated the analytics cache-hit rate and
		// double-billed cost.
		s.oai.mergeField(&s.acc.Input, subClamped2(oai.Usage.PromptTokens, cached, written), &s.acc.Output, oai.Usage.CompletionTokens)
	}
	// /v1/responses client streams carry one cumulative snapshot in
	// response.completed (usage is null on in_progress frames); every field
	// merges by max, mirroring extractSSEUsage. input_tokens is inclusive of
	// the cached share (the convention the converters emit), so the details
	// share is subtracted before it lands in Input.
	var resp struct {
		Response *struct {
			Usage *struct {
				InputTokens     uint64 `json:"input_tokens"`
				OutputTokens    uint64 `json:"output_tokens"`
				CacheCreationIn uint64 `json:"cache_creation_input_tokens"`
				InputDetails    *struct {
					CachedTokens    uint64 `json:"cached_tokens"`
					CacheWriteToken uint64 `json:"cache_write_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &resp) == nil && resp.Response != nil && resp.Response.Usage != nil {
		s.sseUsage = true
		cached, written := uint64(0), uint64(0)
		if resp.Response.Usage.InputDetails != nil {
			cached = resp.Response.Usage.InputDetails.CachedTokens
			written = resp.Response.Usage.InputDetails.CacheWriteToken
		}
		written = max(written, resp.Response.Usage.CacheCreationIn)
		s.acc.Input = max(s.acc.Input, subClamped2(resp.Response.Usage.InputTokens, cached, written))
		s.acc.Output = max(s.acc.Output, resp.Response.Usage.OutputTokens)
		s.acc.CacheRead = max(s.acc.CacheRead, cached)
		s.acc.CacheCreation = max(s.acc.CacheCreation, written)
	}
}

// parseJSONUsage is the Commit-time fallback for NON-stream responses: the
// whole (capped) body is parsed once for a usage object. Ledger semantics
// mirror the SSE path exactly: the buckets are anthropic-normalized (input
// excludes cache), so the cached share riding inside inclusive counters
// (openai prompt_tokens, responses input_tokens) is subtracted, clamped at
// zero. Anything unparseable or usage-less is a silent no-op (error
// envelopes, truncated bodies, streams without usage events).
func (s *UsageScanner) parseJSONUsage() {
	if len(s.raw) == 0 {
		return
	}
	var doc struct {
		Usage *struct {
			InputTokens      uint64 `json:"input_tokens"`
			OutputTokens     uint64 `json:"output_tokens"`
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
			CacheCreation    uint64 `json:"cache_creation_input_tokens"`
			CacheRead        uint64 `json:"cache_read_input_tokens"`
			PromptCacheHit   uint64 `json:"prompt_cache_hit_tokens"`
			PromptDetails    *struct {
				CachedTokens    uint64 `json:"cached_tokens"`
				CacheWriteToken uint64 `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			InputDetails *struct {
				CachedTokens    uint64 `json:"cached_tokens"`
				CacheWriteToken uint64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(s.raw, &doc) != nil || doc.Usage == nil {
		return
	}
	u := doc.Usage
	if u.PromptTokens > 0 || u.CompletionTokens > 0 || u.PromptCacheHit > 0 {
		cached, written := uint64(0), uint64(0)
		if u.PromptDetails != nil {
			cached = u.PromptDetails.CachedTokens
			written = u.PromptDetails.CacheWriteToken
		}
		cached = max(cached, u.PromptCacheHit)
		written = max(written, u.CacheCreation)
		s.acc.Input += subClamped2(u.PromptTokens, cached, written)
		s.acc.Output += u.CompletionTokens
		s.acc.CacheRead = max(s.acc.CacheRead, cached)
		s.acc.CacheCreation = max(s.acc.CacheCreation, written)
		return
	}
	s.acc.Output += u.OutputTokens
	s.acc.CacheCreation = max(s.acc.CacheCreation, u.CacheCreation)
	s.acc.CacheRead = max(s.acc.CacheRead, u.CacheRead)
	// input_tokens is exclusive on anthropic bodies but inclusive of the
	// input_tokens_details share on responses bodies — only the details
	// share is subtracted, so both dialects keep one ledger.
	if u.InputDetails != nil {
		s.acc.Input += subClamped2(u.InputTokens, u.InputDetails.CachedTokens, u.InputDetails.CacheWriteToken)
		s.acc.CacheRead = max(s.acc.CacheRead, u.InputDetails.CachedTokens)
		s.acc.CacheCreation = max(s.acc.CacheCreation, u.InputDetails.CacheWriteToken)
	} else {
		s.acc.Input += u.InputTokens
	}
}

// subClamped returns a−b floored at zero: the inclusive wire counters (openai
// prompt_tokens, responses input_tokens) overlap the cached share, which the
// normalized buckets already carry in CacheRead.
// subClamped2 subtracts two shares (cache hit + cache write) from an
// inclusive counter, floored at zero — the hit share lands in CacheRead, the
// write share in CacheCreation.
func subClamped2(a, hit, write uint64) uint64 {
	return subClamped(subClamped(a, hit), write)
}

func subClamped(a, b uint64) uint64 {
	if b > a {
		return 0
	}
	return a - b
}
