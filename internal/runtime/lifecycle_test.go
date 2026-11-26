package runtime

import (
	"sync/atomic"
	"testing"

	"model-proxy/internal/testsyn"
)

func TestLifecycleRunAndStop(t *testing.T) {
	// Leak accounting pilot (testing.md §并发测试模式目录): every admitted
	// task goroutine must have exited once BeginStop/Wait return.
	baseline := testsyn.Goroutines()
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
	testsyn.LeakCheck(t, baseline, "lifecycle tasks after BeginStop/Wait")
}

func TestLifecycleNilSafe(t *testing.T) {
	var l *Lifecycle
	if l.Run(func(<-chan struct{}) {}) {
		t.Error("nil lifecycle must reject admission")
	}
	l.BeginStop()
	l.Wait()
	if l.StopChannel() != nil {
		t.Error("nil lifecycle StopChannel must be nil")
	}
}
