package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"

	// Registers the pprof handlers on http.DefaultServeMux; the Proxy handler
	// dispatches into that mux only when MP_PPROF=1 was set at construction.
	_ "net/http/pprof"

	"model-proxy/internal/forward"
	"model-proxy/internal/httpx"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	webtransport "model-proxy/internal/web"
	"model-proxy/internal/webauth"
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
	// S2 surface auth. Admin endpoints riding the proxy handler (web-disabled
	// deployments) answer ONLY to the admin token; everything else under the
	// handler is the forward surface — gated by the api-keys file when
	// configured. /health stays open for liveness probes. Bearer and x-api-key
	// are both accepted (OpenAI vs Anthropic client convention).
	adminEndpoint := isAdminProxyEndpoint(r.URL.Path)
	var adminAuthEnabled bool
	var adminBrowserListen string
	if adminEndpoint {
		// Auth source and configured LAN hostname are swapped under p.mu during
		// reload. Capture both once so authentication and browser-origin policy
		// cannot observe different config generations.
		p.mu.RLock()
		admin := p.adminAuth.Load()
		if p.cfg != nil {
			adminBrowserListen = p.cfg.Listen
		}
		p.mu.RUnlock()
		adminAuthEnabled = admin != nil && admin.Enabled()
		if adminAuthEnabled {
			if !admin.Accept(webauth.BearerFromRequest(r.Header.Get)) {
				http.Error(w, "unauthorized: bad admin token", http.StatusUnauthorized)
				return
			}
		}
	} else if keys := p.apiKeys.Load(); keys != nil && keys.Enabled() {
		if !keys.Accept(webauth.BearerFromRequest(r.Header.Get)) {
			http.Error(w, "unauthorized: unknown or missing API key", http.StatusUnauthorized)
			return
		}
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		p.serveModels(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/debug/schedule" {
		// Routing/pin/sticky metadata: authenticated LAN browsers may use the
		// configured listen host/IP; unauthenticated mode remains loopback-only.
		if !webtransport.GuardAdminBrowserOrigin(w, r, adminAuthEnabled, adminBrowserListen) {
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write(p.scheduleStatus())
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/debug/route" {
		// Per-request routing decision preview: no upstream call, no scheduler
		// mutation (see serveRoutePreview).
		if !webtransport.GuardAdminBrowserOrigin(w, r, adminAuthEnabled, adminBrowserListen) {
			return
		}
		p.serveRoutePreview(w, r)
		return
	}
	if p.pprofEnabled && strings.HasPrefix(r.URL.Path, "/debug/pprof") {
		// Live profiling (MP_PPROF=1). Same browser-origin guard as the other
		// debug surfaces; the pprof handlers self-register on DefaultServeMux.
		if !webtransport.GuardAdminBrowserOrigin(w, r, adminAuthEnabled, adminBrowserListen) {
			return
		}
		http.DefaultServeMux.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/events" {
		// Only reachable with web.enabled=false (the web transport serves the
		// endpoint through its guarded /api/ subtree otherwise).
		if !webtransport.GuardAdminBrowserOrigin(w, r, adminAuthEnabled, adminBrowserListen) {
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
	derived := p.derivedRoutes
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
	for exposed := range derived { // derived-routable models are callable → listable
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
	forward.PublishTerminalEvent(p.events, requestID, r, proto, exposed, status)
}

// isAdminProxyEndpoint reports whether the path is an admin-surface endpoint
// that rides the proxy handler (deployments with web.enabled=false): the
// debug endpoints and the SSE events stream. The web transport applies the
// same token check to its own subtree, so both routes to the surface enforce
// the same boundary.
func isAdminProxyEndpoint(path string) bool {
	return path == "/debug/schedule" || path == "/debug/route" ||
		strings.HasPrefix(path, "/debug/pprof") || path == "/api/events"
}
