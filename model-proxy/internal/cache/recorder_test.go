package cache

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRecorderCompleteness(t *testing.T) {
	recorder := NewRecorder(io.NopCloser(strings.NewReader("hello")), 100)
	body, err := io.ReadAll(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if !recorder.Complete() || string(body) != "hello" || string(recorder.Body()) != "hello" {
		t.Errorf("clean recorder = complete:%v read:%q captured:%q",
			recorder.Complete(), body, recorder.Body())
	}

	partial := NewRecorder(&fixedErrorReader{err: io.ErrUnexpectedEOF}, 100)
	if _, err := io.ReadAll(partial); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial read error = %v, want unexpected EOF", err)
	}
	if partial.Complete() {
		t.Error("partial recorder reported complete")
	}

	eofWithData := NewRecorder(&dataEOFReader{data: []byte("final")}, 100)
	if body, err := io.ReadAll(eofWithData); err != nil || string(body) != "final" || !eofWithData.Complete() {
		t.Errorf("data+EOF read = %q, %v complete:%v", body, err, eofWithData.Complete())
	}
}

func TestRecorderTruncatesCaptureButPassesThrough(t *testing.T) {
	source := bytes.Repeat([]byte("x"), 100)
	recorder := NewRecorder(io.NopCloser(bytes.NewReader(source)), 40)
	body, err := io.ReadAll(recorder)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, source) {
		t.Errorf("pass-through body length = %d, want %d", len(body), len(source))
	}
	if recorder.Complete() || len(recorder.Body()) != 40 {
		t.Errorf("truncated recorder = complete:%v captured:%d, want false/40",
			recorder.Complete(), len(recorder.Body()))
	}
}

func TestRecorderCloseDelegates(t *testing.T) {
	want := errors.New("close failed")
	source := &closeTrackingReader{closeErr: want}
	recorder := NewRecorder(source, 1)
	if err := recorder.Close(); !errors.Is(err, want) {
		t.Errorf("Close error = %v, want %v", err, want)
	}
	if !source.closed {
		t.Fatal("Close did not reach source")
	}
}

func TestHeaderForCapturedBody(t *testing.T) {
	upstream := http.Header{
		"Content-Type":      {"application/json"},
		"Content-Length":    {"123"},
		"Transfer-Encoding": {"chunked"},
		"X-Other":           {"keep"},
	}
	passThrough := HeaderForCapturedBody(upstream, false, false, false)
	if passThrough.Get("Content-Length") != "123" || passThrough.Get("Content-Type") != "application/json" {
		t.Errorf("pass-through headers = %v", passThrough)
	}
	passThrough.Set("X-Other", "mutated")
	if upstream.Get("X-Other") != "keep" {
		t.Fatal("HeaderForCapturedBody returned upstream header storage")
	}

	converted := HeaderForCapturedBody(upstream, true, false, false)
	if converted.Get("Content-Length") != "" || converted.Get("Transfer-Encoding") != "" ||
		converted.Get("Content-Type") != "application/json" || converted.Get("X-Other") != "keep" {
		t.Errorf("converted headers = %v", converted)
	}
	stream := HeaderForCapturedBody(upstream, true, true, true)
	if got := stream.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("stream mode mismatch Content-Type = %q", got)
	}
	json := HeaderForCapturedBody(upstream, true, true, false)
	if got := json.Get("Content-Type"); got != "application/json" {
		t.Errorf("JSON mode mismatch Content-Type = %q", got)
	}
}

type fixedErrorReader struct {
	err error
}

func (r *fixedErrorReader) Read([]byte) (int, error) { return 0, r.err }
func (r *fixedErrorReader) Close() error             { return nil }

type dataEOFReader struct {
	data []byte
	done bool
}

func (r *dataEOFReader) Read(buffer []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	return copy(buffer, r.data), io.EOF
}

func (r *dataEOFReader) Close() error { return nil }

type closeTrackingReader struct {
	closed   bool
	closeErr error
}

func (r *closeTrackingReader) Read([]byte) (int, error) { return 0, io.EOF }
func (r *closeTrackingReader) Close() error {
	r.closed = true
	return r.closeErr
}
