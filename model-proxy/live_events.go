package main

import (
	"net/http"

	observeevents "model-proxy/internal/observe/events"
)

// serveEvents is the Proxy adapter for the SSE /api/events endpoint; the
// handler itself lives in internal/observe/events.
func (p *Proxy) serveEvents(w http.ResponseWriter, r *http.Request) {
	observeevents.ServeEvents(p.events, w, r)
}
