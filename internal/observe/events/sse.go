package events

import (
	"encoding/json"
	"net/http"
	"time"
)

// serveEvents is the SSE handler for /api/events. It subscribes, writes the recent
// ring as a burst, then streams new events as `data: {json}\n\n` until the client
// disconnects. A periodic comment keepalive prevents idle proxies from closing the
// connection. Nil-hub-safe (a Proxy without one answers an empty stream).
func ServeEvents(hub *Hub, w http.ResponseWriter, r *http.Request) {
	serveEventsWithTicks(hub, w, r, nil)
}

// serveEventsWithTicks is the SSE entry core. A nil tick channel preserves the
// production 15-second ticker; tests supply a channel to drive the complete
// response path without sleeping or bypassing headers and initial flushes.
func serveEventsWithTicks(hub *Hub, w http.ResponseWriter, r *http.Request, ticks <-chan time.Time) {
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

	if hub == nil {
		// No hub: hold the connection open with keepalives so the client doesn't
		// spin reconnects.
		if ticks == nil {
			keepaliveLoop(w, flusher, r)
		} else {
			keepaliveLoopWithTicks(w, flusher, r, ticks)
		}
		return
	}
	ch, recent, cancel := hub.Subscribe()
	defer cancel()

	writeEvent := func(e Event) {
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
	if ticks == nil {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if !writeKeepalive(w, flusher) {
				return
			}
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
	keepaliveLoopWithTicks(w, flusher, r, ticker.C)
}

func keepaliveLoopWithTicks(w http.ResponseWriter, flusher http.Flusher, r *http.Request, ticks <-chan time.Time) {
	for {
		select {
		case <-r.Context().Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if !writeKeepalive(w, flusher) {
				return
			}
		}
	}
}

func writeKeepalive(w http.ResponseWriter, flusher http.Flusher) bool {
	if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
		return false
	}
	flusher.Flush()
	return true
}
