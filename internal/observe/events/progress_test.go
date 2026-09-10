package events

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// slowReader drips bytes one at a time so Read calls are deterministic and
// independent of the source slice size.
type slowReader struct {
	data []byte
	pos  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n := 1
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

func (r *slowReader) Close() error { return nil }

func TestProgressReaderPublishesOnThrottleBytes(t *testing.T) {
	var calls []struct {
		text string
		size int64
	}
	publish := func(text string, receivedBytes int64) {
		calls = append(calls, struct {
			text string
			size int64
		}{text: text, size: receivedBytes})
	}
	src := &slowReader{data: []byte(strings.Repeat("x", 50))}
	r := NewProgressReaderWithLimits(src, ProgressMeta{RequestID: "r1"}, publish, 1024, 16, 0)

	buf := make([]byte, 1)
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(calls) == 0 {
		t.Fatal("expected at least one progress publish")
	}
	// First publish should fire on first byte (lastPublishedAt.IsZero()).
	if calls[0].size != 1 || calls[0].text != "x" {
		t.Fatalf("first publish = (%q, %d), want (\"x\", 1)", calls[0].text, calls[0].size)
	}
	// Final publish must report all 50 bytes.
	last := calls[len(calls)-1]
	if last.size != 50 || last.text != strings.Repeat("x", 50) {
		t.Fatalf("last publish = (%q, %d), want 50 xs", last.text, last.size)
	}
	// With 16-byte throttle and 50-byte source, plus first-byte immediate and
	// final flush, expect: 1, 17, 33, 49, 50.
	wantSizes := []int64{1, 17, 33, 49, 50}
	if len(calls) != len(wantSizes) {
		t.Fatalf("got %d calls, want %d; sizes=%v", len(calls), len(wantSizes), sizes(calls))
	}
	for i, want := range wantSizes {
		if calls[i].size != want {
			t.Fatalf("call[%d].size = %d, want %d", i, calls[i].size, want)
		}
	}
}

func TestProgressReaderPublishesOnThrottleTime(t *testing.T) {
	var calls int
	publish := func(string, int64) { calls++ }
	// 1 KiB byte threshold so the time threshold is the limiting factor.
	src := &slowReader{data: []byte(strings.Repeat("a", 100))}
	r := NewProgressReaderWithLimits(src, ProgressMeta{RequestID: "r2"}, publish, 1024, 1024, 5*time.Millisecond)

	buf := make([]byte, 1)
	for i := 0; i < 10; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if calls != 1 {
		t.Fatalf("after first 10 bytes, calls = %d, want 1 (first-byte immediate)", calls)
	}
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if _, err := r.Read(buf); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("after time threshold, calls = %d, want 2", calls)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestProgressReaderCapsHeadAndReportsTruncation(t *testing.T) {
	var calls []struct {
		text string
		size int64
	}
	publish := func(text string, receivedBytes int64) {
		calls = append(calls, struct {
			text string
			size int64
		}{text: text, size: receivedBytes})
	}
	// Source is larger than the 32-byte cap; final text must be the head only.
	src := &slowReader{data: []byte(strings.Repeat("b", 100))}
	r := NewProgressReaderWithLimits(src, ProgressMeta{RequestID: "r3"}, publish, 32, 1024, 0)

	buf := make([]byte, 1)
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	last := calls[len(calls)-1]
	if last.size != 100 {
		t.Fatalf("received_bytes = %d, want 100", last.size)
	}
	if len(last.text) != 32 {
		t.Fatalf("text length = %d, want 32 (head cap)", len(last.text))
	}
	wantHead := strings.Repeat("b", 32)
	if last.text != wantHead {
		t.Fatalf("text = %q, want head 32 bs", last.text)
	}
}

func TestProgressReaderNoPublishWithoutReads(t *testing.T) {
	called := false
	publish := func(string, int64) { called = true }
	src := io.NopCloser(bytes.NewReader([]byte("hello")))
	r := NewProgressReader(src, ProgressMeta{RequestID: "r4"}, publish)
	if called {
		t.Fatal("publish should not fire before any Read")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// No bytes were observed, so there is nothing to flush-publish.
	if called {
		t.Fatal("publish should not fire when no bytes were read")
	}
}

func TestProgressReaderCloseIdempotent(t *testing.T) {
	closes := 0
	src := &closeCounter{closes: &closes}
	r := NewProgressReader(src, ProgressMeta{RequestID: "r5"}, func(string, int64) {})
	_ = r.Close()
	_ = r.Close()
	if closes != 1 {
		t.Fatalf("source closed %d times, want 1", closes)
	}
}

type closeCounter struct {
	closes *int
}

func (c *closeCounter) Read(p []byte) (int, error) { return 0, io.EOF }
func (c *closeCounter) Close() error {
	*c.closes++
	return nil
}

func sizes(calls []struct {
	text string
	size int64
}) []int64 {
	out := make([]int64, len(calls))
	for i, c := range calls {
		out[i] = c.size
	}
	return out
}
