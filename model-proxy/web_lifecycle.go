package main

import (
	"context"
	"sync"
)

// webTaskOwner owns background work started by the optional Web component.
// Admission and WaitGroup.Add share a mutex with Close so no login poll can be
// added after shutdown begins. Cancelling the root context stops long-running
// polls; Close then waits for any task already inside its credential-commit
// section before Proxy-owned state may be closed.
type webTaskOwner struct {
	mu       sync.Mutex
	stopping bool
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

func newWebTaskOwner() *webTaskOwner {
	ctx, cancel := context.WithCancel(context.Background())
	return &webTaskOwner{ctx: ctx, cancel: cancel}
}

func (owner *webTaskOwner) run(task func(context.Context)) bool {
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

func (owner *webTaskOwner) close() {
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
