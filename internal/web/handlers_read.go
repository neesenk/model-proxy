package web

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/appapi"

	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	v := s.reads.Dashboard(time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"uptime": v.Uptime, "version": s.version, "listen": v.Listen, "health": v.Health, "model_locks": v.ModelLocks, "quota": v.Quota, "schedule": v.Schedule, "counters": v.Counters, "cache": v.Cache, "warnings": v.Warnings, "credential_store": v.CredentialStore})
}

// handleSessions serves GET /api/sessions: per-session aggregates (span,
// requests/errors, providers/models, token totals, equivalent USD cost) from
// the request log's newest records. Requires request_log to be enabled.
func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	dir := s.reads.RequestLogDirectory()
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "sessions": []any{}})
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}
	// Same cost path as analytics and the budget watcher: config overrides
	// first, then the cached catalog; unpriced models contribute nothing.
	snapshot := s.reads.Pricing()
	costOf := func(model string, usage requestlog.Usage) float64 {
		entry, ok := pricing.Resolve(snapshot.Overrides, snapshot.Catalog, model)
		if !ok {
			return 0
		}
		return pricing.ComputeCost(usage.Input, usage.Output, usage.CacheRead, usage.CacheCreation, entry)
	}
	sessions, err := requestlog.SessionSummaries(dir, 2000, limit, costOf)
	if err != nil {
		// The underlying error may embed local paths; log it server-side and
		// return only a generic message to the client.
		log.Printf("web: session summary failed: %v", err)
		writeJSONErr(w, http.StatusInternalServerError, "failed to list sessions")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "sessions": sessions})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	p := ""
	if s.logFile != nil {
		p = s.logFile()
	}
	if p == "" {
		p = s.reads.LogFile()
	}
	if p == "" {
		writeJSONErr(w, http.StatusNotFound, "no log_file configured")
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
	lines, err := tailFile(p, n)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"lines": lines})
}

func (s *Server) handleRequestsList(w http.ResponseWriter, r *http.Request) {
	dir := s.reads.RequestLogDirectory()
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "records": []any{}})
		return
	}
	q := r.URL.Query()
	f := requestlog.Filter{Model: q.Get("model"), Provider: q.Get("provider"), ErrorsOnly: q.Get("errors") != "", Limit: 100}
	if v := q.Get("shadow"); v == "only" || v == "exclude" {
		f.Shadow = v
	}
	if v := q.Get("status"); v != "" {
		if n, e := strconv.Atoi(v); e == nil {
			f.Status = n
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, e := strconv.Atoi(v); e == nil && n > 0 {
			f.Limit = n
		}
	}
	if f.Limit > 1000 {
		f.Limit = 1000
	}
	if v := q.Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			f.From = time.Unix(n, 0)
		}
	}
	if v := q.Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			f.To = time.Unix(n, 0)
		}
	}
	records, err := requestlog.QuerySummaries(dir, f)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "records": records})
}

func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/requests/"), "/")
	dir := s.reads.RequestLogDirectory()
	if dir == "" || id == "" {
		writeJSONErr(w, http.StatusNotFound, "request logging is off or no id given")
		return
	}
	records, err := requestlog.QueryRecords(dir, requestlog.Filter{RequestID: id, Limit: 50})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	if len(records) == 0 {
		writeJSONErr(w, http.StatusNotFound, "no record for request id "+id)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records})
}

func (s *Server) handleAccountsList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.reads.Accounts()})
}

// handleModels serves GET /api/models: the startup protocol probe's
// per-provider model capability matrix (see docs/web-api.md). The read port
// already projects a detached snapshot with verdict strings.
func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.reads.ModelsDocument())
}

