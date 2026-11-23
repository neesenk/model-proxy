package serve

import (
	"bytes"
	"log"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression (serve audit 2026-11-22): the 21:14 restart's 8s drain deadline
// force-closed 37 in-flight streams. The warn said only "forcing active
// connections closed" — the blast radius took a 1 GiB request-log scan to
// establish, and the two per-stream lines the executor did emit read as
// client disconnects. The drain warn must carry the in-flight handler count.

func captureShutdownLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	return &logs
}

func TestServeHTTPUntilShutdownWarnsForcedInflightCount(t *testing.T) {
	logs := captureShutdownLogs(t)
	const inflight = 3
	entered := make(chan struct{}, inflight)
	returned := make(chan struct{}, inflight)
	server, listener, _ := shutdownFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-r.Context().Done()
		returned <- struct{}{}
	}))

	shutdown := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ServeHTTPUntilShutdown(server, listener, shutdown, 50*time.Millisecond, nil, nil)
	}()

	var clients sync.WaitGroup
	clients.Add(inflight)
	for i := 0; i < inflight; i++ {
		go func() {
			defer clients.Done()
			resp, err := http.Get("http://" + listener.Addr().String() + "/")
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	for i := 0; i < inflight; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("in-flight handler was never admitted")
		}
	}

	close(shutdown)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeHTTPUntilShutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("force-close path did not complete")
	}
	for i := 0; i < inflight; i++ {
		select {
		case <-returned:
		case <-time.After(2 * time.Second):
			t.Fatal("in-flight handler was not cancelled by the force-close")
		}
	}
	waitClients := make(chan struct{})
	go func() { clients.Wait(); close(waitClients) }()
	select {
	case <-waitClients:
	case <-time.After(2 * time.Second):
		t.Fatal("client connections outlived the force-close")
	}

	logged := logs.String()
	want := "[shutdown] HTTP drain exceeded 50ms (context deadline exceeded); forcing active connections closed (in-flight handlers: 3)"
	if n := strings.Count(logged, want); n != 1 {
		t.Fatalf("drain warn lines %q = %d, want exactly 1:\n%s", want, n, logged)
	}
}
