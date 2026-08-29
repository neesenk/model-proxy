package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
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
		if n := len(*h.subs.Load()); n == 0 {
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
	if n := len(*h.subs.Load()); n != 0 {
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

// Exactly-once delivery across snapshot + channel: a Subscribe that lands
// while a Publish is in flight must receive that event either in its recent
// snapshot or on its channel — never both. (Publish loads the subscriber
// snapshot under the same lock as its ring append; loading it after the
// Unlock reopened a duplicate-delivery window when this COW hub landed.)
func TestHubPublishSubscribeExactlyOnce(t *testing.T) {
	h := NewHub()
	const publishers = 4
	const perPublisher = 200
	var pubWG sync.WaitGroup
	var published atomic.Int64
	start := make(chan struct{})
	for p := 0; p < publishers; p++ {
		pubWG.Add(1)
		go func(p int) {
			defer pubWG.Done()
			<-start
			for i := 0; i < perPublisher; i++ {
				h.Publish(Event{Type: "start", RequestID: fmt.Sprintf("r-%d-%d", p, i)})
				published.Add(1)
			}
		}(p)
	}

	type collector struct {
		seen map[string]int
		ch   <-chan Event
	}
	subscribe := func() collector {
		ch, recent, cancel := h.Subscribe()
		seen := make(map[string]int, len(recent))
		for _, e := range recent {
			seen[e.RequestID]++
		}
		cancel()
		return collector{seen: seen, ch: ch}
	}

	// Subscriptions must overlap live publishing, otherwise this test guards
	// nothing (the regression window only exists while a Publish is in flight).
	// Release the publishers, then wait for proof that events are flowing
	// before collecting.
	close(start)
	deadline := time.Now().Add(time.Second)
	for published.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if published.Load() == 0 {
		t.Fatal("publishers made no progress; overlap precondition failed")
	}
	const numCollectors = 6
	collectors := make([]collector, numCollectors)
	for i := range collectors {
		collectors[i] = subscribe()
	}

	pubWG.Wait()
	// Publishers are done and every subscription is cancelled: the channels
	// are quiescent, so a non-blocking drain is complete.
	total := 0
	for i := range collectors {
		drain := true
		for drain {
			select {
			case e := <-collectors[i].ch:
				collectors[i].seen[e.RequestID]++
			default:
				drain = false
			}
		}
		total += len(collectors[i].seen)
	}
	if total == 0 {
		t.Fatal("no collector observed any event; snapshot+channel overlap was never exercised")
	}
	for i := range collectors {
		for id, n := range collectors[i].seen {
			if n > 1 {
				t.Fatalf("event %s delivered %d times to one subscriber (snapshot + channel overlap)", id, n)
			}
		}
	}
}
