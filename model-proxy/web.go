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
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"model-proxy/provider"
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
	// newAqpClientFn builds the AQP client used by the async login flow. In
	// production this is newAqpClient (base = provider.AqpBase); tests override it with
	// newAqpClientWithBase to point at an httptest mock of the compass backend.
	newAqpClientFn func(storePath string) *AqpClient
	// newCodexOptions builds the codexLoginServerOptions used by the async
	// codex device-flow login. In production this returns the real OpenAI
	// deviceauth endpoints (via defaults()); tests override it to point opts at
	// an httptest mock of the 3 endpoints so requestUserCode / pollForToken /
	// exchangeCodeForTokens are fully exercised end-to-end.
	newCodexOptions func() *codexLoginServerOptions
}

// newWebServer builds a webServer bound to a proxy (for live state) and the
// on-disk config path (for validate-before-write + saveAndReload).
func newWebServer(p *Proxy, configFile string) *webServer {
	w := &webServer{
		p:              p,
		configFile:     configFile,
		sessions:       newLoginSessionStore(),
		newAqpClientFn: newAqpClient,
	}
	w.newCodexOptions = func() *codexLoginServerOptions {
		o := &codexLoginServerOptions{}
		o.defaults()
		return o
	}
	return w
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
	case path == "/api/requests" && r.Method == http.MethodGet:
		w.handleRequestsList(resp, r)
	case strings.HasPrefix(path, "/api/requests/") && r.Method == http.MethodGet:
		w.handleRequestDetail(resp, r)
	case path == "/api/config" && r.Method == http.MethodGet:
		w.handleConfigGet(resp, r)
	case path == "/api/config" && r.Method == http.MethodPost:
		w.handleConfigPut(resp, r)
	case path == "/api/config/edit" && r.Method == http.MethodPost:
		w.handleConfigEdit(resp, r)
	case path == "/api/accounts" && r.Method == http.MethodGet:
		w.handleAccountsList(resp, r)
	case path == "/api/tokens" && r.Method == http.MethodGet:
		w.handleTokens(resp, r)
	case path == "/api/tokens/reset" && r.Method == http.MethodPost:
		w.handleTokensReset(resp, r)
	case path == "/api/quota/refresh" && r.Method == http.MethodPost:
		w.handleQuotaRefresh(resp, r)
	case path == "/api/stats" && r.Method == http.MethodGet:
		w.handleStats(resp, r)
	case path == "/api/agents" && r.Method == http.MethodGet:
		w.handleAgents(resp, r)
	case path == "/api/pin" && r.Method == http.MethodGet:
		w.handlePinList(resp, r)
	case path == "/api/pin" && r.Method == http.MethodPost:
		w.handlePinSet(resp, r)
	case path == "/api/pin" && r.Method == http.MethodDelete:
		w.handlePinClear(resp, r)
	case path == "/api/analytics" && r.Method == http.MethodGet:
		w.handleAnalytics(resp, r)
	// The /test suffix must be matched BEFORE the bare /api/accounts/ POST
	// prefix below, which would otherwise swallow it as an account-add for a
	// provider named "<name>/<id>/test".
	case strings.HasPrefix(path, "/api/accounts/") && strings.HasSuffix(path, "/test") && r.Method == http.MethodPost:
		w.handleAccountTest(resp, r)
	case strings.HasPrefix(path, "/api/accounts/") && r.Method == http.MethodPost:
		w.handleAccountAdd(resp, r)
	case strings.HasPrefix(path, "/api/accounts/") && r.Method == http.MethodDelete:
		w.handleAccountRemove(resp, r)
	case strings.HasPrefix(path, "/api/login/") && r.Method == http.MethodPost:
		w.handleLoginStart(resp, r)
	case strings.HasPrefix(path, "/api/login/") && r.Method == http.MethodGet:
		w.handleLoginPoll(resp, r)
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
	routeWarnings := w.p.routeWarnings
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
		"counters": w.p.metrics.aggregateByProvider(),
		"warnings": routeWarnings,
	})
}

// contentTypeFor maps an asset filename to its Content-Type.

