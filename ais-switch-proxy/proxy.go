package main

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

type Proxy struct {
	cfg     *Config
	routes  map[string]*routeState // name -> state
}

type routeState struct {
	route       *Route
	auth        AuthProvider // top-level route auth
	client      *http.Client
	modelRoutes []modelRouteState // per-model routing entries (model_routing)
}

// modelRouteState is a compiled ModelRoute: which models it matches, and its
// own upstream/auth (so a single route can split requests across backends).
type modelRouteState struct {
	models   map[string]bool
	upstream string
	auth     AuthProvider
	authName string // "cqp" | "codex_oauth" | "static" | "none"
	modelMap map[string]string
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{cfg: cfg, routes: map[string]*routeState{}}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		st := &routeState{
			route: r,
			auth:  newAuthProvider(r.Auth, cfg),
			client: &http.Client{
				Timeout: 0, // no overall timeout for streaming
			},
		}
		// Compile per-model routing entries.
		for _, mr := range r.ModelRouting {
			mrs := modelRouteState{
				models:   map[string]bool{},
				upstream: mr.Upstream,
				auth:     newAuthProvider(mr.Auth, cfg),
				authName: mr.Auth,
				modelMap: mr.ModelMap,
			}
			for _, m := range mr.Models {
				mrs.models[m] = true
			}
			st.modelRoutes = append(st.modelRoutes, mrs)
		}
		p.routes[r.Name] = st
	}
	return p
}

func (p *Proxy) handler(w http.ResponseWriter, r *http.Request) {
	// Health-check endpoint.
	if r.URL.Path == "/health/status" || r.URL.Path == "/health" {
		w.WriteHeader(200)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		// List server-side models: forward to the claude route's upstream
		// (<upstream>/models, e.g. .../compass-api/v1/models) with CQP auth.
		// The client path /v1/models maps to upstream /models (the /v1 prefix is
		// already in the route's upstream base).
		p.serveModels(w, r)
		return
	}

	route := p.cfg.findRoute(r.URL.Path)
	if route == nil {
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	st := p.routes[route.Name]
	p.forward(st, w, r)
}

// serveModels lists server-side models by forwarding GET /v1/models to the first
// cqp-authed route's upstream at /models (e.g. .../compass-api/v1/models), with
// the CQP bearer key injected. The client path /v1/models maps to upstream /models
// because the /v1 version prefix is already part of the route's upstream base.
// Errors fall back to an empty list so clients don't hard-fail on listing.
func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	st := p.cqpRoute()
	if st == nil {
		writeModels(w, nil)
		return
	}
	// Build the upstream URL: <upstream>/models, preserving the client's query.
	target := strings.TrimRight(st.route.Upstream, "/") + "/models"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		writeModels(w, nil)
		return
	}
	if err := st.auth.Inject(req); err != nil {
		http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
		return
	}
	resp, err := st.client.Do(req)
	if err != nil {
		log.Printf("[models] upstream: %v", err)
		writeModels(w, nil)
		return
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		log.Printf("[models] upstream status=%d body=%s", resp.StatusCode, truncate(string(rb), 200))
		writeModels(w, nil)
		return
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	w.Write(rb)
}

// cqpRoute returns the first route using CQP auth (the gateway route), or nil.
func (p *Proxy) cqpRoute() *routeState {
	for _, st := range p.routes {
		if st.route.Auth == "cqp" {
			return st
		}
	}
	return nil
}

// writeModels emits an (empty or given) OpenAI-style model list.
func writeModels(w http.ResponseWriter, data []byte) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(200)
	if len(data) > 0 {
		w.Write(data)
		return
	}
	w.Write([]byte(`{"object":"list","data":[]}`))
}

