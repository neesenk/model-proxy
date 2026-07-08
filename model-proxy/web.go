package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web_assets/*
var webAssets embed.FS

// webFS is the file system used by serveUI to read assets. It defaults to the
// embedded assets, but is a package-level variable (typed as fs.FS) so that
// tests can substitute a fake FS. This lets the path-traversal guard be tested
// in isolation — embed.FS itself rejects ".." via fs.ValidPath, so without
// this seam a test cannot prove the guard (not embed.FS) is what blocks a
// traversal request.
var webFS fs.FS = webAssets

// webServer serves the admin UI (/ui/) and the JSON API (/api/). It is created
// by runProxy when cfg.Web.Enabled and registered on the same mux as the proxy
// handler. The proxy's own "/" handler keeps working — ServeMux gives /ui/ and
// /api/ precedence over "/".
type webServer struct {
	p          *Proxy
	configFile string
	logFile    string // resolved at runProxy time; "" → fall back to cfg.LogFile
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
	data, err := fs.ReadFile(webFS, "web_assets/"+name)
	if err != nil {
		http.NotFound(resp, r)
		return
	}
	resp.Header().Set("content-type", contentTypeFor(name))
	resp.Write(data)
}

// serveAPI routes /api/ requests to their handlers. Only /api/status is wired
// today; later tasks add their own cases. Unknown /api paths fall through to a
// 404 JSON error.
func (w *webServer) serveAPI(resp http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/api/status" && r.Method == http.MethodGet:
		w.handleStatus(resp, r)
	case path == "/api/logs" && r.Method == http.MethodGet:
		w.handleLogs(resp, r)
	case path == "/api/config" && r.Method == http.MethodGet:
		w.handleConfigGet(resp, r)
	case path == "/api/config" && r.Method == http.MethodPost:
		w.handleConfigPut(resp, r)
	default:
		writeJSONErr(resp, http.StatusNotFound, "no api route for "+path)
	}
}

// handleStatus returns a dashboard snapshot: uptime, version, listen address,
// per-provider circuit/rate-limit health, quota snapshots, the current
// schedule (per-route ordered providers), and request counters.
//
// Lock discipline: each store is acquired and released in sequence — never
// nested. Mirrors scheduleStatus() (proxy.go): (1) p.mu.RLock for cfg, (2)
// p.healthMu.Lock to copy p.health, (3) p.quota.allSnapshots() takes its own
// RLock internally. Lock ordering is healthMu → quotaMu; acquiring them
// sequentially (not nested) keeps that order trivially correct.
func (w *webServer) handleStatus(resp http.ResponseWriter, r *http.Request) {
	w.p.mu.RLock()
	cfg := w.p.cfg
	w.p.mu.RUnlock()

	now := time.Now()
	w.p.healthMu.Lock()
	health := map[string]any{}
	for name, h := range w.p.health {
		state := "closed"
		switch {
		case now.Before(h.circuitOpenUntil):
			state = "open"
		case h.halfOpenInFlight:
			state = "half_open"
		}
		entry := map[string]any{
			"circuit_state": state,
			"available":     h.available(now),
		}
		if now.Before(h.circuitOpenUntil) {
			entry["circuit_until"] = h.circuitOpenUntil.UTC().Format(time.RFC3339)
		}
		if now.Before(h.rateLimitedUntil) {
			entry["rate_limited_until"] = h.rateLimitedUntil.UTC().Format(time.RFC3339)
		}
		health[name] = entry
	}
	w.p.healthMu.Unlock()

	// allSnapshots takes quotaMu.RLock internally and returns a fresh map; we
	// hand it out verbatim (the shape is provider-defined). nil → omit.
	var quota map[string]any
	if qs := w.p.quota.allSnapshots(); qs != nil {
		quota = make(map[string]any, len(qs))
		for k, v := range qs {
			quota[k] = v
		}
	}

	// scheduleStatus() returns []byte that is already a JSON object
	// {"models":…}. Embed it verbatim via json.RawMessage so writeJSON doesn't
	// double-encode it.
	writeJSON(resp, http.StatusOK, map[string]any{
		"uptime":   time.Since(w.p.metrics.startedAt()).String(),
		"version":  version,
		"listen":   cfg.Listen,
		"health":   health,
		"quota":    quota,
		"schedule": json.RawMessage(w.p.scheduleStatus()),
		"counters": w.p.metrics.snapshot(),
	})
}

// contentTypeFor maps an asset filename to its Content-Type.

// handleLogs returns the last N lines of the daemon's log file. tail defaults to
// 200 and is capped at 1000. The log path is set at runProxy time; if unset the
// handler falls back to cfg.LogFile, or 404 if neither is configured.
func (w *webServer) handleLogs(resp http.ResponseWriter, r *http.Request) {
	path := w.logFile
	if path == "" {
		path = w.p.cfg.LogFile
	}
	if path == "" {
		writeJSONErr(resp, http.StatusNotFound, "no log_file configured")
		return
	}
	n := 200
	if q := r.URL.Query().Get("tail"); q != "" {
		if v, err := strconv.Atoi(q); err == nil && v > 0 {
			n = v
		}
	}
	if n > 1000 {
		n = 1000
	}
	lines, err := tailFile(path, n)
	if err != nil {
		writeJSONErr(resp, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"lines": lines})
}

// tailFile returns the last n lines of path (fewer if the file is shorter).
func tailFile(path string, n int) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	all := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
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

// backupConfig copies path to bak (overwriting any prior backup). Best-effort:
// a backup failure is silent (the live config is the source of truth and can be
// reconstructed from the UI); if path does not yet exist nothing is written.
// (Named backupConfig to avoid a clash with takeover.go's backup.)
func backupConfig(path, bak string) {
	in, err := os.ReadFile(path)
	if err != nil {
		return // nothing to back up (first write)
	}
	os.WriteFile(bak, in, 0o644)
}

// atomicWrite writes data to path via a temp file + rename, mirroring savePool
// (pool.go) and persist (quota.go): Rename is a same-directory atomic move, so
// the on-disk file appears whole or not at all — a crash mid-write never leaves
// a truncated config. The temp file lives next to the target (MkdirAll on its
// dir) so rename stays within one directory.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// saveAndReload is the load-bearing config-mutation pipeline (Tasks 8–9 funnel
// structured edits through it too): validate bytes WITHOUT touching disk →
// back up the current file → write atomically → hot-reload the proxy. On
// validation failure nothing is written and no backup is created. On reload
// failure (defensive — should not happen post-validate) the backup is restored.
func (w *webServer) saveAndReload(data []byte) error {
	if _, err := LoadConfigFromBytes(w.configFile, data); err != nil {
		return err
	}
	bak := w.configFile + ".bak"
	backupConfig(w.configFile, bak)
	if err := atomicWrite(w.configFile, data); err != nil {
		return err
	}
	if err := w.p.reload(w.configFile); err != nil {
		// Defensive: reload shouldn't fail post-validate; restore from backup.
		if rb, rerr := os.ReadFile(bak); rerr == nil {
			atomicWrite(w.configFile, rb)
		}
		return err
	}
	return nil
}

// handleConfigGet returns the raw config YAML plus a small summary (listen
// address + provider/route counts). The YAML is returned verbatim so the UI's
// editor round-trips byte-identically with saveAndReload.
func (w *webServer) handleConfigGet(resp http.ResponseWriter, r *http.Request) {
	data, err := os.ReadFile(w.configFile)
	if err != nil {
		writeJSONErr(resp, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, err := LoadConfigFromBytes(w.configFile, data)
	if err != nil {
		writeJSONErr(resp, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{
		"yaml": string(data),
		"summary": map[string]any{
			"listen":         cfg.Listen,
			"provider_count": len(cfg.Providers),
			"route_count":    len(cfg.Routes),
		},
	})
}

// handleConfigPut accepts a {"yaml": "..."} body, validates it, and runs the
// saveAndReload pipeline. A validation/reload error yields 400 with the error
// message; success yields 200 {"status":"reloaded"}.
func (w *webServer) handleConfigPut(resp http.ResponseWriter, r *http.Request) {
	var req struct {
		YAML string `json:"yaml"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(resp, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if err := w.saveAndReload([]byte(req.YAML)); err != nil {
		writeJSONErr(resp, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(resp, http.StatusOK, map[string]string{"status": "reloaded"})
}