// handleLogs returns the last N lines of the daemon's log file. tail defaults to
// 200 and is capped at 1000. The log path is set at runProxy time; if unset the
// handler falls back to cfg.LogFile, or 404 if neither is configured.
func (w *webServer) handleLogs(resp http.ResponseWriter, r *http.Request) {
	path := w.logFile
	if path == "" {
		// Snapshot cfg under RLock — w.p.cfg is swapped by reload and reading
		// it unlocked would race (production always sets w.logFile so this
		// branch is rarely hit, but -race must stay clean).
		w.p.mu.RLock()
		path = w.p.cfg.LogFile
		w.p.mu.RUnlock()
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

// handleRequestsList returns request-log records (metadata only — no bodies) for
// the Requests UI tab, filtered by model/provider/status/time. Query params:
// model, provider (substring, case-insensitive), status (exact int), errors
// (any value → status>=400 only), from/to (unix or RFC3339), limit (default
// 100, capped at 1000). enabled=false in the response when request logging is
// off (the UI shows a hint instead of a table).
func (w *webServer) handleRequestsList(resp http.ResponseWriter, r *http.Request) {
	dir := w.p.reqLog.directory()
	if dir == "" {
		writeJSON(resp, http.StatusOK, map[string]any{"enabled": false, "records": []any{}})
		return
	}
	q := r.URL.Query()
	f := recordFilter{
		Model:      q.Get("model"),
		Provider:   q.Get("provider"),
		ErrorsOnly: q.Get("errors") != "",
		Limit:      100,
	}
	if v := q.Get("status"); v != "" {
		if s, err := strconv.Atoi(v); err == nil {
			f.Status = s
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			f.Limit = n
		}
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	if v := q.Get("from"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			f.From = time.Unix(t, 0)
		}
	}
	if v := q.Get("to"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			f.To = time.Unix(t, 0)
		}
	}
	recs, err := queryRequestRecords(dir, f)
	if err != nil {
		writeJSONErr(resp, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	summaries := make([]requestLogSummary, 0, len(recs))
	for _, rec := range recs {
		summaries = append(summaries, summarizeRecord(rec))
	}
	writeJSON(resp, http.StatusOK, map[string]any{"enabled": true, "records": summaries})
}

// handleRequestDetail returns the FULL record(s) for one request id, including
// request/response bodies (for the Requests UI detail drawer). Path:
// /api/requests/<id>. 404 when logging is off or the id matches no record.
func (w *webServer) handleRequestDetail(resp http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/requests/")
	id = strings.Trim(id, "/")
	dir := w.p.reqLog.directory()
	if dir == "" || id == "" {
		writeJSONErr(resp, http.StatusNotFound, "request logging is off or no id given")
		return
	}
	recs, err := queryRequestRecords(dir, recordFilter{RequestID: id, Limit: 50})
	if err != nil {
		writeJSONErr(resp, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	if len(recs) == 0 {
		writeJSONErr(resp, http.StatusNotFound, "no record for request id "+id)
		return
	}
	writeJSON(resp, http.StatusOK, map[string]any{"records": recs})
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
// (masking would break deletion). aqp (oauth_auth.json, AqpAccountData shape)
// surfaces email + account_id + created_at; codex (oauth_auth.json,
// CodexAuthFile shape - tokens.account_id + id_token JWT, NO top-level email)
// surfaces account_id + email parsed from the id_token; pooled apikey
// providers surface {id, label, added_at} from the pool.
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
		case "aqp":
			// oauth_auth.json holds AqpAccountData (email + account_id + SSO
			// cookie). Only email + account_id + created_at are surfaced - the
			// SSO cookie is never copied into the response struct.
			a, _ := provider.LoadAqpAccount(authFilePath(name, "oauth_auth"))
			if a != nil && a.AccountID != "" {
				p.Accounts = []acct{{
					ID:      a.AccountID,
					Label:   a.Email,
					AddedAt: time.Unix(a.CreatedAt, 0).UTC().Format(time.RFC3339),
					Email:   a.Email,
				}}
			}
		case "codex":
			// codex_oauth_auth.json holds CodexAuthFile (tokens.account_id +
			// id_token JWT). account_id + email are parsed from the file
			// (email from the id_token's `email` claim); access/refresh/id
			// tokens have no field on `acct` and so cannot leak. Guard against
			// an empty AccountID (no stored field AND no parseable id_token)
			// so a degenerate file doesn't yield a bogus empty entry.
			c, _ := provider.LoadCodexAccount(authFilePath(name, "oauth_auth"))
			if c != nil && c.AccountID != "" {
				p.Accounts = []acct{{
					ID:    c.AccountID,
					Label: c.Email,
					Email: c.Email,
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

// handleTokens returns the per-(provider, model) token-usage snapshot accrued
// from observed SSE streams. Nil-guarded so a degenerate Proxy (no tokens) still
// answers with an empty list. The map[tokenKey]tokenUsage snapshot is flattened
// to a JSON-friendly slice (JSON object keys must be strings; tokenKey is a struct).
func (w *webServer) handleTokens(resp http.ResponseWriter, r *http.Request) {
	type entry struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		tokenUsage
	}
	out := []entry{}
	if w.p.tokens != nil {
		for k, u := range w.p.tokens.snapshot() {
			out = append(out, entry{Provider: k.Provider, Model: k.Model, tokenUsage: u})
		}
	}
	writeJSON(resp, http.StatusOK, map[string]any{"usage": out})
}

// handleTokensReset zeroes all call-statistics state: the in-memory metrics +
// token counters, the persisted SQLite bucket history, and the flusher baseline
// (so the next flush sees zero delta). The next persist tick overwrites the DB
// with the empty snapshot.
func (w *webServer) handleTokensReset(resp http.ResponseWriter, r *http.Request) {
	w.p.resetStats()
	writeJSON(resp, http.StatusOK, map[string]string{"status": "reset"})
}

// handleQuotaRefresh triggers an immediate quota re-poll so the Web UI's Usage
// sections can be refreshed on demand instead of waiting for the next poll
// interval. With an empty body it polls every provider (pollAll); with a
// {"provider":"<key>"} body it polls just that one account's provider key
// (pollOne) - the key is a config name or a pooled-account virtual id
// "name#<accountID>" (mirrors accountProviderKey / the quota snapshot map). The
// poll runs synchronously - fetchQuota blocks until Quota() returns (including
// transient-error retries) and persists - so the caller can re-fetch
// /api/status right after and see the fresh snapshot. A nil quota tracker
// (degenerate test Proxy) is a no-op 200. An unknown provider key is a 404.
func (w *webServer) handleQuotaRefresh(resp http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider string `json:"provider,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // empty body is valid -> refresh all
	if w.p.quota == nil {
		writeJSON(resp, http.StatusOK, map[string]string{"status": "refreshed"})
		return
	}
	if req.Provider != "" {
		if !w.p.quota.pollOne(req.Provider) {
			writeJSONErr(resp, http.StatusNotFound, "unknown provider: "+req.Provider)
			return
		}
		writeJSON(resp, http.StatusOK, map[string]string{"status": "refreshed", "provider": req.Provider})
		return
	}
	w.p.quota.pollAll(time.Now())
	writeJSON(resp, http.StatusOK, map[string]string{"status": "refreshed"})
}

// handleStats returns per-(provider, model) bucket rows from the SQLite store
// over a time range, for the `stats` CLI / time-series queries. Query params:
// from, to (unix seconds or RFC3339; default last 60 minutes), optional
// provider/model filters, and bucket (display granularity, e.g. "10m"/"1h";
// default "1m" = raw 1-minute rows; storage is always 1-minute, so widening the
// bucket only reduces returned rows via SQL aggregation). Nil-safe: a Proxy
// without a stats store (tests) answers with an empty list.
func (w *webServer) handleStats(resp http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from := now.Add(-time.Hour).Unix()
	to := now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			to = t
		}
	}
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	bucketSecs := normalizeBucket(r.URL.Query().Get("bucket"))
	buckets := []statsBucket{}
	if w.p.stats != nil {
		got, err := w.p.stats.queryRange(from, to, provider, model, bucketSecs)
		if err != nil {
			writeJSONErr(resp, http.StatusInternalServerError, "stats query: "+err.Error())
			return
		}
		buckets = got
	}
	writeJSON(resp, http.StatusOK, map[string]any{
		"from":    from,
		"to":      to,
		"bucket":  bucketSecs,
		"buckets": buckets,
	})
}

// handleAgents returns agent-dimension buckets (which client made the request)
// over a time range, for the `stats --by-agent` CLI / Web UI "who is burning my
// quota" view. Query params mirror /api/stats: from, to (unix or RFC3339;
// default last 60 minutes), optional agent/provider/model filters, and bucket
// (display granularity; storage is always 1-minute). Nil-safe: no stats store
// (tests) → empty list.
func (w *webServer) handleAgents(resp http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from := now.Add(-time.Hour).Unix()
	to := now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			to = t
		}
	}
	agent := r.URL.Query().Get("agent")
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	bucketSecs := normalizeBucket(r.URL.Query().Get("bucket"))
	buckets := []agentBucket{}
	if w.p.stats != nil {
		got, err := w.p.stats.queryAgentRange(from, to, agent, provider, model, bucketSecs)
		if err != nil {
			writeJSONErr(resp, http.StatusInternalServerError, "agent stats query: "+err.Error())
			return
		}
		buckets = got
	}
	writeJSON(resp, http.StatusOK, map[string]any{
		"from":    from,
		"to":      to,
		"bucket":  bucketSecs,
		"buckets": buckets,
	})
}

// handlePinSet installs a manual route→provider pin (hot-switch). Body:
// {"route": "<exposed>", "provider": "<name>", "ttl_seconds": 0}. ttl_seconds
// <= 0 means no expiry. 200 {"route","provider","expires_at"} on success; 400 on
// a missing/unknown route or a provider the route can't reach (the pin would be
// a silent no-op, so reject it with a clear message).
func (w *webServer) handlePinSet(resp http.ResponseWriter, r *http.Request) {
	var body struct {
		Route      string `json:"route"`
		Provider   string `json:"provider"`
		TTLSeconds int64  `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONErr(resp, http.StatusBadRequest, "parse pin body: "+err.Error())
		return
	}
	if body.Route == "" || body.Provider == "" {
		writeJSONErr(resp, http.StatusBadRequest, "route and provider are required")
		return
	}
	ttl := time.Duration(0)
	if body.TTLSeconds > 0 {
		ttl = time.Duration(body.TTLSeconds) * time.Second
	}
	pe, ok := w.p.setPin(body.Route, body.Provider, ttl)
	if !ok {
		writeJSONErr(resp, http.StatusBadRequest, fmt.Sprintf(
			"cannot pin %q to %q: no such route, or the route has no target for that provider", body.Route, body.Provider))
		return
	}
	expires := ""
	if !pe.expiresAt.IsZero() {
		expires = pe.expiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(resp, http.StatusOK, map[string]any{
		"route":      body.Route,
		"provider":   body.Provider,
		"expires_at": expires,
		"status":     "pinned",
	})
}

// handlePinClear removes a pin. Query: ?route=<exposed>. 200 whether or not a pin
// existed; the response reports removed=true/false so the CLI can distinguish.
func (w *webServer) handlePinClear(resp http.ResponseWriter, r *http.Request) {
	route := r.URL.Query().Get("route")
	if route == "" {
		writeJSONErr(resp, http.StatusBadRequest, "route query param is required")
		return
	}
	removed := w.p.clearPin(route)
	writeJSON(resp, http.StatusOK, map[string]any{
		"route":   route,
		"removed": removed,
	})
}

// handlePinList lists active pins (route → {provider, expires_at}).
func (w *webServer) handlePinList(resp http.ResponseWriter, r *http.Request) {
	pins := w.p.listPins()
	out := make([]map[string]any, 0, len(pins))
	for route, pe := range pins {
		expires := ""
		if !pe.expiresAt.IsZero() {
			expires = pe.expiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{
			"route":      route,
			"provider":   pe.provider,
			"expires_at": expires,
		})
	}
	writeJSON(resp, http.StatusOK, map[string]any{"pins": out})
}

// handleAnalytics returns per-(provider, model) calendar day/month aggregates
// with server-computed equivalent-payg cost. Cost is price × tokens, never
// stored or fabricated; unknown prices yield cost=null + priced=false. Query
// params: from/to (unix or RFC3339; default last 30d), provider/model filters,
// granularity=day|month (default day). Nil-safe: no stats store → empty series.
func (w *webServer) handleAnalytics(resp http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from := now.Add(-30 * 24 * time.Hour).Unix()
	to := now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			from = t
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if t, ok := parseStatsTime(v); ok {
			to = t
		}
	}
	provider := r.URL.Query().Get("provider")
	model := r.URL.Query().Get("model")
	granularity := r.URL.Query().Get("granularity")
	if granularity == "" {
		granularity = "day"
	}
	if granularity != "day" && granularity != "month" {
		writeJSONErr(resp, http.StatusBadRequest, "granularity must be day or month")
		return
	}

	type point struct {
		Bucket        int64    `json:"bucket"`
		Requests      uint64   `json:"requests"`
		Input         uint64   `json:"input"`
		Output        uint64   `json:"output"`
		CacheCreation uint64   `json:"cache_creation"`
		CacheRead     uint64   `json:"cache_read"`
		Cost          *float64 `json:"cost"`
		Priced        bool     `json:"priced"`
	}
	type series struct {
		Provider string  `json:"provider"`
		Model    string  `json:"model"`
		Points   []point `json:"points"`
	}

	buckets := []analyticsBucket{}
	if w.p.stats != nil {
		got, err := w.p.stats.queryAnalytics(from, to, provider, model, granularity)
		if err != nil {
			writeJSONErr(resp, http.StatusInternalServerError, "analytics query: "+err.Error())
			return
		}
		buckets = got
	}
	cat := w.p.pricingSnapshot()
	prices := w.p.priceOverrides()

	byKey := map[string]*series{}
	var keys []string
	priced, unpriced := map[string]bool{}, map[string]bool{}
	var totInput, totOutput uint64
	var totCost *float64
	for _, b := range buckets {
		k := b.Provider + "\x00" + b.Model
		s, ok := byKey[k]
		if !ok {
			s = &series{Provider: b.Provider, Model: b.Model}
			byKey[k] = s
			keys = append(keys, k)
		}
		e, ok := resolvePrice(prices, cat, b.Model)
		var cost *float64
		if ok {
			cr := computeCost(b.Input, b.Output, b.CacheRead, b.CacheCreation, e)
			cost = &cr.Cost
			priced[b.Model] = true
			if totCost == nil {
				totCost = new(float64)
			}
			*totCost += cr.Cost
		} else {
			unpriced[b.Model] = true
		}
		s.Points = append(s.Points, point{
			Bucket: b.Bucket, Requests: b.Requests, Input: b.Input, Output: b.Output,
			CacheCreation: b.CacheCreation, CacheRead: b.CacheRead, Cost: cost, Priced: ok,
		})
		totInput += b.Input
		totOutput += b.Output
	}
	out := []series{}
	for _, k := range keys {
		out = append(out, *byKey[k])
	}
	coverage := map[string]any{"priced": mapKeys(priced), "unpriced": mapKeys(unpriced)}
	writeJSON(resp, http.StatusOK, map[string]any{
		"granularity":    granularity,
		"from":           from,
		"to":             to,
		"series":         out,
		"totals":         map[string]any{"input": totInput, "output": totOutput, "cost": totCost},
		"price_coverage": coverage,
	})
}

// mapKeys returns the sorted keys of a set map (stable JSON output).
func mapKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return []string{}
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseStatsTime parses a stats time param as unix seconds (integer) or RFC3339.
func parseStatsTime(v string) (int64, bool) {
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n, true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.Unix(), true
	}
	return 0, false
}

// handleAccountAdd adds an account to a provider's credential pool. For apikey
// providers it runs the non-printing add core (validate → dedup → save to pool
// file): addApikeyAccount for zhipu/deepseek/kimi-code (validate against
// usage_url), addVolcengineAccount for volcengine (validate the Ark API Key via
// usage_url, a Bearer GET to /models, and — when AK/SK are supplied — the signed
// GetAFPUsage). For aqp/codex it returns 400 pointing at the async login flow
// (POST /api/login/<n>/start — Tasks 14/15): those providers use SSO/OAuth and
// cannot be added by a bare API key POST. After a successful save it triggers a
// best-effort reload so the new virtual provider is picked up; the reload error
// is ignored because the account was already persisted to the pool file (a
// later reload/next request will see it).
//
// The cfg passed to the cores is a snapshot copy taken under RLock — the cores
// never hold p.mu during their network validation call (usage_url / GetAFPUsage
// probe), so a concurrent request isn't blocked on the upstream timeout.
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

// handleAccountTest runs a ONE-SHOT end-to-end probe through a single account:
// it sends a minimal real chat request to the provider's upstream with that
// account's credential (probeModelCallable — the same wiring `models refresh`
// uses) and reports the outcome. Read-only: no reload, no health/sticky/quota
// state is touched.
//
// Path shape: /api/accounts/<provider>/<id>/test. The id is the same account id
// GET /api/accounts surfaces (pool id for apikey providers, AccountID for
// aqp/codex); it selects the virtual provider key: "name#<id>" for a ≥2-account
// pool, the plain name otherwise (mirrors buildProviders). An unknown provider
// or an id matching no account is a 404; a provider with no probe-able model
// (no route targets it AND an empty models: list) is a 400. The probe outcome
// itself is always a 200 with {"status":"ok"|"failed", ...} — a failed probe is
// a valid answer, not a server error.
func (w *webServer) handleAccountTest(resp http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/accounts/")
	rest = strings.TrimSuffix(rest, "/test")
	parts := strings.SplitN(strings.Trim(rest, "/"), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeJSONErr(resp, http.StatusBadRequest, "expected /api/accounts/<provider>/<id>/test")
		return
	}
	name, id := parts[0], parts[1]
	cfg := w.p.snapshotConfig()
	prov, ok := cfg.Providers[name]
	if !ok {
		writeJSONErr(resp, http.StatusNotFound, "unknown provider: "+name)
		return
	}
	// Resolve the account id to the virtual provider key. The id sources mirror
	// handleAccountsList: aqp/codex read the oauth_auth file (single-credential,
	// plain-name key), apikey providers read the credential pool (≥2 accounts →
	// "name#<id>" virtual, 1 account → plain name).
	key := name
	switch prov.Provider {
	case "aqp":
		a, _ := provider.LoadAqpAccount(authFilePath(name, "oauth_auth"))
		if a == nil || a.AccountID != id {
			writeJSONErr(resp, http.StatusNotFound, "unknown account: "+id)
			return
		}
	case "codex":
		c, _ := provider.LoadCodexAccount(authFilePath(name, "oauth_auth"))
		if c == nil || c.AccountID != id {
			writeJSONErr(resp, http.StatusNotFound, "unknown account: "+id)
			return
		}
	default:
		pool, _ := loadPool(name, prov.Provider)
		found := false
		for _, a := range pool.Accounts {
			if a.ID == id {
				found = true
				break
			}
		}
		if !found {
			writeJSONErr(resp, http.StatusNotFound, "unknown account: "+id)
			return
		}
		if len(pool.Accounts) >= 2 {
			key = name + "#" + id
		}
	}
	// Pick a model to probe: prefer a model this provider serves in some route
	// (sorted for determinism), else its first config model.
	model := ""
	if ms := routeModelsForProvider(cfg, name); len(ms) > 0 {
		model = ms[0]
	} else if len(prov.Models) > 0 {
		model = prov.Models[0]
	}
	if model == "" {
		writeJSONErr(resp, http.StatusBadRequest, name+" has no model to probe (no route targets it and its models: list is empty)")
		return
	}
	w.p.mu.RLock()
	impl := w.p.providers[key]
	w.p.mu.RUnlock()
	if impl == nil {
		writeJSONErr(resp, http.StatusNotFound, "provider "+key+" not available (reload pending?)")
		return
	}
	// The probe blocks on the upstream (up to the scheduling timeout) — run it
	// WITHOUT holding p.mu so in-flight forwards aren't stalled.
	client := &http.Client{Timeout: cfg.Scheduling.timeout()}
	start := time.Now()
	ok, status, reason := probeModelCallable(client, prov, impl, model)
	out := map[string]any{
		"http_status": status,
		"latency_ms":  time.Since(start).Milliseconds(),
		"provider":    name,
		"account_id":  id,
		"model":       model,
	}
	if ok {
		out["status"] = "ok"
	} else {
		out["status"] = "failed"
		out["reason"] = reason
	}
	writeJSON(resp, http.StatusOK, out)
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
		if err := provider.ClearAqpAccount(authFilePath(name, "oauth_auth")); err != nil {
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

// --- Async login endpoints (Task 14: aqp; Task 15: codex) ---
//
// The async login flow is a 3-step dance over HTTP:
//  1. POST /api/login/<provider>/start — bootstraps the login (aqp: SSO URL +
//     cookie-jar client; codex: device-flow user code), stashes the in-flight
//     state in a session, and launches a goroutine that polls until the user
//     completes login. Returns {session_id, login_url} (aqp) or {session_id,
//     verify_url, user_code} (codex).
//  2. The user opens the login_url / verify_url in a browser and authenticates.
//  3. GET /api/login/<session>/poll — returns the session state ("pending" →
//     "done" / "error"). The polling goroutine resolves the session: on success
//     it persists the credential and hot-reloads.

// handleLoginStart dispatches an async login start by provider. aqp bootstraps
// the SSO URL + launches the poll goroutine; codex bootstraps the device-flow
// user code + launches the device-flow poll goroutine. The provider must exist
// in config (guards against typos). The switch is on the RESOLVED provider_id
// (prov.Provider), NOT the URL name: a config entry can be named anything
// (e.g. aqp-alt: {provider_id: aqp}), and we must dispatch aqp-alt → the aqp
// flow. The name is passed through to the start/poll functions so credentials
// are saved to the right file (<name>_oauth_auth.json).
func (w *webServer) handleLoginStart(resp http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/login/")
	name := strings.TrimSuffix(rest, "/start")
	w.p.mu.RLock()
	prov, ok := w.p.cfg.Providers[name]
	w.p.mu.RUnlock()
	if !ok {
		writeJSONErr(resp, http.StatusNotFound, "unknown provider: "+name)
		return
	}
	switch prov.Provider {
	case "aqp":
		w.startAqpLogin(resp, r, name)
	case "codex":
		w.startCodexLogin(resp, r, name)
	default:
		writeJSONErr(resp, http.StatusBadRequest, name+" has no async login flow")
	}
}

// startAqpLogin bootstraps the SSO login URL against the aqp backend, stashes
// the AqpClient (with its cookie jar) in a session, kicks the poll goroutine,
// and returns the session id + login URL. The goroutine resolves the session
// to "done" (mint+save+reload) or "error". name is the config key (not the
// provider_id) so credentials land in <name>_oauth_auth.json — matching what
// buildProviders/newAuthProvider reads.
func (w *webServer) startAqpLogin(resp http.ResponseWriter, r *http.Request, name string) {
	sess := w.sessions.create("aqp")
	sess.aqpClient = w.newAqpClientFn(authFilePath(name, "oauth_auth"))
	loginURL, err := sess.aqpClient.BootstrapLoginURL()
	if err != nil {
		sess.setState("error", err.Error())
		writeJSONErr(resp, http.StatusBadGateway, err.Error())
		return
	}
	// Stash the login_url as the session detail (re-surfaced by poll so the UI
	// can recover it). Written under the session mutex to stay race-clean with
	// the poll goroutine's setState calls.
	sess.mu.Lock()
	sess.detail = loginURL
	sess.mu.Unlock()
	go w.runAqpPoll(sess, name)
	writeJSON(resp, http.StatusOK, map[string]string{
		"session_id": sess.id,
		"login_url":  loginURL,
	})
}

// runAqpPoll is the goroutine that resolves an aqp login session: poll auth/info
// (the jar carries SSO_A → the 200 sets SSO_C) → fetchAPIKey (mints the managed
// key + identity) → saveAccount → hot-reload. Every failure path sets state to
// "error" so the poll endpoint surfaces it; there is no retry — the user starts
// a fresh session. name is the config key so saveAccount writes the right file.
func (w *webServer) runAqpPoll(sess *loginSession, name string) {
	if _, err := sess.aqpClient.PollSession(3 * time.Minute); err != nil {
		sess.setState("error", err.Error())
		return
	}
	keyData, err := sess.aqpClient.fetchAPIKey()
	if err != nil {
		sess.setState("error", "api key provisioning: "+err.Error())
		return
	}
	a := &provider.AqpAccountData{
		AccountID:        keyData.EmployeeEmail,
		Email:            keyData.EmployeeEmail,
		ProjectID:        keyData.ProjectID,
		SSOSessionCookie: sess.aqpClient.SessionCookie(),
		LastRefreshAt:    time.Now().Unix(),
	}
	if err := provider.SaveAqpAccount(authFilePath(name, "oauth_auth"), a); err != nil {
		sess.setState("error", err.Error())
		return
	}
	_ = w.p.reload(w.configFile) // best-effort: account is already persisted
	sess.setState("done", a.Email)
}

// startCodexLogin bootstraps the OAuth device flow: requestUserCode → stash the
// device_auth_id + user_code + poll interval + options in a session → kick the
// poll goroutine → return {session_id, verify_url, user_code}. The goroutine
// resolves the session to "done" (exchange+save+reload) or "error". name is the
// config key so the auth file is written to <name>_oauth_auth.json.
func (w *webServer) startCodexLogin(resp http.ResponseWriter, r *http.Request, name string) {
	opts := w.newCodexOptions()
	uc, err := requestUserCode(opts, provider.CodexOAuthClientID)
	if err != nil {
		writeJSONErr(resp, http.StatusBadGateway, err.Error())
		return
	}
	sess := w.sessions.create("codex")
	interval, _ := strconv.Atoi(uc.Interval)
	sess.codex = &codexLoginState{
		deviceAuthID: uc.DeviceAuthID,
		userCode:     uc.UserCode,
		interval:     interval,
		opts:         opts,
	}
	// Stash verify_url+user_code as the session detail (re-surfaced by poll so
	// the UI can recover them). Written under the session mutex to stay
	// race-clean with the poll goroutine's setState calls.
	sess.mu.Lock()
	sess.detail = codexOAuthVerifyURL + "  code: " + uc.UserCode
	sess.mu.Unlock()
	go w.runCodexPoll(sess, name)
	writeJSON(resp, http.StatusOK, map[string]string{
		"session_id": sess.id,
		"verify_url": codexOAuthVerifyURL,
		"user_code":  uc.UserCode,
	})
}

// runCodexPoll is the goroutine that resolves a codex device-flow session:
// pollForToken (the user authorizes in the browser) → exchangeCodeForTokens
// (trade the auth code for access/refresh/id tokens) → write the codex auth
// file 0600 → hot-reload. Every failure path sets state to "error" so the poll
// endpoint surfaces it; there is no retry — the user starts a fresh session.
// name is the config key so the auth file is written to <name>_oauth_auth.json.
func (w *webServer) runCodexPoll(sess *loginSession, name string) {
	cs := sess.codex
	authCode, err := pollForToken(cs.opts, cs.deviceAuthID, cs.userCode, cs.interval)
	if err != nil {
		sess.setState("error", err.Error())
		return
	}
	af, err := exchangeCodeForTokens(cs.opts, provider.CodexOAuthClientID, authCode.AuthorizationCode, authCode.CodeVerifier)
	if err != nil {
		sess.setState("error", err.Error())
		return
	}
	path := authFilePath(name, "oauth_auth")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		sess.setState("error", err.Error())
		return
	}
	b, _ := json.MarshalIndent(af, "", "  ")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		sess.setState("error", err.Error())
		return
	}
	_ = w.p.reload(w.configFile) // best-effort: tokens are already persisted
	sess.setState("done", af.Tokens.AccountID)
}

// handleLoginPoll returns the current state of an async login session
// ("pending" / "done" / "error") plus the detail (login_url or error msg) and
// result (email on success). A missing session yields 404 (expired or unknown).
func (w *webServer) handleLoginPoll(resp http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/login/")
	id := strings.TrimSuffix(rest, "/poll")
	sess, ok := w.sessions.get(id)
	if !ok {
		writeJSONErr(resp, http.StatusNotFound, "unknown or expired session")
		return
	}
	sess.mu.Lock()
	state, detail, result := sess.state, sess.detail, sess.result
	sess.mu.Unlock()
	writeJSON(resp, http.StatusOK, map[string]string{
		"state":  state,
		"detail": detail,
		"result": result,
	})
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

// backupConfig copies path to bak (overwriting any prior backup at that exact
// path). Best-effort: a backup failure is silent (the live config is the source
// of truth and can be reconstructed from the UI); if path does not yet exist
// nothing is written. The bak directory (e.g. `back/`) is created on demand.
// (Named backupConfig to avoid a clash with takeover.go's backup.)
func backupConfig(path, bak string) {
	in, err := os.ReadFile(path)
	if err != nil {
		return // nothing to back up (first write)
	}
	os.MkdirAll(filepath.Dir(bak), 0o755)
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

// writeConfigValidated is the shared validate->backup->atomic-write tail used by
// both the web UI's saveAndReload (which adds a reload) and the CLI's
// models-refresh writeProviderModels (which reloads separately). On validation
// failure nothing is written and no backup is created. Returns the backup path
// so callers that reload can restore on reload failure.
//
// The backup is written to a `back/` directory (sibling of the config file) with
// a timestamped name (`config.yaml.20060102-150405.bak`), so each write keeps its
// own backup instead of overwriting the previous one. `back/` is created on
// demand (best-effort, like the backup itself).
func writeConfigValidated(configFile, data string) (bak string, err error) {
	if _, err := LoadConfigFromBytes(configFile, []byte(data)); err != nil {
		return "", err
	}
	bak = backupConfigPath(configFile)
	backupConfig(configFile, bak)
	if err := atomicWrite(configFile, []byte(data)); err != nil {
		return bak, err
	}
	return bak, nil
}

// backupConfigPath returns the timestamped backup path for a config file: a
// `back/` directory sibling of `configFile`, with a name of
// `<base>.<YYYYMMDD-HHMMSS>.bak`. Each call (within the same second) yields a
// distinct path only if the second differs; two writes in the same second
// collide on the same name (the later overwrites the earlier) - acceptable
// since sub-second backup granularity is not meaningful.
func backupConfigPath(configFile string) string {
	dir := filepath.Dir(configFile)
	base := filepath.Base(configFile)
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(dir, "back", base+"."+stamp+".bak")
}

// saveAndReload is the load-bearing config-mutation pipeline (Tasks 8–9 funnel
// structured edits through it too): validate bytes WITHOUT touching disk →
// back up the current file → write atomically → hot-reload the proxy. On
// validation failure nothing is written and no backup is created. On reload
// failure (defensive — should not happen post-validate) the backup is restored.
func (w *webServer) saveAndReload(data []byte) error {
	bak, err := writeConfigValidated(w.configFile, string(data))
	if err != nil {
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
	// provider_models: name -> models list, so the Provider form can prefill +
	// edit each provider's models sequence (the raw yaml round-trip is the only
	// other place models are visible). Built from the already-parsed cfg.
	provModels := make(map[string][]string, len(cfg.Providers))
	for name, p := range cfg.Providers {
		provModels[name] = p.Models
	}
	// routes: exposed model -> targets, so the Routes form can prefill + edit each
	// route's target list as structured rows (provider/model/priority). Mirrors
	// RouteTarget; priority is always emitted (0 when unset).
	type routeTargetOut struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Priority int    `json:"priority"`
	}
	routesOut := make(map[string][]routeTargetOut, len(cfg.Routes))
	for exposed, targets := range cfg.Routes {
		row := make([]routeTargetOut, 0, len(targets))
		for _, t := range targets {
			row = append(row, routeTargetOut{Provider: t.Provider, Model: t.Model, Priority: t.Priority})
		}
		routesOut[exposed] = row
	}
	writeJSON(resp, http.StatusOK, map[string]any{
		"yaml": string(data),
		"summary": map[string]any{
			"listen":         cfg.Listen,
			"provider_count": len(cfg.Providers),
			"route_count":    len(cfg.Routes),
		},
		"provider_models": provModels,
		"routes":          routesOut,
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

// scalarNode builds a plain scalar node. Tag is left empty so yaml.v3 infers
// the type per-value when encoding: "5" → int, "plan" → string,
// "127.0.0.1:18000" → string. This matters for NEW keys: a newly-added int key
// (e.g. circuit_threshold on a config with no scheduling block) must NOT carry
// an explicit !!str tag, or reload fails ("cannot unmarshal !!str 5 into int").
// For existing-key edits only .Value is touched (setScalar/setChildScalar),
// preserving the node's original tag — so this default only affects new keys.
func scalarNode(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: v}
}

// setScalar sets a top-level key -> scalar value (creating the key if absent).
// On update only Value is touched: yaml.v3 auto-detects the scalar tag (!int /
// !!str / !!bool) on unmarshal, so re-tagging would corrupt typed fields (a
// !!str "5" fails to decode into an int). New keys use scalarNode whose Tag is
// empty, so yaml.v3 infers the type per-value at encode time.
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
			// provider_id first so a freshly-added provider block validates
			// (config.validate requires it) - without it the Web UI's "add
			// provider" flow always 400s with "provider_id is empty".
			for _, k := range []string{"provider_id", "openai_base_url", "anthropic_base_url", "usage_url", "billing"} {
				if v, ok := d[k]; ok {
					setChildScalar(p, k, fmt.Sprint(v))
				}
			}
			// models is a sequence (list of real model names), not a scalar.
			// The client sends it as a JSON array; graft it via mustEncode so
			// comments/order elsewhere survive (same path as route targets).
			if v, ok := d["models"]; ok {
				setChildNode(p, "models", mustEncode(v))
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
