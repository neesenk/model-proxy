package runtime

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestLifecycleRunAndStop(t *testing.T) {
	l := NewLifecycle()
	var ran atomic.Int32
	for i := 0; i < 5; i++ {
		if !l.Run(func(<-chan struct{}) { ran.Add(1) }) {
			t.Fatal("admission rejected before stop")
		}
	}
	l.BeginStop()
	l.Wait()
	if ran.Load() != 5 {
		t.Errorf("ran = %d, want 5", ran.Load())
	}
	if l.Run(func(<-chan struct{}) {}) {
		t.Error("admission accepted after stop")
	}
	select {
	case <-l.StopChannel():
	default:
		t.Error("StopChannel open after BeginStop")
	}
}

func TestLifecycleRunBeforeLogDrain(t *testing.T) {
	l := NewLifecycle()
	release := make(chan struct{})
	started := make(chan struct{})
	if !l.RunBeforeLogDrain(func() {
		close(started)
		<-release
	}) {
		t.Fatal("admission rejected")
	}
	<-started
	done := make(chan struct{})
	go func() { l.BeginStop(); l.WaitBeforeLogDrain(); close(done) }()
	select {
	case <-done:
		t.Fatal("WaitBeforeLogDrain returned while task blocked")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitBeforeLogDrain did not return after task finished")
	}
}

func TestLifecycleNilSafe(t *testing.T) {
	var l *Lifecycle
	if l.Run(func(<-chan struct{}) {}) || l.RunBeforeLogDrain(func() {}) {
		t.Error("nil lifecycle must reject admission")
	}
	l.BeginStop()
	l.Wait()
	l.WaitBeforeLogDrain()
	if l.StopChannel() != nil {
		t.Error("nil lifecycle StopChannel must be nil")
	}
}
