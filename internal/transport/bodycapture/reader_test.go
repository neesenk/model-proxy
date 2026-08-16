package bodycapture

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
)

func TestReaderCapturesExactBytesAndPassesThrough(t *testing.T) {
	want := []byte("hello request log world\nline two\n")
	var captured []byte
	var total int64
	var truncated bool
	reader := New(io.NopCloser(bytes.NewReader(want)), 1024, func(got []byte, gotTotal int64, gotTruncated bool) {
		captured = append([]byte(nil), got...)
		total = gotTotal
		truncated = gotTruncated
	})

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pass-through bytes = %q, want %q", got, want)
	}
	if !bytes.Equal(captured, want) {
		t.Errorf("captured bytes = %q, want %q", captured, want)
	}
	if total != int64(len(want)) || truncated {
		t.Errorf("capture metadata = total:%d truncated:%v, want %d/false", total, truncated, len(want))
	}
}

func TestReaderTruncatesCaptureButPassesThroughFullBody(t *testing.T) {
	want := bytes.Repeat([]byte("ABCDEFGH"), 1000)
	const max = 4096
	var captured []byte
	var total int64
	var truncated bool
	reader := New(io.NopCloser(bytes.NewReader(want)), max, func(got []byte, gotTotal int64, gotTruncated bool) {
		captured = append([]byte(nil), got...)
		total = gotTotal
		truncated = gotTruncated
	})

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("pass-through bytes = %d, want %d", len(got), len(want))
	}
	if !truncated || len(captured) != max || total != int64(len(want)) {
		t.Errorf("capture metadata = captured:%d total:%d truncated:%v, want %d/%d/true", len(captured), total, truncated, max, len(want))
	}
}

func TestReaderMarksTruncatedWhenLaterReadCrossesExactBoundary(t *testing.T) {
	source := &chunkedReadCloser{chunks: [][]byte{[]byte("1234"), []byte("5")}}
	var captured []byte
	var total int64
	var truncated bool
	reader := New(source, 4, func(got []byte, gotTotal int64, gotTruncated bool) {
		captured = append([]byte(nil), got...)
		total = gotTotal
		truncated = gotTruncated
	})

	got, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if string(got) != "12345" {
		t.Errorf("pass-through = %q, want %q", got, "12345")
	}
	if string(captured) != "1234" || total != 5 || !truncated {
		t.Errorf("capture = %q total:%d truncated:%v, want %q/5/true", captured, total, truncated, "1234")
	}
}

func TestReaderZeroLimitMarksAnyBodyTruncated(t *testing.T) {
	var captured []byte
	var total int64
	var truncated bool
	reader := New(io.NopCloser(bytes.NewReader([]byte("x"))), 0, func(got []byte, gotTotal int64, gotTruncated bool) {
		captured = append([]byte(nil), got...)
		total = gotTotal
		truncated = gotTruncated
	})
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if len(captured) != 0 || total != 1 || !truncated {
		t.Errorf("capture = %q total:%d truncated:%v, want empty/1/true", captured, total, truncated)
	}
}

func TestReaderCloseCallsCallbackAndSourceCloseOnce(t *testing.T) {
	closeErr := errors.New("source close failed")
	source := &closeTrackingReader{Reader: bytes.NewReader([]byte("x")), closeErr: closeErr}
	var callbacks int
	reader := New(source, 64, func([]byte, int64, bool) { callbacks++ })

	if err := reader.Close(); !errors.Is(err, closeErr) {
		t.Fatalf("first Close error = %v, want %v", err, closeErr)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("second Close error = %v, want nil", err)
	}
	if callbacks != 1 || source.closes != 1 {
		t.Errorf("callbacks/source closes = %d/%d, want 1/1", callbacks, source.closes)
	}
}

