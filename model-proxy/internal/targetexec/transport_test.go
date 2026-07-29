package targetexec

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTimingResponseWriter(t *testing.T) {
	recorder := httptest.NewRecorder()
	writer := newTimingResponseWriter(recorder)
	if writer.hasFirstByte {
		t.Fatal("first byte stamped before a write")
	}
	writer.Write([]byte("hello"))
	if !writer.hasFirstByte || writer.firstByte.IsZero() {
		t.Fatal("first byte was not stamped")
	}
	first := writer.firstByte
	writer.Write([]byte("world"))
	writer.Flush()
	if writer.firstByte != first {
		t.Fatal("later write moved the first-byte timestamp")
	}
	if got := recorder.Body.String(); got != "helloworld" {
		t.Fatalf("body = %q, want helloworld", got)
	}
}

func TestSniffSSEFraming(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "event", body: "event: message_start\ndata: {}\n\n", want: true},
		{name: "data", body: "data: {}\n\n", want: true},
		{name: "leading whitespace", body: "\n  event: x\n", want: true},
		{name: "id", body: "id: 7\n", want: true},
		{name: "retry", body: "retry: 1000\n", want: true},
		{name: "heartbeat", body: ": ping\n\n", want: true},
		{name: "json", body: `{"id":"c1","choices":[]}`, want: false},
		{name: "empty", body: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{Body: io.NopCloser(strings.NewReader(test.body))}
			if got := sniffSSEFraming(response); got != test.want {
				t.Fatalf("sniff = %v, want %v", got, test.want)
			}
			restored, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(restored); got != test.body {
				t.Fatalf("restored body = %q, want %q", got, test.body)
			}
		})
	}
}

func TestSniffSSEFraming_SplitMarker(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		full   string
	}{
		{
			name:   "event",
			chunks: []string{"ev", "ent: message_start\ndata: {}\n\n"},
			full:   "event: message_start\ndata: {}\n\n",
		},
		{
			name:   "short id",
			chunks: []string{"i", "d: 7\n\n"},
			full:   "id: 7\n\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &http.Response{Body: &chunkedReadCloser{chunks: test.chunks}}
			if !sniffSSEFraming(response) {
				t.Fatalf("split %s marker was not recognized", test.name)
			}
			restored, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(restored); got != test.full {
				t.Fatalf("restored body = %q, want %q", got, test.full)
			}
		})
	}
}

func TestSniffSSEFraming_SplitShortFieldNoBlock(t *testing.T) {
	assertSniffReturnsOnShortField(t, []string{"i", "d: 7\n\n"}, "id: 7\n\n")
}

func TestSniffSSEFraming_HeartbeatNoBlock(t *testing.T) {
	assertSniffReturnsOnShortField(t, []string{": ping\n\n"}, ": ping\n\n")
}

func assertSniffReturnsOnShortField(t *testing.T, chunks []string, prefix string) {
	t.Helper()
	reader, writer := io.Pipe()
	response := &http.Response{Body: reader}
	done := make(chan bool, 1)
	go func() { done <- sniffSSEFraming(response) }()
	for _, chunk := range chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case got := <-done:
		if !got {
			t.Fatal("decisive short field was not recognized")
		}
	case <-time.After(time.Second):
		t.Fatal("sniff waited to fill its window after a decisive field")
	}
	go func() {
		writer.Write([]byte("event: x\n\n"))
		writer.Close()
	}()
	restored, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(restored); got != prefix+"event: x\n\n" {
		t.Fatalf("restored body = %q", got)
	}
}

func TestFlushCopyClassifiesTermination(t *testing.T) {
	t.Run("clean eof", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		body := io.NopCloser(strings.NewReader("ok"))
		if got := flushCopy(recorder, body); got != streamEOF {
			t.Fatalf("end = %v, want clean EOF", got)
		}
		if got := recorder.Body.String(); got != "ok" {
			t.Fatalf("body = %q", got)
		}
	})
	t.Run("client gone", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader("ok"))
		if got := flushCopy(failingResponseWriter{}, body); got != streamClientGone {
			t.Fatalf("end = %v, want client gone", got)
		}
	})
	t.Run("upstream error", func(t *testing.T) {
		body := &errorReadCloser{err: errors.New("upstream read")}
		if got := flushCopy(httptest.NewRecorder(), body); got != streamUpstreamErr {
			t.Fatalf("end = %v, want upstream error", got)
		}
	})
}

type chunkedReadCloser struct {
	chunks []string
}

func (reader *chunkedReadCloser) Read(buffer []byte) (int, error) {
	if len(reader.chunks) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer, reader.chunks[0])
	reader.chunks[0] = reader.chunks[0][count:]
	if reader.chunks[0] == "" {
		reader.chunks = reader.chunks[1:]
	}
	return count, nil
}

func (*chunkedReadCloser) Close() error { return nil }

type failingResponseWriter struct{}

func (failingResponseWriter) Header() http.Header       { return make(http.Header) }
func (failingResponseWriter) WriteHeader(int)           {}
func (failingResponseWriter) Write([]byte) (int, error) { return 0, errors.New("client gone") }

type errorReadCloser struct {
	err error
}

func (reader *errorReadCloser) Read([]byte) (int, error) { return 0, reader.err }
func (*errorReadCloser) Close() error                    { return nil }
