// lifecycle.go owns admission-gated background work for the application
// runtime: tasks are admitted under a mutex so Add never races Wait.
package runtime

import "sync"

// Lifecycle is the single owner for Proxy-level background work. The
// mutex makes admission (WaitGroup.Add) atomic with shutdown, avoiding Add/Wait
// races while reload is scheduling a best-effort refresh.
type Lifecycle struct {
	mu       sync.Mutex
	stopping bool
	stop     chan struct{}
	wg       sync.WaitGroup
}

func NewLifecycle() *Lifecycle {
	return &Lifecycle{stop: make(chan struct{})}
}

func (l *Lifecycle) Run(task func(stop <-chan struct{})) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return false
	}
	l.wg.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.wg.Done()
		task(l.stop)
	}()
	return true
}

// StopChannel returns the channel closed by BeginStop (read-only lifecycle
// probe; production tasks receive it through their task function).
func (l *Lifecycle) StopChannel() <-chan struct{} {
	if l == nil {
		return nil
	}
	return l.stop
}

func (l *Lifecycle) BeginStop() {
	if l == nil {
		return
	}
	l.mu.Lock()
	if !l.stopping {
		l.stopping = true
		close(l.stop)
	}
	l.mu.Unlock()
}

func (l *Lifecycle) Wait() {
	if l != nil {
		l.wg.Wait()
	}
}