func TestReaderDistinctInstancesAreRaceClean(t *testing.T) {
	var group sync.WaitGroup
	var callbacks atomic.Int64
	for range 50 {
		group.Add(1)
		go func() {
			defer group.Done()
			reader := New(io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("z"), 1024))), 2048, func([]byte, int64, bool) {
				callbacks.Add(1)
			})
			if _, err := io.Copy(io.Discard, reader); err != nil {
				t.Errorf("Copy: %v", err)
			}
			if err := reader.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	group.Wait()
	if got := callbacks.Load(); got != 50 {
		t.Errorf("callbacks = %d, want 50", got)
	}
}

func TestReaderReusedPoolBufferStartsEmpty(t *testing.T) {
	// First reader fills and returns a buffer to the pool.
	first := New(io.NopCloser(bytes.NewReader(bytes.Repeat([]byte("A"), 4096))), 8192, func([]byte, int64, bool) {})
	if _, err := io.Copy(io.Discard, first); err != nil {
		t.Fatalf("first Copy: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// A second reader may receive the same buffer; its capture must contain
	// exactly its own bytes, not residue from the first reader.
	want := []byte("short body")
	var captured []byte
	second := New(io.NopCloser(bytes.NewReader(want)), 8192, func(got []byte, _ int64, truncated bool) {
		captured = append([]byte(nil), got...)
		if truncated {
			t.Error("unexpected truncation")
		}
	})
	if _, err := io.Copy(io.Discard, second); err != nil {
		t.Fatalf("second Copy: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if !bytes.Equal(captured, want) {
		t.Errorf("captured = %q, want %q", captured, want)
	}
}

func BenchmarkReaderTee(b *testing.B) {
	for _, size := range []struct {
		name string
		n    int
	}{
		{"1KB", 1024},
		{"64KB", 64 * 1024},
		{"1MB", 1024 * 1024},
	} {
		b.Run(size.name, func(b *testing.B) {
			body := bytes.Repeat([]byte("z"), size.n)
			b.ReportAllocs()
			b.SetBytes(int64(size.n))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				reader := New(io.NopCloser(bytes.NewReader(body)), 1<<20, func([]byte, int64, bool) {})
				if _, err := io.Copy(io.Discard, reader); err != nil {
					b.Fatal(err)
				}
				if err := reader.Close(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type closeTrackingReader struct {
	*bytes.Reader
	closeErr error
	closes   int
}

type chunkedReadCloser struct {
	chunks [][]byte
}

func (r *chunkedReadCloser) Read(buffer []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(buffer, chunk), nil
}

func (*chunkedReadCloser) Close() error { return nil }

func (r *closeTrackingReader) Close() error {
	r.closes++
	return r.closeErr
}

// TestPutCaptureBufferDropsOversized: buffers that grew past
// maxPooledCaptureBytes are NOT returned to the free-list — the pool must not
// pin megabyte capacities (8 × 2MB responses state captures would otherwise
// stay resident forever, and every later small request would reuse them).
func TestPutCaptureBufferDropsOversized(t *testing.T) {
	captureBuffers.mu.Lock()
	captureBuffers.buffers = nil
	captureBuffers.mu.Unlock()

	big := new(bytes.Buffer)
	big.Write(make([]byte, maxPooledCaptureBytes+1))
	putCaptureBuffer(big)
	captureBuffers.mu.Lock()
	pooled := len(captureBuffers.buffers)
	captureBuffers.buffers = nil
	captureBuffers.mu.Unlock()
	if pooled != 0 {
		t.Fatalf("oversized buffer pooled (len=%d), want dropped", pooled)
	}

	small := new(bytes.Buffer)
	small.Write(make([]byte, 64))
	putCaptureBuffer(small)
	captureBuffers.mu.Lock()
	pooled = len(captureBuffers.buffers)
	captureBuffers.buffers = nil
	captureBuffers.mu.Unlock()
	if pooled != 1 {
		t.Fatalf("small buffer not pooled (len=%d), want 1", pooled)
	}
}