// handleTokens serves GET /api/tokens. With no params it returns the
// cumulative provider/model token counters plus the agent-dimension breakdown
// over the same in-memory since-daemon-start window (both reset by
// POST /api/tokens/reset). Two range selectors switch to aggregating
// persisted minute buckets instead:
//
//   - ?window=1h|24h|7d|all (legacy preset contract) — minute >= now-window.
//   - ?from=&to= (unix seconds or RFC3339, same parser as /api/stats) —
//     either bound may be omitted (unbounded on that side).
//
// Boundary semantics (storage is minute-aligned): from truncates DOWN to the
// minute boundary so the whole boundary minute counts; a to inside a minute
// includes that minute's bucket (bucket start <= to). Windowed views cover
// completed persisted minutes only — sub-minute live counters appear only in
// the cumulative view. Fail-closed validation: an unknown window, an
// unparseable from/to, from > to, or window combined with from/to is a 400 —
// a malformed selector must not silently widen or narrow the reported usage.
// Windowed agent queries stay on GET /api/agents.
func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	window := q.Get("window")
	fromStr, toStr := q.Get("from"), q.Get("to")
	if window != "" && (fromStr != "" || toStr != "") {
		writeJSONErr(w, http.StatusBadRequest, "window and from/to are mutually exclusive")
		return
	}
	var from, to int64
	if window != "" {
		windowSecs, ok := observestats.ParseWindow(window)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "window must be one of 1h, 24h, 7d, all")
			return
		}
		if windowSecs > 0 {
			from = (time.Now().Unix() - windowSecs) / 60 * 60
		}
	}
	if fromStr != "" {
		n, ok := parseStatsTime(fromStr)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "from must be unix seconds or RFC3339")
			return
		}
		from = n / 60 * 60
	}
	if toStr != "" {
		n, ok := parseStatsTime(toStr)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "to must be unix seconds or RFC3339")
			return
		}
		to = n
	}
	if from > 0 && to > 0 && from > to {
		writeJSONErr(w, http.StatusBadRequest, "from must be <= to")
		return
	}
	usage, err := s.reads.Tokens(from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "token usage query: "+err.Error())
		return
	}
	agents, err := s.reads.Agents(from, to)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "agent usage query: "+err.Error())
		return
	}
	if window == "" {
		window = "all"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"usage": usage, "agents": agents, "since": s.reads.StatsSince(),
		"window": window, "from": from, "to": to,
	})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from, to := now.Add(-time.Hour).Unix(), now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			from = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			to = n
		}
	}
	bucket := observestats.NormalizeBucket(r.URL.Query().Get("bucket"))
	bs, err := s.reads.Stats(appapi.StatsQuery{From: from, To: to, Provider: r.URL.Query().Get("provider"), Model: r.URL.Query().Get("model"), BucketSecs: bucket})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "stats query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "bucket": bucket, "buckets": bs})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from, to := now.Add(-time.Hour).Unix(), now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			from = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			to = n
		}
	}
	bucket := observestats.NormalizeBucket(r.URL.Query().Get("bucket"))
	bs, err := s.reads.AgentStats(appapi.AgentStatsQuery{From: from, To: to, Agent: r.URL.Query().Get("agent"), Provider: r.URL.Query().Get("provider"), Model: r.URL.Query().Get("model"), BucketSecs: bucket})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "agent stats query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "bucket": bucket, "buckets": bs})
}

// handleSecurity serves GET /api/security: guard audit-log records (secret /
// path / drift hits) projected by the read port. kind is validated here so an
// unknown value is a client error instead of a silently empty result; from/to
// follow the /api/stats parsing convention (unix seconds or RFC3339) and are
// converted to the audit log's unix-millisecond filter domain (to is
// inclusive to the end of the named second).
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := appapi.SecurityQuery{Kind: q.Get("kind"), Limit: 100}
	switch query.Kind {
	case "", "secret", "path", "drift":
	default:
		writeJSONErr(w, http.StatusBadRequest, "kind must be secret, path or drift")
		return
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			query.Limit = n
		}
	}
	if query.Limit > 1000 {
		query.Limit = 1000
	}
	if v := q.Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			query.From = n * 1000
		}
	}
	if v := q.Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			query.To = n*1000 + 999
		}
	}
	result, err := s.reads.Security(query)
	if err != nil {
		// The underlying error may embed local paths; log it server-side and
		// return only a generic message to the client.
		log.Printf("web: security audit query failed: %v", err)
		writeJSONErr(w, http.StatusInternalServerError, "failed to query security log")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleShadowReport(w http.ResponseWriter, r *http.Request) {
	dir := s.reads.RequestLogDirectory()
	if dir == "" {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "entries": []any{}})
		return
	}
	now := time.Now()
	from, to := now.Add(-24*time.Hour).Unix(), now.Unix()
	if v := r.URL.Query().Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			from = n
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			to = n
		}
	}
	entries, err := requestlog.ShadowReport(dir, requestlog.Filter{From: time.Unix(from, 0), To: time.Unix(to, 0), Limit: 10000})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "shadow report: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "entries": entries})
}

