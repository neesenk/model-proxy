package main

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
)

// tokenKey aliases the shared (provider, model) key so existing call sites
// (scanner, tests) read naturally. Persistence is handled by statsStore
// (SQLite); the JSON file format is gone.
type tokenKey = pmKey

type tokenUsage struct {
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

type tokenCounter struct {
	mu sync.Mutex
	m  map[tokenKey]*tokenUsage
}

func newTokenCounter() *tokenCounter {
	return &tokenCounter{m: map[tokenKey]*tokenUsage{}}
}

// commit records one observed usage payload under the (provider, model) key.
// All reads and writes of a *tokenUsage's fields happen under tc.mu: the
// get-or-create and the read-modify-write are a single critical section so two
// concurrent scanners committing to the same key cannot lose increments.
func (tc *tokenCounter) commit(k tokenKey, add tokenUsage) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	u := tc.m[k]
	if u == nil {
		u = &tokenUsage{}
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
func (tc *tokenCounter) snapshot() map[tokenKey]tokenUsage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make(map[tokenKey]tokenUsage, len(tc.m))
	for k, v := range tc.m {
		out[k] = *v
	}
	return out
}

// seed sets a (provider, model) entry to a baseline (boot restore from SQLite).
func (tc *tokenCounter) seed(k tokenKey, u tokenUsage) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	v := u
	tc.m[k] = &v
}

func (tc *tokenCounter) reset() {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.m = map[tokenKey]*tokenUsage{}
}

const scanLineCap = 64 * 1024

// usageScanner is a pass-through io.ReadCloser: bytes read from src are returned
// verbatim, and observed incrementally to extract SSE usage events. It never
// modifies, buffers the stream, or blocks the client. Failures are silent (no
// usage recorded). commit happens once on EOF.
type usageScanner struct {
	src  io.ReadCloser
	key  tokenKey
	tc   *tokenCounter
	line []byte // current incomplete line (bounded by scanLineCap)
	acc  tokenUsage
	done bool
}

func newUsageScanner(src io.ReadCloser, key tokenKey, tc *tokenCounter) *usageScanner {
	return &usageScanner{src: src, key: key, tc: tc}
}

func (s *usageScanner) Read(p []byte) (int, error) {
	n, err := s.src.Read(p)
	if n > 0 {
		s.observe(p[:n])
	}
	if err != nil && !s.done {
		s.done = true
		s.tc.commit(s.key, s.acc)
	}
	return n, err
}

func (s *usageScanner) Close() error {
	if !s.done {
		s.done = true
		s.tc.commit(s.key, s.acc)
	}
	return s.src.Close()
}

// observe scans a chunk for complete lines, extracting usage. Partial line bytes
// are held in s.line (capped); an over-long line is flushed (skipped) to bound memory.
func (s *usageScanner) observe(chunk []byte) {
	for _, b := range chunk {
		if b == '\n' {
			s.parseLine(s.line)
			s.line = s.line[:0]
			continue
		}
		if len(s.line) < scanLineCap {
			s.line = append(s.line, b)
		}
		// else: drop the byte (oversized line) - still passed through via p.
	}
}

// parseLine inspects one SSE data line for a usage payload.
func (s *usageScanner) parseLine(line []byte) {
	trimmed := bytes.TrimLeft(line, " \t")
	if !bytes.HasPrefix(trimmed, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	// Try anthropic shapes first, then openai.
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
			OutputTokens uint64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &anth) == nil {
		if anth.Type == "message_start" {
			s.acc.Input += anth.Message.Usage.InputTokens
			s.acc.CacheCreation += anth.Message.Usage.CacheCreationTokens
			s.acc.CacheRead += anth.Message.Usage.CacheReadTokens
		}
		if anth.Type == "message_delta" {
			s.acc.Output += anth.Usage.OutputTokens
		}
	}
	var oai struct {
		Usage *struct {
			PromptTokens     uint64 `json:"prompt_tokens"`
			CompletionTokens uint64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &oai) == nil && oai.Usage != nil {
		s.acc.Input += oai.Usage.PromptTokens
		s.acc.Output += oai.Usage.CompletionTokens
	}
}
