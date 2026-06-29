package main

import (
	"bytes"
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
	route  *Route
	auth   AuthProvider
	client *http.Client
}

func NewProxy(cfg *Config) *Proxy {
	p := &Proxy{cfg: cfg, routes: map[string]*routeState{}}
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		p.routes[r.Name] = &routeState{
			route: r,
			auth:  newAuthProvider(r, cfg),
			client: &http.Client{
				Timeout: 0, // no overall timeout for streaming
			},
		}
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
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"object":"list","data":[]}`))
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
	mapped := model
	if m, ok := st.route.ModelMap[model]; ok && m != "" {
		mapped = m
	}
	if mapped != model && mapped != "" {
		body = rewriteModel(body, mapped)
	}

	for attempt := 0; attempt < 2; attempt++ {
		upstream := st.route.Upstream
		// If upstream_path is empty, keep the original path.
		path := r.URL.Path
		if st.route.UpstreamPath != "" {
			path = st.route.UpstreamPath
		}
		// Preserve the query string.
		targetURL := strings.TrimRight(upstream, "/") + path
		if r.URL.RawQuery != "" {
			targetURL += "?" + r.URL.RawQuery
		}

		req, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build upstream req: "+err.Error(), http.StatusInternalServerError)
			return
		}
		// Copy client headers (drop hop-by-hop and host).
		copyHeaders(req.Header, r.Header)
		req.Header.Del("Host")
		req.Header.Del("Content-Length")
		req.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))

		if err := st.auth.Inject(req); err != nil {
			http.Error(w, "auth: "+err.Error(), http.StatusUnauthorized)
			return
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
			if rerr := st.auth.Refresh(); rerr != nil {
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
		copyHeaders(w.Header(), resp.Header)
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

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
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
