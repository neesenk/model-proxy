package app

import (
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
)

func TestProxyCloseWaitsForOwnedTasksAndRejectsNewWork(t *testing.T) {
	p := newTestProxy(t, &Config{})
	started := make(chan struct{})
	release := make(chan struct{})
	if !p.lifecycle.Run(func(<-chan struct{}) {
		close(started)
		<-release
	}) {
		t.Fatal("initial lifecycle task was rejected")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned before the owned task finished")
	case <-time.After(20 * time.Millisecond):
	}
	if p.lifecycle.Run(func(<-chan struct{}) {}) {
		t.Fatal("lifecycle admitted new work after shutdown began")
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the owned task exited")
	}

	// Idempotent: a second close must return immediately.
	p.Close()
}

func TestProxyCloseDrainsOwnedRequestLogger(t *testing.T) {
	p := newTestProxy(t, &Config{})
	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
		p.reqLog.Run()
	})
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}
	p.reqLog.Enqueue(p.reqLog.BuildRecord(requestlog.Input{
		Timestamp: time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
		RequestID: "before-close", ResponseBody: []byte("durable"),
	}))

	p.Close()
	records, err := requestlog.QueryRecords(logDir, requestlog.Filter{RequestID: "before-close"})
	if err != nil {
		t.Fatalf("query drained request log: %v", err)
	}
	if len(records) != 1 || records[0].ResponseBody != "durable" {
		t.Fatalf("Close returned without durably draining accepted record: %+v", records)
	}
}

func TestProxyCloseWaitsForShadowBeforeDrainingRequestLogger(t *testing.T) {
	p := newTestProxy(t, &Config{})
	logDir := t.TempDir()
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: logDir, MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) {
		p.reqLog.Run()
	})
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	started := make(chan struct{})
	release := make(chan struct{})
	// The task deliberately ignores stop: the LIFECYCLE contract is that
	// WaitBeforeLogDrain waits for admitted tasks unconditionally; bounding a
	// hung task is runShadow's responsibility (grace + cancel), covered by
	// TestCloseCancelsInFlightShadowRequest.
	if !p.lifecycle.RunBeforeLogDrain(func(<-chan struct{}) {
		close(started)
		<-release
		p.reqLog.Enqueue(p.reqLog.BuildRecord(requestlog.Input{
			Timestamp: time.Date(2026, 7, 28, 1, 0, 0, 0, time.UTC),
			RequestID: "shadow-r1", ResponseBody: []byte("shadow-durable"),
		}))
	}) {
		t.Fatal("shadow task was rejected before shutdown")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-p.lifecycle.StopChannel():
	case <-time.After(time.Second):
		close(release)
		t.Fatal("Close did not begin lifecycle shutdown")
	}
	select {
	case <-closed:
		close(release)
		t.Fatal("Close returned before the shadow task finished")
	default:
	}
	if p.lifecycle.RunBeforeLogDrain(func(<-chan struct{}) {}) {
		close(release)
		t.Fatal("lifecycle admitted shadow work after shutdown began")
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after the shadow task exited")
	}
	records, err := requestlog.QueryRecords(logDir, requestlog.Filter{RequestID: "shadow-r1"})
	if err != nil {
		t.Fatalf("query shadow record after Close: %v", err)
	}
	if len(records) != 1 || !records[0].Shadow || records[0].ResponseBody != "shadow-durable" {
		t.Fatalf("request logger drained before the shadow record became durable: %+v", records)
	}
}
