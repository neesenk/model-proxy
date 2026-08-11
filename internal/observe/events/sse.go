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
		keepaliveLoop(w, flusher, r)
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
