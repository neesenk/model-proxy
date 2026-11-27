package events

import (
	"encoding/json"
	"io"
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

// shutdownNotice is the last frame of every /api/events stream once the
// daemon is stopping. `retry:` is the EventSource's own reconnection delay:
// the page does not re-implement reconnecting, the browser retries every
// second until the daemon is back and then replays the ring as on any
// connect. The notice is a NAMED event — it reaches addEventListener
// ('shutdown'), never the onmessage handler that folds request events — so
// the page can show "restarting" instead of an error without parsing it.
const shutdownNotice = "retry: 1000\nevent: shutdown\ndata: {\"type\":\"shutdown\"}\n\n"

// writeShutdownNotice ends a stream with the shutdown notice, flushed.
func writeShutdownNotice(w http.ResponseWriter, flusher http.Flusher) {
	_, _ = io.WriteString(w, shutdownNotice)
	flusher.Flush()
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
	// A daemon already draining answers a (re)connecting page with the notice
	// at once: nothing is replayed to a leaving process, and this handler
	// returns immediately so the reconnect cannot hold the drain either.
	if hub.Stopping() {
		writeShutdownNotice(w, flusher)
		return
	}
	// SubscribeContext: the subscription dies with the request context even if
	// a future refactor drops the explicit cancel below — a forgotten cancel
	// would otherwise leak the buffered channel for the process lifetime.
	ch, recent, cancel := hub.SubscribeContext(r.Context())
	defer cancel()

	writeEvent := func(e Event) bool {
		b, _ := json.Marshal(e)
		// A dead connection must end the loop, not spin writes into it until
		// the context or the next keepalive notices (mirrors writeKeepalive).
		if _, err := w.Write([]byte("data: ")); err != nil {
			return false
		}
		if _, err := w.Write(b); err != nil {
			return false
		}
		if _, err := w.Write([]byte("\n\n")); err != nil {
			return false
		}
		return true
	}
	// Replay recent history first.
	for _, e := range recent {
		if !writeEvent(e) {
			return
		}
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
		case <-hub.stopping:
			// The daemon is stopping: tell the page and let go of the handler
			// — the drain then finishes without waiting for this stream.
			writeShutdownNotice(w, flusher)
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if !writeKeepalive(w, flusher) {
				return
			}
		case e := <-ch:
			if !writeEvent(e) {
				return
			}
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
