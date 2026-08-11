// lifecycle.go owns admission-gated background work for the application
// runtime: tasks are admitted under a mutex so Add never races Wait, and
// log-producing finite tasks drain before the request logger shuts down.
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
	// beforeLogDrain tracks finite tasks whose final output is written through
	// requestlog.Logger (currently Shadow evaluations). Close rejects new
	// admissions, waits for this group, and only then drains the logger.
	beforeLogDrain sync.WaitGroup
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

// runBeforeLogDrain admits finite background work that may still enqueue a
// request-log record. Admission shares the lifecycle mutex with beginStop, so
// Add cannot race Wait and no task can start after shutdown begins.
func (l *Lifecycle) RunBeforeLogDrain(task func()) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	if l.stopping {
		l.mu.Unlock()
		return false
	}
	l.beforeLogDrain.Add(1)
	l.mu.Unlock()
	go func() {
		defer l.beforeLogDrain.Done()
		task()
	}()
	return true
}

// StopChannel returns the channel closed by BeginStop (read-only lifecycle
// probe for tests; production never selects on it directly).
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

func (l *Lifecycle) WaitBeforeLogDrain() {
	if l != nil {
		l.beforeLogDrain.Wait()
	}
}
