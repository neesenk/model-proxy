package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sync"
)

type tokenKey struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type tokenUsage struct {
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	Requests      uint64 `json:"requests"`
}

type tokenCounter struct {
	path string
	mu   sync.Mutex
	m    map[tokenKey]*tokenUsage
}

func newTokenCounter(path string) *tokenCounter {
	return &tokenCounter{path: path, m: map[tokenKey]*tokenUsage{}}
}

func (tc *tokenCounter) entry(k tokenKey) *tokenUsage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	u := tc.m[k]
	if u == nil {
		u = &tokenUsage{}
		tc.m[k] = u
	}
	return u
}

func (tc *tokenCounter) commit(k tokenKey, add tokenUsage) {
	u := tc.entry(k)
	u.Input += add.Input
	u.Output += add.Output
	u.CacheCreation += add.CacheCreation
	u.CacheRead += add.CacheRead
	if add.Input > 0 || add.Output > 0 {
		u.Requests++
	}
}

func (tc *tokenCounter) snapshot() map[tokenKey]tokenUsage {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	out := make(map[tokenKey]tokenUsage, len(tc.m))
	for k, v := range tc.m {
		out[k] = *v
	}
	return out
}

func (tc *tokenCounter) save() error {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	flat := map[string]tokenUsage{}
	for k, v := range tc.m {
		flat[k.Provider+"\x00"+k.Model] = *v
	}
	data, err := json.MarshalIndent(flat, "", "  ")
	if err != nil {
		return err
	}
	tmp := tc.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, tc.path)
}

func (tc *tokenCounter) load() error {
	data, err := os.ReadFile(tc.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var flat map[string]tokenUsage
	if err := json.Unmarshal(data, &flat); err != nil {
		return err
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for k, v := range flat {
		prov, mod := splitKey(k)
		u := v
		tc.m[tokenKey{prov, mod}] = &u
	}
	return nil
}

func splitKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
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
		// else: drop the byte (oversized line) — still passed through via p.
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
