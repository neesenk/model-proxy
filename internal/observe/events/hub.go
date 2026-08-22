// Package events owns the in-memory live-request event stream.
package events

import (
	"context"
	"sync"
)

const recentCap = 200

// Event is one request lifecycle event. Type "start" fires when a request
// enters forwarding; "end" fires when it commits or terminates early; "guard"
// fires when the outbound secret scan hits (Detail then carries the pattern
// type names and the configured action — never the matched content); "budget"
// fires when a configured monthly cost budget crosses (Detail carries the
// JSON {scope, month, threshold_usd, actual_usd} payload).
type Event struct {
	Type          string `json:"type"` // "start" | "end" | "guard"
	Ts            int64  `json:"ts"`   // unix milliseconds
	RequestID     string `json:"request_id"`
	Agent         string `json:"agent"`
	Protocol      string `json:"protocol"`
	Exposed       string `json:"exposed"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	Detail        string `json:"detail,omitempty"` // free-form context for non-lifecycle types
}

// Hub fans events out to subscribers and retains a bounded recent-event ring.
// Publish never blocks on a slow subscriber.
type Hub struct {
	mu     sync.Mutex
	subs   map[chan Event]struct{}
	recent []Event
}

func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}}
}

// Publish appends e to the recent ring and offers it to every subscriber.
func (h *Hub) Publish(e Event) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recent = append(h.recent, e)
	if len(h.recent) > recentCap {
		h.recent = h.recent[len(h.recent)-recentCap:]
	}
	subs := make([]chan Event, 0, len(h.subs))
	for ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Subscribe returns future events, a detached recent-event snapshot, and an
// idempotent cancellation function. Callers must cancel when done.
func (h *Hub) Subscribe() (<-chan Event, []Event, func()) {
	ch := make(chan Event, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	recent := append([]Event(nil), h.recent...)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, recent, cancel
}

// SubscribeContext is Subscribe plus an automatic release: the subscription
// is removed when ctx is done, so a caller that forgets cancel cannot leak a
// dead subscriber (and its buffered channel) for the process lifetime. The
// returned cancel still works for explicit early cleanup and is safe to call
// twice (AfterFunc's stop + the map delete are both idempotent).
func (h *Hub) SubscribeContext(ctx context.Context) (<-chan Event, []Event, func()) {
	ch, recent, cancel := h.Subscribe()
	stop := context.AfterFunc(ctx, cancel)
	return ch, recent, func() {
		stop()
		cancel()
	}
}

// Snapshot returns a detached copy of the recent-event ring.
func (h *Hub) Snapshot() []Event {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Event(nil), h.recent...)
}

// FindEnd returns the newest terminal event for requestID.
func (h *Hub) FindEnd(requestID string) (Event, bool) {
	if h == nil {
		return Event{}, false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.recent) - 1; i >= 0; i-- {
		if h.recent[i].Type == "end" && h.recent[i].RequestID == requestID {
			return h.recent[i], true
		}
	}
	return Event{}, false
}
