package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// live_events.go powers the real-time request monitor (#6): a fan-out hub that
// the forward path publishes request lifecycle events to, and an SSE endpoint
// (/api/events) that streams them to Web UI subscribers. The point is to see, in
// real time, which agent is sending, which upstream it routed to, the status, and
// the latency — so a misbehaving agent (e.g. retrying in a loop) is visible
// without jq-ing the request log.

const liveRecentCap = 200

// liveEvent is one request lifecycle event published to subscribers. Type "start"
// fires when a request enters forward (after routing); "end" fires on commit
// (served). The end event carries the chosen provider, status, and latency; input
// /output tokens are best-effort (filled when the usage scanner has committed by
// the time of publish, else 0).
type liveEvent struct {
	Type          string `json:"type"` // "start" | "end"
	Ts            int64  `json:"ts"`   // unix milliseconds
	RequestID     string `json:"request_id"`
	Agent         string `json:"agent"`
	Protocol      string `json:"protocol"`
	Exposed       string `json:"exposed"`        // route name
	Provider      string `json:"provider"`       // chosen upstream (end only)
	UpstreamModel string `json:"upstream_model"` // (end only)
	Status        int    `json:"status"`         // (end only)
	LatencyMs     int64  `json:"latency_ms"`     // (end only)
	Input         uint64 `json:"input"`          // best-effort (end only)
	Output        uint64 `json:"output"`         // best-effort (end only)
}

// eventHub fans out liveEvents to SSE subscribers and keeps a bounded ring of
// recent events so a newly-connected client sees recent history before going live.
// Publishing is non-blocking: a slow subscriber's full buffer is dropped to (the
// client misses events rather than the whole proxy stalling on a write).
type eventHub struct {
	mu     sync.Mutex
	subs   map[chan liveEvent]struct{}
	recent []liveEvent // ring, newest last; capped at liveRecentCap
}

func newEventHub() *eventHub {
	return &eventHub{subs: map[chan liveEvent]struct{}{}}
}

// publish appends e to the recent ring and fans it out to every subscriber
// (non-blocking; a full subscriber buffer is skipped).
func (h *eventHub) publish(e liveEvent) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.recent = append(h.recent, e)
	if len(h.recent) > liveRecentCap {
		h.recent = h.recent[len(h.recent)-liveRecentCap:]
	}
	subs := make([]chan liveEvent, 0, len(h.subs))
	for ch := range h.subs {
		subs = append(subs, ch)
	}
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- e:
		default: // subscriber too slow → drop this event for it (don't block forward)
		}
	}
}

// subscribe returns a channel of events plus a snapshot of recent history. The
// caller MUST call the returned canceler to unsubscribe (drops the buffered chan).
func (h *eventHub) subscribe() (<-chan liveEvent, []liveEvent, func()) {
	ch := make(chan liveEvent, 32)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	recent := append([]liveEvent(nil), h.recent...)
	h.mu.Unlock()
	cancel := func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
	return ch, recent, cancel
}

// serveEvents is the SSE handler for /api/events. It subscribes, writes the recent
// ring as a burst, then streams new events as `data: {json}\n\n` until the client
// disconnects. A periodic comment keepalive prevents idle proxies from closing the
// connection. Nil-hub-safe (a Proxy without one answers an empty stream).
func (p *Proxy) serveEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if p.events == nil {
		// No hub: hold the connection open with keepalives so the client doesn't
		// spin reconnects.
		keepaliveLoop(w, flusher, r)
		return
	}
	ch, recent, cancel := p.events.subscribe()
	defer cancel()

	writeEvent := func(e liveEvent) {
		b, _ := json.Marshal(e)
		w.Write([]byte("data: "))
		w.Write(b)
		w.Write([]byte("\n\n"))
	}
	// Replay recent history first.
	for _, e := range recent {
		writeEvent(e)
	}
	flusher.Flush()

	// Stream new events + a 15s keepalive comment.
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		case e := <-ch:
			writeEvent(e)
			flusher.Flush()
		}
	}
}

// keepaliveLoop holds an SSE connection open with only keepalive comments (used
// when there is no hub to subscribe to).
func keepaliveLoop(w http.ResponseWriter, flusher http.Flusher, r *http.Request) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
