package web

import (
	"context"
	"testing"
	"time"
)

func TestWebTaskOwnerCloseCancelsAndWaitsThroughAdmittedCommit(t *testing.T) {
	owner := newTaskOwner()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{})

	if !owner.Run(func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
	}) {
		t.Fatal("task was not admitted before close")
	}
	<-started

	closed := make(chan struct{})
	go func() {
		owner.Close()
		close(closed)
	}()

	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("close did not cancel the Web task context")
	}
	select {
	case <-closed:
		t.Fatal("close returned before the admitted Web task finished")
	default:
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not return after the Web task finished")
	}
}

func TestWebTaskOwnerRejectsTasksAfterClose(t *testing.T) {
	owner := newTaskOwner()
	owner.Close()

	ran := make(chan struct{})
	if owner.Run(func(context.Context) { close(ran) }) {
		t.Fatal("task was admitted after close")
	}
	select {
	case <-ran:
		t.Fatal("rejected task still ran")
	default:
	}

	owner.Close()
}
