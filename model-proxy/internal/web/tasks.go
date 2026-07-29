package web

import (
	"context"
	"sync"
)

// taskOwner owns background work started by the Web transport. Admission and
// WaitGroup.Add share a mutex with Close, so an accepted login poll can finish
// its application-defined credential commit before shutdown returns.
type taskOwner struct {
	mu       sync.Mutex
	stopping bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newTaskOwner() *taskOwner {
	ctx, cancel := context.WithCancel(context.Background())
	return &taskOwner{ctx: ctx, cancel: cancel}
}

// Run admits task unless shutdown has started. The task receives a context
// cancelled by Close; it is responsible for honoring cancellation up to its
// own commit boundary.
func (owner *taskOwner) Run(task func(context.Context)) bool {
	if owner == nil || task == nil {
		return false
	}
	owner.mu.Lock()
	if owner.stopping {
		owner.mu.Unlock()
		return false
	}
	owner.wg.Add(1)
	ctx := owner.ctx
	owner.mu.Unlock()
	go func() {
		defer owner.wg.Done()
		task(ctx)
	}()
	return true
}

// Close is idempotent. It rejects later work, cancels admitted work, and waits
// until every admitted task exits.
func (owner *taskOwner) Close() {
	if owner == nil {
		return
	}
	owner.mu.Lock()
	if !owner.stopping {
		owner.stopping = true
		owner.cancel()
	}
	owner.mu.Unlock()
	owner.wg.Wait()
}
