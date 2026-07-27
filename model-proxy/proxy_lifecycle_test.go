package main

import (
	"testing"
	"time"
)

func TestProxyCloseWaitsForOwnedTasksAndRejectsNewWork(t *testing.T) {
	p := newTestProxy(t, &Config{})
	started := make(chan struct{})
	release := make(chan struct{})
	if !p.lifecycle.run(func(<-chan struct{}) {
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
	if p.lifecycle.run(func(<-chan struct{}) {}) {
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
	p.reqLog = newRequestLogger(t.TempDir(), 1<<20, 1<<10, 0)
	p.reqLogStarted = p.lifecycle.run(func(<-chan struct{}) {
		p.reqLog.loop()
	})
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	p.Close()
	select {
	case <-p.reqLog.closed:
	default:
		t.Fatal("Close returned before request logger drained")
	}
}
