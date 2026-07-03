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

	"model-proxy/provider"
)

// Proxy holds the compiled provider instances + the config.
type Proxy struct {
	mu        sync.RWMutex
	cfg       *Config
	providers map[string]provider.Provider // provider name → Provider (shared)
	client    *http.Client
}

// buildProviders creates provider.Provider instances from config, wiring the
// main package's existing AuthProvider/Login/Logout/Usage functions as callbacks.
func buildProviders(cfg *Config) map[string]provider.Provider {
	m := map[string]provider.Provider{}
	for name, prov := range cfg.Providers {
		auth := newAuthProvider(prov.Provider, name, cfg)
		pcfg := &provider.Config{
			ProviderID: prov.Provider,
			BaseURL:    prov.BaseURL,
			Headers:    prov.Headers,
			UsageURL:   prov.UsageURL,
			Auth:       authAdapter{auth},
		}
		// Wire callbacks by provider type.
		switch prov.Provider {
		case "compass":
			pcfg.LoginFn = func() error { return runLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearAccount(cfg.Auth.SSOCookieFile) }
			pcfg.UsageFn = func() (any, error) { return showCompassUsageData(cfg) }
		case "codex":
			pcfg.LoginFn = func() error { return runCodexLogin(cfg) }
			pcfg.LogoutFn = func() error { return clearCodexAuth(cfg) }
			pcfg.UsageFn = func() (any, error) { return showCodexUsageData(cfg, prov) }
		case "zhipu":
			pcfg.LoginFn = func() error { return runApiKeyLoginErr(cfg, name, prov) }
			pcfg.LogoutFn = func() error { return clearApiKey(name) }
			pcfg.UsageFn = func() (any, error) { return showZhipuUsageData(cfg, name, prov) }
		}
		p, err := provider.New(pcfg, name)
		if err != nil {
			log.Printf("[proxy] failed to build provider %s: %v (using auth-only)", name, err)
			continue
		}
		m[name] = p
	}
	return m
}

// authAdapter bridges main.AuthProvider → provider.Authenticator.
type authAdapter struct{ inner AuthProvider }

func (a authAdapter) Inject(req *http.Request) error  { return a.inner.Inject(req) }
func (a authAdapter) Refresh() error                    { return a.inner.Refresh() }

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{
		cfg:       cfg,
		providers: buildProviders(cfg),
		client:    &http.Client{Timeout: 0},
	}
	return p
}

func (p *Proxy) reload(configPath string) error {
	cfg, err := LoadConfig(configPath)
	if err != nil {
		return err
	}
	newProviders := buildProviders(cfg)
	p.mu.Lock()
	p.cfg = cfg
	p.providers = newProviders
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
	provImpl := p.providers[provName]

	// Rewrite model to the real name if different.
	if realModel != model {
		body = rewriteModel(body, realModel)
	}

	// codex backend requires store:false.
	if prov.Provider == "codex" {
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
		if prov.Provider == "compass" && strings.Contains(upPath, "/messages") && !strings.Contains(targetURL, "beta=") {
			if r.URL.RawQuery != "" {
				targetURL += "?" + r.URL.RawQuery + "&beta=true"
			} else {
				targetURL += "?beta=true"
			}
		} else if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
			return
		}
		copyHeaderWhitelist(req.Header, r.Header,
			"content-type", "accept", "user-agent", "x-session-id",
			"user_id", "x-claude-code-session-id", "x-interaction-type", "x-interaction-id",
			"prompt_cache_key", "x-anthropic-billing-header", "anthropic-beta", "accept-language")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if err := provImpl.AuthHeaders(req); err != nil {
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
			if prov.Provider == "compass" {
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
			if rerr := provImpl.Refresh(); rerr != nil {
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
// Stops immediately if the client disconnects (write error), so the proxy
// doesn't keep pulling the upstream stream after the client is gone.
func flushCopy(w http.ResponseWriter, rc io.ReadCloser) {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client disconnected — stop reading upstream.
				break
			}
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
