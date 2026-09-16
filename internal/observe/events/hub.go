// Package events owns the in-memory live-request event stream.
package events

import (
	"context"
	"sync"
	"sync/atomic"
)

const recentCap = 200

// Event is one request lifecycle event. Type "start" fires when a request
// enters forwarding; "end" fires when it commits or terminates early; "guard"
// fires when the outbound secret scan hits (Detail then carries the pattern
// type names and the configured action — never the matched content); "budget"
// fires when a configured monthly cost budget crosses (Detail carries the
// JSON {scope, month, threshold_usd, actual_usd} payload); "progress" fires
// as the committed upstream response body streams to the client.
type Event struct {
	Type      string `json:"type"` // "start" | "end" | "guard" | "budget" | "progress"
	Ts        int64  `json:"ts"`   // unix milliseconds
	RequestID string `json:"request_id"`
	// SessionID is the client session id resolved from the configured header
	// allowlist (request_log.session_headers); "" when the client sent none.
	SessionID     string `json:"session_id,omitempty"`
	Agent         string `json:"agent"`
	Protocol      string `json:"protocol"`
	Exposed       string `json:"exposed"`
	Provider      string `json:"provider"`
	UpstreamModel string `json:"upstream_model"`
	Status        int    `json:"status"`
	LatencyMs     int64  `json:"latency_ms"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheRead     uint64 `json:"cache_read,omitempty"`
	CacheCreation uint64 `json:"cache_creation,omitempty"`
	ReceivedBytes int64  `json:"received_bytes,omitempty"` // response bytes seen so far (progress only)
	Text          string `json:"text,omitempty"`           // head-capped response prefix (progress only)
	Detail        string `json:"detail,omitempty"`         // free-form context for non-lifecycle types
}

// Hub fans events out to subscribers and retains a bounded recent-event ring.
// Publish never blocks on a slow subscriber.
type Hub struct {
	mu     sync.Mutex
	recent []Event
	// subs is an immutable subscriber snapshot swapped copy-on-write under mu.
	// Publish loads it under the same lock as its recent-ring append (see
	// Publish for why the ordering matters) and without allocating — one
	// slice per subscription change instead of one per event.
	subs atomic.Pointer[[]chan Event]
}

func NewHub() *Hub {
	h := &Hub{}
	empty := []chan Event{}
	h.subs.Store(&empty)
	return h
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
	// The subscriber snapshot must be loaded under the same lock as the
	// append. Subscribe also holds mu while it registers the channel and
	// copies the recent ring, so this orders the two exactly: either the
	// subscriber's channel is in our snapshot (it gets e once, via delivery)
	// or it is not (its recent copy already contains e). Loading after the
	// Unlock instead would let a Subscribe land in between and deliver e
	// twice — once from the snapshot, once from the channel.
	subs := *h.subs.Load()
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
	current := *h.subs.Load()
	next := make([]chan Event, len(current)+1)
	copy(next, current)
	next[len(current)] = ch
	h.subs.Store(&next)
	recent := append([]Event(nil), h.recent...)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		current := *h.subs.Load()
		next := make([]chan Event, 0, len(current))
		for _, existing := range current {
			if existing != ch {
				next = append(next, existing)
			}
		}
		h.subs.Store(&next)
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

// HasSubscribers reports whether any active subscription exists. It is safe
// to call from the hot path; the subscriber snapshot is loaded atomically.
func (h *Hub) HasSubscribers() bool {
	if h == nil {
		return false
	}
	subs := h.subs.Load()
	return subs != nil && len(*subs) > 0
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
