package main

import (
	"bytes"
	"io"
	"sync"
	"testing"
)

func TestUsageScannerAnthropic(t *testing.T) {
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_creation_input_tokens\":50,\"cache_read_input_tokens\":10}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":200}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	key := tokenKey{Provider: "zhipu", Model: "glm-5"}
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), key, tc)
	io.Copy(io.Discard, sc)

	got := tc.snapshot()[key]
	if got.Input != 100 || got.CacheCreation != 50 || got.CacheRead != 10 || got.Output != 200 {
		t.Errorf("usage = %+v, want in=100 cc=50 cr=10 out=200", got)
	}
}

func TestUsageScannerOpenAI(t *testing.T) {
	stream := []byte("data: {\"id\":\"x\",\"choices\":[]}\n\ndata: {\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":13}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "deepseek", Model: "d"}, tc)
	io.Copy(io.Discard, sc)
	got := tc.snapshot()[tokenKey{Provider: "deepseek", Model: "d"}]
	if got.Input != 7 || got.Output != 13 {
		t.Errorf("usage = %+v, want in=7 out=13", got)
	}
}

// Split every usage payload byte-by-byte to prove the scanner reassembles across
// arbitrarily small reads.
func TestUsageScannerSplitBoundaries(t *testing.T) {
	payload := []byte("data: {\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":99}}\n\n")
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(&oneByteReader{b: payload}), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(io.Discard, sc)
	got := tc.snapshot()[tokenKey{Provider: "p", Model: "m"}]
	if got.Input != 42 || got.Output != 99 {
		t.Errorf("split-boundary usage = %+v, want in=42 out=99", got)
	}
}

// Pass-through must be byte-identical.
func TestUsageScannerPassthrough(t *testing.T) {
	stream := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\ndata: garbage\n\n")
	var sink bytes.Buffer
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(&sink, sc)
	if !bytes.Equal(sink.Bytes(), stream) {
		t.Errorf("passthrough not byte-identical:\nwant %q\ngot  %q", stream, sink.Bytes())
	}
}

// An oversized line is skipped for scanning but still passed through.
func TestUsageScannerOversizedLine(t *testing.T) {
	huge := bytes.Repeat([]byte("x"), 80_000)
	stream := append([]byte("data: "), huge...)
	stream = append(stream, []byte("\n\ndata: {\"usage\":{\"prompt_tokens\":3}}\n\n")...)
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	var sink bytes.Buffer
	sc := newUsageScanner(io.NopCloser(bytes.NewReader(stream)), tokenKey{Provider: "p", Model: "m"}, tc)
	io.Copy(&sink, sc)
	if !bytes.Equal(sink.Bytes(), stream) {
		t.Error("oversized passthrough mismatch")
	}
	if got := tc.snapshot()[tokenKey{Provider: "p", Model: "m"}].Input; got != 3 {
		t.Errorf("usage after oversized line = %d, want 3", got)
	}
}

func TestTokenCounterPersist(t *testing.T) {
	path := t.TempDir() + "/tokens.json"
	tc := newTokenCounter(path)
	tc.commit(tokenKey{Provider: "z", Model: "m"}, tokenUsage{Input: 10, Output: 20, Requests: 1})
	if err := tc.save(); err != nil {
		t.Fatal(err)
	}
	tc2 := newTokenCounter(path)
	if err := tc2.load(); err != nil {
		t.Fatal(err)
	}
	got := tc2.snapshot()[tokenKey{Provider: "z", Model: "m"}]
	if got.Input != 10 || got.Output != 20 || got.Requests != 1 {
		t.Errorf("persist round-trip failed: %+v", got)
	}
}

// oneByteReader yields one byte per Read to force split-boundary scanning.
type oneByteReader struct {
	b   []byte
	off int
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	p[0] = r.b[r.off]
	r.off++
	return 1, nil
}

// TestTokenCounterConcurrent would race under -race before the fix (the old
// commit mutated *tokenUsage fields after releasing tc.mu inside entry()). It
// spawns 50 concurrent committers to the SAME key plus a concurrent snapshot
// reader; after the fix every increment lands (no lost updates) and -race is
// clean. Final Input/Output must equal exactly the number of committers.
func TestTokenCounterConcurrent(t *testing.T) {
	tc := newTokenCounter(t.TempDir() + "/tokens.json")
	key := tokenKey{Provider: "p", Model: "m"}
	const committers = 50

	var snapDone sync.WaitGroup
	snapDone.Add(1)
	stopSnap := make(chan struct{})
	// Concurrent reader: hammers snapshot during commits. Before the fix this
	// read *tokenUsage fields while commit mutated them unlocked -> -race.
	go func() {
		defer snapDone.Done()
		for {
			select {
			case <-stopSnap:
				return
			default:
				_ = tc.snapshot()
			}
		}
	}()

	var wg sync.WaitGroup
	wg.Add(committers)
	start := make(chan struct{})
	for i := 0; i < committers; i++ {
		go func() {
			defer wg.Done()
			<-start
			tc.commit(key, tokenUsage{Input: 1, Output: 1})
		}()
	}
	close(start) // release all committers together to maximize contention
	wg.Wait()
	close(stopSnap)
	snapDone.Wait()

	got := tc.snapshot()[key]
	if got.Input != committers {
		t.Errorf("Input = %d, want %d (lost increments)", got.Input, committers)
	}
	if got.Output != committers {
		t.Errorf("Output = %d, want %d (lost increments)", got.Output, committers)
	}
	if got.Requests != committers {
		t.Errorf("Requests = %d, want %d", got.Requests, committers)
	}
}
