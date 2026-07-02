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
	"sync"
	"time"
)

// Proxy holds the compiled provider auth instances + the config.
type Proxy struct {
	mu        sync.RWMutex
	cfg       *Config
	authCache map[string]AuthProvider // provider name → AuthProvider (shared)
	client    *http.Client
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		cfg:       cfg,
		authCache: map[string]AuthProvider{},
		client:    &http.Client{Timeout: 0}, // no overall timeout for streaming
	}
	// Build one AuthProvider per provider (shared across requests).
	for name, prov := range cfg.Providers {
		p.authCache[name] = newAuthProvider(prov.Auth, cfg)
	}
	return p
}

// reload re-reads the config file and atomically swaps cfg + authCache.
// Called on SIGHUP (serve reload).
func (p *Proxy) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	authCache := map[string]AuthProvider{}
	for name, prov := range cfg.Providers {
		authCache[name] = newAuthProvider(prov.Auth, cfg)
	}
	p.mu.Lock()
	p.cfg = cfg
	p.authCache = authCache
	p.mu.Unlock()
	return nil
}

func (p *Proxy) handler(w http.ResponseWriter, r *http.Request) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if r.URL.Path == "/health/status" || r.URL.Path == "/health" {
		w.WriteHeader(200)
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		p.serveModels(w, r)
		return
	}
	proto := protocolForPath(r.URL.Path)
	if proto == "" {
		http.Error(w, fmt.Sprintf("no route for path %s", r.URL.Path), http.StatusBadGateway)
		return
	}
	p.forward(proto, w, r)
}

// serveModels lists all exposed models (from routes) merged with provider
// metadata (context/output from providers[].models).
func (p *Proxy) serveModels(w http.ResponseWriter, r *http.Request) {
	data := p.exposedModelsJSON()
	writeModels(w, data)
}

// exposedModelsJSON builds an OpenAI-style model list from all routes' models.
func (p *Proxy) exposedModelsJSON() []byte {
	type m struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	}
	var models []m
	seen := map[string]bool{}
	for _, route := range p.cfg.Routes {
		for exposed := range route.Models {
			if seen[exposed] {
				continue
			}
			seen[exposed] = true
			ownedBy := ""
			if parts := strings.SplitN(exposed, "/", 2); len(parts) == 2 {
				ownedBy = parts[0]
			}
			models = append(models, m{ID: exposed, Object: "model", OwnedBy: ownedBy})
		}
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

// forward proxies a request to the provider selected by the model→provider/model
// mapping in the route for this protocol. The protocol (anthropic|openai)
// determines both the client path and the upstream path (same protocol, no
// conversion). Refreshes auth and retries once on 401.
func (p *Proxy) forward(proto string, w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	r.Body.Close()

	model := extractModel(body)

	// Look up the route for this protocol.
	route, ok := p.cfg.Routes[proto]
	if !ok {
		http.Error(w, fmt.Sprintf("no route for protocol %s", proto), http.StatusBadGateway)
		return
	}
	// Resolve exposed model → provider/realModel.
	target, ok := route.Models[model]
	if !ok {
		http.Error(w, fmt.Sprintf("model %q not found in route %s", model, proto), http.StatusBadGateway)
		return
	}
	parts := strings.SplitN(target, "/", 2)
	if len(parts) != 2 {
		http.Error(w, fmt.Sprintf("invalid model mapping %q", target), http.StatusBadGateway)
		return
	}
	provName, realModel := parts[0], parts[1]
	prov, ok := p.cfg.Providers[provName]
	if !ok {
		http.Error(w, fmt.Sprintf("unknown provider %q", provName), http.StatusBadGateway)
		return
	}
	auth := p.authCache[provName]

	// Rewrite model to the real name if different.
	if realModel != model {
		body = rewriteModel(body, realModel)
	}

	// codex backend requires store:false.
	if prov.Auth == "codex_oauth" {
		body = ensureJSONField(body, "store", false)
	}

	// Determine the upstream path from the client path (same protocol, no
	// conversion). Strip /v1 prefix since the provider baseURL already has
	// the version (e.g. .../compass-api/v1).
	upPath := r.URL.Path
	if strings.HasPrefix(upPath, "/v1/") {
		upPath = strings.TrimPrefix(upPath, "/v1")
	}

	for attempt := 0; attempt < 2; attempt++ {
		targetURL := strings.TrimRight(prov.BaseURL, "/") + upPath
		// compass + anthropic /messages needs ?beta=true.
		if prov.Auth == "cqp" && strings.Contains(upPath, "/messages") && !strings.Contains(targetURL, "beta=") {
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
		copyHeaderWhitelist(req.Header, r.Header,
			"content-type", "accept", "user-agent", "x-session-id",
			"user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id",
			"prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if err := auth.Inject(req); err != nil {
			http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
			return
		}
		// Extra provider-specific headers from config.
		for k, v := range prov.Headers {
			req.Header.Set(k, v)
		}
		// Compass-specific headers.
		if strings.Contains(prov.BaseURL, "compass") {
			req.Header.Set("anthropic-version", "2023-06-01")
			if prov.Auth == "cqp" {
				req.Header.Set("x-compass-request-id", newRequestID())
			}
		}

		start := time.Now()
		resp, err := p.client.Do(req)
		if err != nil {
			http.Error(w, "upstream: "+err.Error(), http.StatusBadGateway)
			return
		}

		if resp.StatusCode == 401 && attempt == 0 {
			resp.Body.Close()
			log.Printf("[proto=%s provider=%s] %s",
				proto, provName, cl(ansiYellow, "401, refreshing auth and retrying"))
			if rerr := auth.Refresh(); rerr != nil {
				http.Error(w, "auth refresh: "+rerr.Error(), http.StatusUnauthorized)
				return
			}
			continue
		}

		log.Printf("[proto=%s provider=%s] %s %s model=%s→%s status=%s %dms bytes=%d",
			proto, provName, r.Method, r.URL.Path, model, realModel,
			statusColor(resp.StatusCode, fmt.Sprintf("%d", resp.StatusCode)),
			time.Since(start).Milliseconds(), len(body))

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

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}

func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func extractModel(body []byte) string {
	var v struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.Model
}

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