func (s *Server) handleFusion(w http.ResponseWriter, r *http.Request) {
	stats, runs := s.reads.Fusion(r.URL.Query().Get("workflow"), time.Now())
	writeJSON(w, http.StatusOK, map[string]any{"workflows": stats, "runs": runs})
}

func (s *Server) handlePinList(w http.ResponseWriter, _ *http.Request) {
	pins := s.reads.Pins()
	out := make([]map[string]any, 0, len(pins))
	for _, p := range pins {
		expires := ""
		if !p.ExpiresAt.IsZero() {
			expires = p.ExpiresAt.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{"route": p.Route, "provider": p.Provider, "expires_at": expires})
	}
	writeJSON(w, http.StatusOK, map[string]any{"pins": out})
}

func (s *Server) handleAnalytics(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	from, to := now.Add(-30*24*time.Hour).Unix(), now.Unix()
	q := r.URL.Query()
	if v := q.Get("from"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			from = n
		}
	}
	if v := q.Get("to"); v != "" {
		if n, ok := parseStatsTime(v); ok {
			to = n
		}
	}
	g := q.Get("granularity")
	if g == "" {
		g = "day"
	}
	if g != "day" && g != "month" {
		writeJSONErr(w, http.StatusBadRequest, "granularity must be day or month")
		return
	}
	bs, err := s.reads.Analytics(appapi.AnalyticsQuery{From: from, To: to, Provider: q.Get("provider"), Model: q.Get("model"), Granularity: g})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "analytics query: "+err.Error())
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
	prices := s.reads.Pricing()
	by := map[string]*series{}
	keys := []string{}
	priced, unpriced := map[string]bool{}, map[string]bool{}
	var input, output uint64
	var total *float64
	for _, b := range bs {
		k := b.Provider + "\x00" + b.Model
		v := by[k]
		if v == nil {
			v = &series{Provider: b.Provider, Model: b.Model}
			by[k] = v
			keys = append(keys, k)
		}
		entry, ok := pricing.Resolve(prices.Overrides, prices.Catalog, b.Model)
		var cost *float64
		if ok {
			x := pricing.ComputeCost(b.Input, b.Output, b.CacheRead, b.CacheCreation, entry)
			cost = &x
			priced[b.Model] = true
			if total == nil {
				total = new(float64)
			}
			*total += x
		} else {
			unpriced[b.Model] = true
		}
		v.Points = append(v.Points, point{Bucket: b.Bucket, Requests: b.Requests, Input: b.Input, Output: b.Output, CacheCreation: b.CacheCreation, CacheRead: b.CacheRead, Cost: cost, Priced: ok})
		input += b.Input
		output += b.Output
	}
	out := make([]series, 0, len(keys))
	for _, k := range keys {
		out = append(out, *by[k])
	}
	writeJSON(w, http.StatusOK, map[string]any{"granularity": g, "from": from, "to": to, "series": out, "totals": map[string]any{"input": input, "output": output, "cost": total}, "price_coverage": map[string]any{"priced": mapKeys(priced), "unpriced": mapKeys(unpriced)}})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	d, err := s.reads.ConfigDocument()
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": d.YAML, "summary": d.Summary, "provider_models": d.ProviderModels, "provider_meta": d.ProviderMeta, "routes": d.Routes, "settings": d.Settings})
}