// forward proxies a request: read body → rewrite model → inject auth → forward → stream back.
// Refreshes auth and retries once on an upstream 401.
func (p *Proxy) forward(st *routeState, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	model := extractModel(body)

	// Select upstream/auth/model_map for this request. If a model_routing entry
	// matches the request's model, it wins; otherwise the route's top-level
	// fields are used.
	upstream := st.route.Upstream
	auth := st.auth
	modelMap := st.route.ModelMap
	authName := st.route.Auth
	for _, mr := range st.modelRoutes {
		if mr.models[model] {
			upstream = mr.upstream
			auth = mr.auth
			modelMap = mr.modelMap
			authName = mr.authName
			break
		}
	}

	// The chatgpt.com codex backend requires store:false in the request body
	// (it rejects with 400 "Store must be set to false"). Inject it for
	// codex_oauth requests only; gateway requests are left untouched.
	if authName == "codex_oauth" {
		body = ensureJSONField(body, "store", false)
	}

	// Rewrite the model alias (if configured) before forwarding.
	mapped := model
	if m, ok := modelMap[model]; ok && m != "" {
		mapped = m
	}
	if mapped != model && mapped != "" {
		body = rewriteModel(body, mapped)
	}

	for attempt := 0; attempt < 2; attempt++ {
		// If upstream_path is empty, derive the upstream path from the client path.
		// The route's upstream base already contains the version prefix (e.g.
		// .../compass-api/v1), so strip the client's leading /v1 to avoid a
		// doubled /v1/v1 path (which the gateway rejects for /v1/responses).
		path := r.URL.Path
		if st.route.UpstreamPath != "" {
			path = st.route.UpstreamPath
		} else if strings.HasPrefix(path, "/v1/") {
			path = strings.TrimPrefix(path, "/v1")
		}
		// Build the upstream URL. For the claude/messages route with CQP auth, the
		// gateway expects ?beta=true (matches AIS Switch); append it preserving any
		// existing query.
		targetURL := strings.TrimRight(upstream, "/") + path
		isMessagesCQP := authName == "cqp" && strings.Contains(path, "/messages")
		if isMessagesCQP && !strings.Contains(targetURL, "beta=") {
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery + "&beta=true"
			} else {
				targetURL += "?beta=true"
			}
		} else if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Copy only a whitelist of client headers (matches AIS Switch / ais-switch-cli
		// forwarder), not all — avoids leaking the client's Authorization/Cookie/other
		// headers to the upstream beyond what's intended.
		copyHeaderWhitelist(req.Header, r.Header,
			"content-type", "accept", "user-agent", "x-session-id",
			"user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id",
			"prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if err := auth.Inject(req); err != nil {
			http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
			return
		}
		// Gateway-specific headers for the claude/messages route (matches AIS Switch):
		// anthropic-version is always sent; x-compass-request-id is a per-request UUID
		// the gateway expects on managed-key requests.
		if strings.Contains(upstream, "compass") {
			req.Header.Set("anthropic-version", "2023-06-01")
			if authName == "cqp" {
				req.Header.Set("x-compass-request-id", newRequestID())
			}
		}

		start := time.Now()
		resp, err := st.client.Do(req)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}

		// 401 → refresh and retry once.
		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			log.Printf("[route=%s] %s",
				st.route.Name, cl(ansiYellow, "401, refreshing auth and retrying"))
			if rerr := auth.Refresh(); rerr != nil {
				http.Error(w, "auth refresh: "+rerr.Error(), http.StatusUnauthorized)
				return
			}
			continue
		}

		log.Printf("[route=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
			st.route.Name, r.Method, r.URL.Path, model, mapped,
			statusColor(resp.StatusCode, fmt.Sprintf("%d", resp.StatusCode)),
			time.Since(start).Milliseconds(), len(body))

		// Copy response headers and body back (streaming flush).
		// Full copy is fine here — these are the upstream's response headers
		// (content-type, etc.) we want to pass to the client.
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		flushCopy(w, resp.Body)
		resp.Body.Close()
		return
	}
}

// flushCopy reads, writes, and flushes per chunk, supporting SSE streaming.
func flushCopy(w http.ResponseWriter, rc io.ReadCloser) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			break
		}
	}
}

// copyHeaderWhitelist copies only the named headers from src to dst (first value
// only). Matches the AIS Switch / ais-switch-cli forwarder whitelist so the
// client's Authorization/Cookie/other headers don't leak upstream.
func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

// newRequestID returns a random UUID v4 string, for x-compass-request-id.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to a time-based value.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// extractModel reads the model field from the JSON body.
func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

// rewriteModel replaces the model field in the JSON body, preserving the rest.
func rewriteModel(body []byte, newModel string) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	v["model"] = newModel
	out, err := json.Marshal(v)
	if err != nil {
		return body
	}
	return out
}

// ensureJSONField sets body[key] = val if the key is absent. Used to inject
// required fields the upstream expects (e.g. store:false for the codex backend)
// without overwriting a value the client already set.
func ensureJSONField(body []byte, key string, val any) []byte {
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	if _, ok := v[key]; !ok {
		v[key] = val
		out, err := json.Marshal(v)
		if err != nil {
			return body
		}
		return out
	}
	return body
}
