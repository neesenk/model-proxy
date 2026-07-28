package cache

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestReplayPreservesHeadersStatusBodyAndFlushes(t *testing.T) {
	store := testStore()
	body := bytes.Repeat([]byte("x"), 40*1024)
	store.Put("key", http.StatusCreated, http.Header{
		"Set-Cookie":    {"a=1", "b=2"},
		"Content-Type":  {"text/event-stream"},
		"Cache-Control": {"no-cache"},
	}, body, time.Unix(1000, 0))
	entry, ok := store.Lookup("key", time.Unix(1001, 0))
	if !ok {
		t.Fatal("stored entry missed")
	}
	writer := newRecordingWriter()
	if err := Replay(writer, entry); err != nil {
		t.Fatal(err)
	}
	if writer.status != http.StatusCreated || !bytes.Equal(writer.body.Bytes(), body) {
		t.Errorf("replay status/body = %d/%d bytes, want 201/%d", writer.status, writer.body.Len(), len(body))
	}
	if got := writer.Header().Values("Set-Cookie"); len(got) != 2 || got[0] != "a=1" || got[1] != "b=2" {
		t.Errorf("Set-Cookie values = %v, want [a=1 b=2]", got)
	}
	if writer.flushes != 2 {
		t.Errorf("flushes = %d, want 2 for 40 KiB body", writer.flushes)
	}
}

func TestReplayStopsOnWriteError(t *testing.T) {
	store := testStore()
	store.Put("key", 200, nil, []byte("body"), time.Now())
	entry, _ := store.Lookup("key", time.Now())
	want := errors.New("write failed")
	writer := &recordingWriter{header: make(http.Header), writeErr: want}
	if err := Replay(writer, entry); !errors.Is(err, want) {
		t.Errorf("Replay error = %v, want %v", err, want)
	}
	if err := Replay(writer, nil); err != nil {
		t.Errorf("nil Replay error = %v", err)
	}
}

type recordingWriter struct {
	header   http.Header
	body     bytes.Buffer
	status   int
	flushes  int
	writeErr error
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: make(http.Header)}
}

func (w *recordingWriter) Header() http.Header {
	return w.header
}

func (w *recordingWriter) WriteHeader(status int) {
	w.status = status
}

func (w *recordingWriter) Write(body []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.body.Write(body)
}

func (w *recordingWriter) Flush() {
	w.flushes++
}
