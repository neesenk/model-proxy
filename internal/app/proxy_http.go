package app

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/httpx"
	"model-proxy/internal/observe/counters"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	// Registers the pprof handlers on http.DefaultServeMux; the Proxy handler
	// dispatches into that mux only when MP_PPROF=1 was set at construction.
	_ "net/http/pprof"

	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	webtransport "model-proxy/internal/web"
)

// reqIDPrefix is a per-process 8-hex-char nonce (generated once from crypto/rand
// at package init via httpx.NewRequestID). Combined with an atomic counter, this gives
// each request a unique id with ONE atomic add (no crypto/rand syscall per
// request). Always generated — even when request_log is off — so live start↔end
// event pairing + Live↔Requests cross-page linking work.
var reqIDPrefix = httpx.NewRequestID()[:8]
var reqIDCounter atomic.Uint64

func nextRequestID() string {
	return fmt.Sprintf("%s-%010d", reqIDPrefix, reqIDCounter.Add(1))
}

func (p *Proxy) Handler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/health/status" || r.URL.Path == "/health" {
		w.WriteHeader(200)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		p.serveModels(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/debug/schedule" {
		// Routing/pin/sticky metadata: same loopback trust boundary as the
		// admin API for browser-shaped requests (CLI/curl passes untouched).
		if !webtransport.GuardBrowserOrigin(w, r) {
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write(p.scheduleStatus())
		return
	}
	if p.pprofEnabled && strings.HasPrefix(r.URL.Path, "/debug/pprof") {
		// Live profiling (MP_PPROF=1). Same browser-origin guard as the other
		// debug surfaces; the pprof handlers self-register on DefaultServeMux.
		if !webtransport.GuardBrowserOrigin(w, r) {
			return
		}
		http.DefaultServeMux.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
		// Only reachable with web.enabled=false (the web transport serves the
		// endpoint through its guarded /api/ subtree otherwise).
		if !webtransport.GuardBrowserOrigin(w, r) {
			return
		}
		observeevents.ServeEvents(p.events, w, r)
		return
	}
	proto := string(protocol.ForPath(r.URL.Path))
	// Generate the request id ONCE, here at the handler top, so EVERY downstream
	// path — including the unknown-path 502 below, which returns before forward —
	// can publish live events carrying a stable id (the contract: every start/end
	// carries a stable request_id; cache-hit and 400/502 terminals must produce an
	// end). Cheap: one atomic add, no data dependency.
	requestID := nextRequestID()
	if proto == "" {
		p.publishTerminalEvent(requestID, r, "", r.URL.Path, http.StatusBadGateway)
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	p.forward(proto, w, r, requestID)
}

func (*Proxy) responsesPreviousID(body []byte) string {
	return protocol.PreviousResponseID(body)
}

// serveModels lists all exposed models (from routes) merged with provider
// metadata (context/output from providers[].models).
func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	data := p.exposedModelsJSON()
	writeModels(w, data)
}

// exposedModelsJSON builds an OpenAI-style model list from all exposed model
// names (routes' keys) plus the claude_mapping keys (so anthropic clients can
// discover claude-* aliases too).
func (p *Proxy) exposedModelsJSON() []byte {
	p.mu.RLock()
	cfg := p.cfg
	implicit := p.implicitRoutes
	p.mu.RUnlock()
	type m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var models []m
	seen := map[string]bool{}
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, m{ID: id, Object: "model"})
	}
	for exposed := range cfg.Routes {
		add(exposed)
	}
	for exposed := range implicit { // implicitly-routable models are callable → listable
		add(exposed)
	}
	for claude := range cfg.ClaudeMapping {
		add(claude)
	}
	out, _ := json.Marshal(map[string]any{"object": "list", "data": models})
	return out
}

func writeModels(w http.ResponseWriter, data []byte) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	if len(data) > 0 {
		w.Write(data)
		return
	}
	w.Write([]byte(`{"object":"list","data":[]}`))
}

// publishTerminalEvent emits a live "end" event for a request that ends before
// the normal start/commit flow — a malformed body (400) or an unrouted model
// (502). Without it, an agent retry-looping on a missing/removed model is
// invisible to the live monitor, defeating the feature's core use case. The
// requestID is generated at the handler top and threaded in so these terminal
// events still pair with a stable id (the contract: 400/502 终局也必须产生 end
// 且带稳定 request_id).
func (p *Proxy) publishTerminalEvent(requestID string, r *http.Request, proto, exposed string, status int) {
	p.events.Publish(observeevents.Event{
		Type:      "end",
		Ts:        time.Now().UnixMilli(),
		RequestID: requestID,
		Agent:     counters.DetectAgent(r),
		Protocol:  proto,
		Exposed:   exposed,
		Status:    status,
	})
}
