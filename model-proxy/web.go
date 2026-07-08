package main

import (
	"bytes"
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

	"gopkg.in/yaml.v3"
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
	sessions   *loginSessionStore
}

// newWebServer builds a webServer bound to a proxy (for live state) and the
// on-disk config path (for validate-before-write + saveAndReload).
func newWebServer(p *Proxy, configFile string) *webServer {
	return &webServer{p: p, configFile: configFile, sessions: newLoginSessionStore()}
}

// webGC periodically drops stale login sessions. It runs as a goroutine
// started by runProxy; the ticker lives for the process lifetime.
func webGC(s *loginSessionStore) {
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		s.gc()
	}
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
	case path == "/api/config/edit" && r.Method == http.MethodPost:
		w.handleConfigEdit(resp, r)
	case path == "/api/accounts" && r.Method == http.MethodGet:
		w.handleAccountsList(resp, r)
	case strings.HasPrefix(path, "/api/accounts/") && r.Method == http.MethodPost:
		w.handleAccountAdd(resp, r)
	case strings.HasPrefix(path, "/api/accounts/") && r.Method == http.MethodDelete:
		w.handleAccountRemove(resp, r)
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

// handleAccountsList returns the per-provider account list with secrets
// stripped. By construction the response cannot leak a credential: the acct
// struct has NO field for api_key / access_key / secret_key / SSO cookie, so
// even a programming mistake in the builder can't serialize one. The account
// id is emitted UNMASKED — the UI needs the real id to remove an account
// (masking would break deletion). For aqp/codex (oauth_auth.json, AccountData
// shape) we surface email + account_id; for pooled apikey providers we surface
// {id, label, added_at} from the pool.
func (w *webServer) handleAccountsList(resp http.ResponseWriter, r *http.Request) {
	w.p.mu.RLock()
	providers := w.p.cfg.Providers
	w.p.mu.RUnlock()

	type acct struct {
		ID      string `json:"id"`
		Label   string `json:"label"`
		AddedAt string `json:"added_at"`
		Email   string `json:"email,omitempty"` // aqp/codex only
	}
	type prov struct {
		Name       string `json:"name"`
		ProviderID string `json:"provider_id"`
		Billing    string `json:"billing"`
		Accounts   []acct `json:"accounts"`
	}
	out := []prov{}
	for name, pcfg := range providers {
		p := prov{Name: name, ProviderID: pcfg.Provider, Billing: pcfg.Billing, Accounts: []acct{}}
		switch pcfg.Provider {
		case "aqp", "codex":
			// oauth_auth.json holds AccountData (email + account_id + SSO cookie).
			// Only email + account_id are surfaced — the SSO cookie is never copied
			// into the response struct. Guard against empty AccountID so a
			// differently-shaped codex file doesn't yield a bogus empty entry.
			a, _ := loadAccount(authFilePath(name, "oauth_auth"))
			if a != nil && a.AccountID != "" {
				p.Accounts = []acct{{
					ID:      a.AccountID,
					Label:   a.Email,
					AddedAt: time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339),
					Email:   a.Email,
				}}
			}
		default:
			// Plural pool (<name>_apikeys.json) or legacy singular (<name>_apikey.json).
			// Only id/label/added_at are copied — APIKey/AccessKey/SecretKey have no
			// field on `acct` and so cannot leak.
			pool, _ := loadPool(name, pcfg.Provider)
			for _, a := range pool.Accounts {
				p.Accounts = append(p.Accounts, acct{ID: a.ID, Label: a.Label, AddedAt: a.AddedAt})
			}
		}
		out = append(out, p)
	}
	writeJSON(resp, http.StatusOK, map[string]any{"providers": out})
}

