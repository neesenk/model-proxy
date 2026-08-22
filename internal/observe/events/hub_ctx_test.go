package events

import (
	"context"
	"testing"
	"time"
)

// SubscribeContext must release the subscription when the context is done
// even if the caller never invokes cancel (the leak the explicit-cancel
// contract could not prevent).
func TestSubscribeContextAutoReleases(t *testing.T) {
	h := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _, _ = h.SubscribeContext(ctx)
	cancel()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		n := len(h.subs)
		h.mu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("subscription survived context cancellation")
}

// Explicit cancel still works after SubscribeContext, and is safe to repeat.
func TestSubscribeContextExplicitCancelIdempotent(t *testing.T) {
	h := NewHub()
	ch, _, cancel := h.SubscribeContext(context.Background())
	cancel()
	cancel() // must not panic or deadlock
	select {
	case e := <-ch:
		t.Fatalf("received event %+v after cancel", e)
	default:
	}
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("subs = %d after explicit cancel", n)
	}
}

// A live subscription keeps delivering; auto-release only fires on ctx done.
func TestSubscribeContextStillDeliversUntilDone(t *testing.T) {
	h := NewHub()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, _, _ := h.SubscribeContext(ctx)
	h.Publish(Event{Type: "end", RequestID: "r1"})
	select {
	case e := <-ch:
		if e.RequestID != "r1" {
			t.Fatalf("event = %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no event delivered to live subscription")
	}
}
