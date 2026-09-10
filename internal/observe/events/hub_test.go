package events

import (
	"testing"
	"time"
)

func TestHubPublishSubscribeRecent(t *testing.T) {
	h := NewHub()
	h.Publish(Event{Type: "end", Agent: "claude-code", Exposed: "glm", Status: 200})

	ch, recent, cancel := h.Subscribe()
	if len(recent) != 1 || recent[0].Agent != "claude-code" {
		t.Errorf("recent replay = %+v, want one claude-code event", recent)
	}
	recent[0].Agent = "mutated"
	if got := h.Snapshot()[0].Agent; got != "claude-code" {
		t.Errorf("Subscribe recent snapshot mutated Hub storage: Agent = %q", got)
	}
	h.Publish(Event{Type: "end", Agent: "codex", Status: 500})
	select {
	case event := <-ch:
		if event.Agent != "codex" || event.Status != 500 {
			t.Errorf("event = %+v, want codex/500", event)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive published event")
	}

	cancel()
	cancel() // cancellation is idempotent
	h.Publish(Event{Type: "end", RequestID: "after-cancel"})
	select {
	case event := <-ch:
		t.Errorf("received event after cancel: %+v", event)
	default:
	}
}

func TestHubDropsSlowSubscriber(t *testing.T) {
	h := NewHub()
	ch, _, cancel := h.Subscribe()
	defer cancel()
	for i := 0; i < 32; i++ {
		h.Publish(Event{Type: "end", Status: i})
	}
	done := make(chan struct{})
	go func() {
		h.Publish(Event{Type: "end", RequestID: "overflow"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on a full subscriber buffer")
	}
	for want := 0; want < 32; want++ {
		select {
		case event := <-ch:
			if event.RequestID == "overflow" || event.Status != want {
				t.Fatalf("buffered event %d = %+v, want original status %d", want, event, want)
			}
		default:
			t.Fatalf("received only %d buffered events, want 32", want)
		}
	}
	select {
	case event := <-ch:
		t.Fatalf("received overflow event instead of dropping it: %+v", event)
	default:
	}
}

func TestHubRecentRingAndFindEnd(t *testing.T) {
	h := NewHub()
	for i := 0; i < recentCap+5; i++ {
		h.Publish(Event{Type: "start", RequestID: "other"})
	}
	h.Publish(Event{Type: "end", RequestID: "wanted", Status: 200})
	h.Publish(Event{Type: "end", RequestID: "wanted", Status: 201})

	snapshot := h.Snapshot()
	if len(snapshot) != recentCap {
		t.Fatalf("Snapshot length = %d, want %d", len(snapshot), recentCap)
	}
	snapshot[0].Type = "mutated"
	if h.Snapshot()[0].Type == "mutated" {
		t.Fatal("Snapshot returned mutable internal storage")
	}
	event, ok := h.FindEnd("wanted")
	if !ok || event.Status != 201 {
		t.Fatalf("FindEnd = %+v, %v; want newest status 201", event, ok)
	}
	if _, ok := h.FindEnd("missing"); ok {
		t.Fatal("FindEnd reported a missing request")
	}
}

func TestNilHubPublishAndFindEnd(t *testing.T) {
	var h *Hub
	h.Publish(Event{Type: "end"})
	if snapshot := h.Snapshot(); snapshot != nil {
		t.Fatalf("nil Hub Snapshot = %+v, want nil", snapshot)
	}
	if _, ok := h.FindEnd("missing"); ok {
		t.Fatal("nil Hub found an event")
	}
}

func TestHubHasSubscribers(t *testing.T) {
	h := NewHub()
	if h.HasSubscribers() {
		t.Fatal("new hub should have no subscribers")
	}
	_, _, cancel := h.Subscribe()
	if !h.HasSubscribers() {
		t.Fatal("hub should report subscribers after Subscribe")
	}
	cancel()
	if h.HasSubscribers() {
		t.Fatal("hub should report no subscribers after cancel")
	}
	var nilHub *Hub
	if nilHub.HasSubscribers() {
		t.Fatal("nil hub should report no subscribers")
	}
}