// handleAccountAdd adds an account to a provider's credential pool. For apikey
// providers (zhipu/deepseek/volcengine) it runs the non-printing add core
// (validate-against-usage_url → dedup → save to pool file); for volcengine it
// uses addVolcengineAccount (AK/SK triple, no usage_url probe). For aqp/codex it
// returns 400 pointing at the async login flow (POST /api/login/<n>/start —
// Tasks 14/15): those providers use SSO/OAuth and cannot be added by a bare API
// key POST. After a successful save it triggers a best-effort reload so the new
// virtual provider is picked up; the reload error is ignored because the
// account was already persisted to the pool file (a later reload/next request
// will see it).
//
// The cfg passed to the cores is a snapshot copy taken under RLock — the cores
// never hold p.mu during their network validation call (usage_url probe), so a
// concurrent request isn't blocked on a 15s upstream timeout.
func (w *webServer) handleAccountAdd(resp http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	w.p.mu.RLock()
	prov, ok := w.p.cfg.Providers[name]
	w.p.mu.RUnlock()
	if !ok {
		writeJSONErr(resp, http.StatusNotFound, "unknown provider: "+name)
		return
	}
	switch prov.Provider {
	case "aqp", "codex":
		writeJSONErr(resp, http.StatusBadRequest,
			name+" uses the async login flow: POST /api/login/"+name+"/start")
		return
	}
	var req struct {
		APIKey    string `json:"api_key"`
		AccessKey string `json:"access_key"`
		SecretKey string `json:"secret_key"`
		Label     string `json:"label"`
		Replace   bool   `json:"replace"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(resp, http.StatusBadRequest, err.Error())
		return
	}
	cred := accountCred{APIKey: req.APIKey, AccessKey: req.AccessKey, SecretKey: req.SecretKey}
	cfg := w.p.snapshotConfig()
	var (
		id  string
		err error
	)
	if prov.Provider == "volcengine" {
		id, err = addVolcengineAccount(cfg, name, prov, cred, req.Label, req.Replace)
	} else {
		id, err = addApikeyAccount(cfg, name, prov, cred, req.Label, req.Replace)
	}
	if err != nil {
		writeJSONErr(resp, http.StatusBadRequest, err.Error())
		return
	}
	// Best-effort: if reload fails the account is still saved to the pool file;
	// the next reload/request will pick it up. Surface success regardless.
	_ = w.p.reload(w.configFile)
	writeJSON(resp, http.StatusOK, map[string]string{"id": id, "status": "added"})
}

// handleAccountRemove removes an account. For apikey providers it delegates to
// removeApikeyAccount (pool file rewrite); for aqp it calls clearAccount on the
// oauth_auth file (logout semantics — aqp is single-credential); for codex it
// os.Removes the oauth_auth file (codex is single-credential too — using Remove
// directly because clearAccount's not-exist tolerance is equivalent here, but
// the explicit Remove mirrors the codex logout path). All paths trigger a
// best-effort reload so the removed virtual provider is dropped from routing.
//
// Path shape: /api/accounts/<provider>/<id>. A missing id segment yields 400
// (not a 405/panic); an unknown provider yields 404.
func (w *webServer) handleAccountRemove(resp http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[1] == "" {
		writeJSONErr(resp, http.StatusBadRequest, "expected /api/accounts/<provider>/<id>")
		return
	}
	name, id := parts[0], parts[1]
	w.p.mu.RLock()
	prov, ok := w.p.cfg.Providers[name]
	w.p.mu.RUnlock()
	if !ok {
		writeJSONErr(resp, http.StatusNotFound, "unknown provider: "+name)
		return
	}
	switch prov.Provider {
	case "aqp":
		if err := clearAccount(authFilePath(name, "oauth_auth")); err != nil {
			writeJSONErr(resp, http.StatusInternalServerError, err.Error())
			return
		}
	case "codex":
		// codex is single-credential; remove the auth file outright. os.Remove
		// returns an error if the file is already gone — treat that as success
		// (idempotent remove, matching aqp's clearAccount not-exist tolerance).
		if err := os.Remove(authFilePath(name, "oauth_auth")); err != nil && !os.IsNotExist(err) {
			writeJSONErr(resp, http.StatusInternalServerError, err.Error())
			return
		}
	default:
		if err := removeApikeyAccount(name, prov.Provider, id); err != nil {
			writeJSONErr(resp, http.StatusBadRequest, err.Error())
			return
		}
	}
	_ = w.p.reload(w.configFile)
	writeJSON(resp, http.StatusOK, map[string]string{"status": "removed"})
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

// --- Structured (form-based) config edits (Task 8: general + scheduling) ---
//
// Structured edits mutate the config's yaml.Node tree (not a decoded struct) so
// comments and key ordering survive a round-trip — a struct decode→re-encode
// would strip every comment in the file. Each editor applies a targeted change
// to the node tree, re-encodes with yaml.NewEncoder (SetIndent 2), and funnels
// the bytes through saveAndReload (validate→backup→atomicWrite→reload).
// provider/route/claude_mapping kinds land in Task 9 (editStructured); until
// then they return 400 so this file compiles standalone.

// loadConfigNode parses the file into a yaml.Node root (preserving
// comments/order). The root is a document node whose Content[0] is the
// top-level mapping.
func loadConfigNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

// mapNode returns the mapping node at root.Content[0] (the top-level document
// mapping). Returns nil if the document is empty or not a mapping.
func mapNode(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	return root.Content[0]
}

// scalarNode builds a plain string scalar node.
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// setScalar sets a top-level key -> scalar value (creating the key if absent).
// On update only Value is touched: yaml.v3 auto-detects the scalar tag (!int /
// !!str / !!bool) on unmarshal, so re-tagging would corrupt typed fields (a
// !!str "5" fails to decode into an int). New keys default to !!str.
func setScalar(root *yaml.Node, key, value string) {
	m := mapNode(root)
	if m == nil {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1].Value = value
			return
		}
	}
	m.Content = append(m.Content, scalarNode(key), scalarNode(value))
}

// childMap returns the mapping node under key, creating it (as an empty
// mapping) at the end of the parent mapping if absent. Accepts either the
// document root (whose Content[0] is the top-level mapping) or a mapping node
// directly, so it composes for nested access: childMap(childMap(root, "a"), "b").
func childMap(root *yaml.Node, key string) *yaml.Node {
	m := root
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		m = root.Content[0]
	}
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key && m.Content[i+1].Kind == yaml.MappingNode {
			return m.Content[i+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content, scalarNode(key), child)
	return child
}

// setChildScalar sets key -> value under parentMap (creating the key if
// absent). Like setScalar, it does not re-tag an existing node (yaml.v3's
// auto-detected tag round-trips correctly for both int and string fields).
func setChildScalar(parent *yaml.Node, key, value string) {
	if parent == nil {
		return
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1].Value = value
			return
		}
	}
	parent.Content = append(parent.Content, scalarNode(key), scalarNode(value))
}

// editConfigNode loads the config node tree, applies mutate to the root, then
// re-encodes (SetIndent 2) and runs saveAndReload. Structured edits preserve
// comments/order via the Node API; the whole mutation funnels through the same
// validate→backup→write→reload pipeline as the raw YAML editor.
func (w *webServer) editConfigNode(mutate func(root *yaml.Node)) error {
	root, err := loadConfigNode(w.configFile)
	if err != nil {
		return err
	}
	if mapNode(root) == nil {
		return fmt.Errorf("config is not a YAML mapping")
	}
	mutate(root)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(root); err != nil {
		return err
	}
	enc.Close()
	return w.saveAndReload(buf.Bytes())
}

// handleConfigEdit dispatches a structured edit by kind. All five kinds are
// handled: general + scheduling apply scalar patches; provider/route/claude_mapping
// (Task 9) flow through editStructured for nested CRUD. Unknown kinds return 400.
func (w *webServer) handleConfigEdit(resp http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind string         `json:"kind"`
		Name string         `json:"name"`
		Data map[string]any `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONErr(resp, http.StatusBadRequest, err.Error())
		return
	}
	var err error
	switch req.Kind {
	case "general":
		err = w.editGeneral(req.Data)
	case "scheduling":
		err = w.editScheduling(req.Data)
	case "provider", "route", "claude_mapping":
		// Task 9: structured CRUD via editStructured.
		err = w.editStructured(req.Kind, req.Name, req.Data)
	default:
		writeJSONErr(resp, http.StatusBadRequest, "unknown edit kind: "+req.Kind)
		return
	}
	if err != nil {
		writeJSONErr(resp, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(resp, http.StatusOK, map[string]string{"status": "reloaded"})
}

// editGeneral applies scalar general-settings edits (listen, log_level,
// log_file) to the top-level mapping.
func (w *webServer) editGeneral(d map[string]any) error {
	return w.editConfigNode(func(root *yaml.Node) {
		for _, k := range []string{"listen", "log_level", "log_file"} {
			if v, ok := d[k]; ok {
				setScalar(root, k, fmt.Sprint(v))
			}
		}
	})
}

// editScheduling applies scalar scheduling edits under the `scheduling` block,
// creating it if absent.
func (w *webServer) editScheduling(d map[string]any) error {
	return w.editConfigNode(func(root *yaml.Node) {
		s := childMap(root, "scheduling")
		for _, k := range []string{"circuit_threshold", "circuit_cooldown", "rate_limit_backoff", "upstream_timeout", "sticky_dwell", "quota_poll_interval", "quota_switch_margin"} {
			if v, ok := d[k]; ok {
				setChildScalar(s, k, fmt.Sprint(v))
			}
		}
	})
}

// --- Structured edits for provider / route / claude_mapping (Task 9) ---
//
// These kinds mutate nested mappings or graft sequence nodes (route targets,
// model lists). Like the general/scheduling editors they funnel through
// editConfigNode → saveAndReload so comments/order survive and the same
// validate→backup→write→reload pipeline runs.
//
// Provider scalars (base URLs, usage_url, billing) set fields under
// `providers.<name>`. Route targets replace the whole `routes.<name>` sequence
// (encoded via mustEncode so the client sends a plain JSON array). Claude
// mapping add sets `claude_mapping.<alias> → <route>`; delete removes the alias.

// editStructured applies a structured edit for the given kind. Provider scalar
// fields + billing live under providers.<name>; route targets replace the
// routes.<name> sequence; claude_mapping add/delete works on the alias. A
// `{"delete": true}` payload removes the named entity (provider, route, or —
// keyed by data.alias — claude_mapping alias).
func (w *webServer) editStructured(kind, name string, d map[string]any) error {
	if d["delete"] == true {
		return w.editConfigNode(func(root *yaml.Node) {
			switch kind {
			case "provider":
				deleteKey(childMap(root, "providers"), name)
			case "route":
				deleteKey(childMap(root, "routes"), name)
			case "claude_mapping":
				if alias, ok := d["alias"].(string); ok {
					deleteKey(childMap(root, "claude_mapping"), alias)
				}
			}
		})
	}
	return w.editConfigNode(func(root *yaml.Node) {
		switch kind {
		case "provider":
			p := childMap(childMap(root, "providers"), name)
			for _, k := range []string{"openai_base_url", "anthropic_base_url", "usage_url", "billing"} {
				if v, ok := d[k]; ok {
					setChildScalar(p, k, fmt.Sprint(v))
				}
			}
		case "route":
			if raw, ok := d["targets"]; ok {
				setChildNode(childMap(root, "routes"), name, mustEncode(raw))
			}
		case "claude_mapping":
			if alias, ok := d["alias"].(string); ok {
				if rt, ok := d["route"].(string); ok {
					setChildScalar(childMap(root, "claude_mapping"), alias, rt)
				}
			}
		}
	})
}

// deleteKey removes a key (and its value) from a mapping node in place. It
// rewrites m.Content with a compacting pass: pairs whose key matches `key` are
// dropped, all others are preserved in order. The m.Content[:0] alias is safe
// because we read m.Content[i] and m.Content[i+1] before overwriting them via
// `out` (which shares the backing array but always lags behind i).
func deleteKey(m *yaml.Node, key string) {
	if m == nil {
		return
	}
	out := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			continue // drop pair
		}
		out = append(out, m.Content[i], m.Content[i+1])
	}
	m.Content = out
}

// setChildNode sets key → valueNode under parent (replacing an existing value
// node in place, or appending a new key+value pair when absent). Used for
// sequence grafts (route targets) where the value is a pre-built yaml.Node
// rather than a scalar.
func setChildNode(parent *yaml.Node, key string, valueNode *yaml.Node) {
	if parent == nil {
		return
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = valueNode
			return
		}
	}
	parent.Content = append(parent.Content, scalarNode(key), valueNode)
}

// mustEncode marshals a Go value (typically decoded from JSON, e.g.
// []any of map[string]any for route targets) to a yaml.Node and returns the
// inner content node (the document wrapper's single child). This is used to
// graft sequence/mapping values into the config tree without hand-rolling
// node construction. Errors are ignored: yaml.Marshal cannot fail for the
// basic types JSON decoding produces (string/bool/float64/[]any/map[string]any).
// On an empty result (a nil input), returns a !!null scalar so the key still
// writes something coherent.
func mustEncode(v any) *yaml.Node {
	b, _ := yaml.Marshal(v)
	var n yaml.Node
	yaml.Unmarshal(b, &n)
	if len(n.Content) > 0 {
		return n.Content[0]
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
}
