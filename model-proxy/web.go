package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

//go:embed web_assets/*
var webAssets embed.FS

// webServer serves the admin UI (/ui/) and the JSON API (/api/). It is created
// by runProxy when cfg.Web.Enabled and registered on the same mux as the proxy
// handler. The proxy's own "/" handler keeps working — ServeMux gives /ui/ and
// /api/ precedence over "/".
type webServer struct {
	p          *Proxy
	configFile string
}

// newWebServer builds a webServer bound to a proxy (for live state) and the
// on-disk config path (for validate-before-write + saveAndReload).
func newWebServer(p *Proxy, configFile string) *webServer {
	return &webServer{p: p, configFile: configFile}
}

// register mounts /ui/ (static assets) and /api/ (JSON) on the mux.
func (w *webServer) register(mux *http.ServeMux) {
	mux.HandleFunc("/ui/", w.serveUI)
	mux.HandleFunc("/api/", w.serveAPI)
}

// serveUI serves embedded assets under /ui/. /ui/ maps to index.html; a trailing
// slash (e.g. /ui/) also maps to index.html. Path traversal (any ".." segment)
// is rejected with 404. Task 5 replaces serveAPI with a real router.
func (w *webServer) serveUI(resp http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/ui/")
	if name == "" || strings.HasSuffix(name, "/") {
		name = "index.html"
	}
	if strings.Contains(name, "..") {
		http.NotFound(resp, r)
		return
	}
	data, err := webAssets.ReadFile("web_assets/" + name)
	if err != nil {
		http.NotFound(resp, r)
		return
	}
	resp.Header().Set("content-type", contentTypeFor(name))
	resp.Write(data)
}

// serveAPI is a catch-all for /api/ until Task 5 wires the real router.
func (w *webServer) serveAPI(resp http.ResponseWriter, r *http.Request) {
	writeJSONErr(resp, http.StatusNotFound, "no api route for "+r.URL.Path)
}

// contentTypeFor maps an asset filename to its Content-Type.
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(resp http.ResponseWriter, code int, v any) {
	resp.Header().Set("content-type", "application/json")
	resp.WriteHeader(code)
	fmt.Fprint(resp, jsonMust(v))
}

// writeJSONErr writes a {"error": msg} JSON response.
func writeJSONErr(resp http.ResponseWriter, code int, msg string) {
	writeJSON(resp, code, map[string]string{"error": msg})
}

// jsonMust marshals v, panicking on failure (used only for in-memory values
// where encoding/json cannot fail in practice).
func jsonMust(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("jsonMust: %v", err))
	}
	return string(b)
}
