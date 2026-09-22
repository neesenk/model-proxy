package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"model-proxy/internal/appapi"

	observeanalytics "model-proxy/internal/observe/analytics"
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
	queries := s.reads.RequestLogQueries()
	if queries == nil {
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
	// first, then the cached catalog, then the provider alias; unpriced
	// models contribute nothing.
	snapshot := s.reads.Pricing()
	costOf := func(provider, model string, usage requestlog.Usage) float64 {
		entry, ok := pricing.ResolveAliased(snapshot.Overrides, snapshot.Catalog, snapshot.Aliases, provider, model)
		if !ok {
			return 0
		}
		return pricing.ComputeCost(usage.Input, usage.Output, usage.CacheRead, usage.CacheCreation, entry)
	}
	sessions, err := queries.SessionSummaries(2000, limit, costOf)
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

func (s *Server) handleMCPSurface(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.reads.MCPSurface())
}

// handleMCPAnalytics serves GET /api/mcp/analytics: persisted MCP usage
// buckets grouped by exposed name, plus the per-tool dimension in
// tool_series. Defaults match /api/analytics (window = last 30 days,
// granularity = day); from=0 clamps to the oldest persisted bucket so the
// UI's all-time anchor matches the real window. Invalid granularity, an
// unparseable from/to, or from > to is a 400 (same fail-closed selector
// contract as /api/tokens — a malformed selector must not silently widen or
// narrow the reported window); a disabled stats store or store error is
// fail-closed with a clear message.
func (s *Server) handleMCPAnalytics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	from, to := now.Add(-30*24*time.Hour).Unix(), now.Unix()
	if v := q.Get("from"); v != "" {
		n, ok := parseStatsTime(v)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "from must be unix seconds or RFC3339")
			return
		}
		from = n
	}
	if v := q.Get("to"); v != "" {
		n, ok := parseStatsTime(v)
		if !ok {
			writeJSONErr(w, http.StatusBadRequest, "to must be unix seconds or RFC3339")
			return
		}
		to = n
	}
	if from > to {
		writeJSONErr(w, http.StatusBadRequest, "from must be <= to")
		return
	}
	if from == 0 {
		if earliest := s.reads.StatsSince(); earliest > 0 {
			from = earliest
		}
	}
	g := q.Get("granularity")
	if g == "" {
		g = "day"
	}
	switch g {
	case "minute", "hour", "day", "week", "month":
	default:
		writeJSONErr(w, http.StatusBadRequest, "granularity must be minute, hour, day, week or month")
		return
	}
	result, err := s.reads.MCPAnalytics(appapi.MCPAnalyticsQuery{
		From:        from,
		To:          to,
		Granularity: g,
		Name:        q.Get("name"),
		Tool:        q.Get("tool"),
	})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "mcp analytics query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleMCPTest runs the MCP handshake against one server (POST
// /api/mcp/test {"name": "..."}). Unknown names are a client error; a failed
// handshake is a 200 with ok:false (the UI renders the reason inline).
func (s *Server) handleMCPTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		writeJSONErr(w, http.StatusBadRequest, "mcp test: name is required")
		return
	}
	result, err := s.commands.ProbeMCP(r.Context(), req.Name)
	if err != nil {
		writePortErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleRequestsList(w http.ResponseWriter, r *http.Request) {
	queries := s.reads.RequestLogQueries()
	if queries == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "records": []any{}, "facets": requestlog.Facets{Providers: []string{}, Models: []string{}, Agents: []string{}, ProviderModels: map[string][]string{}}})
		return
	}
	q := r.URL.Query()
	f := requestlog.Filter{Model: q.Get("model"), Provider: q.Get("provider"), Agent: q.Get("agent"), Session: q.Get("session"), ErrorsOnly: q.Get("errors") != "", Limit: 100}
	if v := q.Get("shadow"); v == "only" || v == "exclude" {
		f.Shadow = v
	}
	if v := q.Get("kind"); v == "mcp" || v == "llm" {
		f.Kind = v
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
	f.UsageOnly = true
	records, facets, err := queries.SummariesWithFacets(f)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "records": records, "facets": facets})
}

func (s *Server) handleRequestDetail(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/requests/"), "/")
	// kind is the caller's stream hint (mcp rows drill the split stream
	// directly; see appapi.RequestLogQueries.Detail).
	detailStream := ""
	if v := r.URL.Query().Get("kind"); v == "mcp" || v == "llm" {
		detailStream = v
	}
	queries := s.reads.RequestLogQueries()
	if queries == nil || id == "" {
		writeJSONErr(w, http.StatusNotFound, "request logging is off or no id given")
		return
	}
	records, err := queries.Detail(id, detailStream)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "request query: "+err.Error())
		return
	}
	if len(records) == 0 {
		writeJSONErr(w, http.StatusNotFound, "no record for request id "+id)
		return
	}
	// Guard annotations (interceptions / verdicts / unblocks for this request)
	// ride the response envelope — Record itself never carries guard state.
	guard := queries.GuardAnnotations([]string{id})[id]
	if guard == nil {
		guard = []requestlog.GuardMark{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"records": records, "guard": guard})
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
	from, to := statsWindow(r.URL.Query(), time.Hour)
	bucket := observestats.NormalizeBucket(r.URL.Query().Get("bucket"))
	bs, err := s.reads.Stats(appapi.StatsQuery{From: from, To: to, Provider: r.URL.Query().Get("provider"), Model: r.URL.Query().Get("model"), BucketSecs: bucket})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "stats query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "bucket": bucket, "buckets": bs})
}

func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	from, to := statsWindow(r.URL.Query(), time.Hour)
	bucket := observestats.NormalizeBucket(r.URL.Query().Get("bucket"))
	bs, err := s.reads.AgentStats(appapi.AgentStatsQuery{From: from, To: to, Agent: r.URL.Query().Get("agent"), Provider: r.URL.Query().Get("provider"), Model: r.URL.Query().Get("model"), BucketSecs: bucket})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "agent stats query: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": from, "to": to, "bucket": bucket, "buckets": bs})
}

// handleSecurity serves GET /api/security: guard audit-log records (secret /
// path / drift hits and the unblock trail) projected by the read port. kind is
// validated here so an unknown value is a client error instead of a silently
// empty result; from/to follow the /api/stats parsing convention (unix seconds
// or RFC3339) and are converted to the audit log's unix-millisecond filter
// domain (to is inclusive to the end of the named second).
func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := appapi.SecurityQuery{Kind: q.Get("kind"), Limit: 100}
	switch query.Kind {
	case "", "secret", "path", "drift", "unblock":
	default:
		writeJSONErr(w, http.StatusBadRequest, "kind must be secret, path, drift or unblock")
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

// handleSecurityExplain serves GET /api/security/explain: the on-demand
// re-location of one audit record's hits inside the persisted request body.
// kind is validated here so drift (no request body) and unknown values are
// client errors; name is comma-separated (the audit record's names field).
// Business outcomes (no_request_log / not_found / redacted / cross_request /
// scanner_unavailable) travel in the result's status field, not HTTP codes.
func (s *Server) handleSecurityExplain(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := q.Get("kind")
	if kind != "secret" && kind != "path" {
		writeJSONErr(w, http.StatusBadRequest, "kind must be secret or path (drift records have no request body)")
		return
	}
	requestID := q.Get("request_id")
	if requestID == "" {
		writeJSONErr(w, http.StatusBadRequest, "request_id is required")
		return
	}
	var names []string
	for _, n := range strings.Split(q.Get("name"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		writeJSONErr(w, http.StatusBadRequest, "name is required")
		return
	}
	result, err := s.reads.SecurityExplain(requestID, kind, names)
	if err != nil {
		// Same convention as handleSecurity: the error may embed local paths.
		log.Printf("web: security explain failed: %v", err)
		writeJSONErr(w, http.StatusInternalServerError, "failed to analyze security record")
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
	from, to := statsWindow(r.URL.Query(), 24*time.Hour)
	entries, err := s.reads.RequestLogQueries().ShadowReport(requestlog.Filter{From: time.Unix(from, 0), To: time.Unix(to, 0), Limit: 10000})
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
	q := r.URL.Query()
	from, to := statsWindow(q, 30*24*time.Hour)
	// from=0 is the "all time" sentinel: clamp the window to the oldest
	// persisted bucket so the echoed from (the chart's grid anchor) covers
	// real history instead of epoch→now — decades of empty past also poison
	// the client's span-based granularity gating. No stats store (or an empty
	// one) keeps 0: the response simply has no series.
	if from == 0 {
		if earliest := s.reads.StatsSince(); earliest > 0 {
			from = earliest
		}
	}
	g := q.Get("granularity")
	if g == "" {
		g = "day"
	}
	switch g {
	case "minute", "hour", "day", "week", "month":
	default:
		writeJSONErr(w, http.StatusBadRequest, "granularity must be minute, hour, day, week or month")
		return
	}
	by := q.Get("by")
	if by == "" {
		by = "model"
	}
	if by != "model" && by != "agent" {
		writeJSONErr(w, http.StatusBadRequest, "by must be model or agent")
		return
	}
	query := appapi.AnalyticsQuery{From: from, To: to, Provider: q.Get("provider"), Model: q.Get("model"), Agent: q.Get("agent"), Granularity: g, By: by}
	bs, err := s.reads.Analytics(query)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "analytics query: "+err.Error())
		return
	}
	// The derived metrics (tokens totals, tok/s, cache hit, err%, weighted
	// averages, equivalent cost) come from internal/observe/analytics — the
	// single server-side definition, folded identically for the window
	// totals, the compare window and each series.
	prices := s.reads.Pricing()
	seriesOut := observeanalytics.Group(bs, by, prices.Overrides, prices.Catalog, prices.Aliases)
	totals := observeanalytics.FoldTotals(bs, prices.Overrides, prices.Catalog, prices.Aliases)
	priced, unpriced := map[providerModelPair]bool{}, map[providerModelPair]bool{}
	for _, b := range bs {
		key := providerModelPair{Provider: b.Provider, Model: b.Model}
		if _, ok := pricing.ResolveAliased(prices.Overrides, prices.Catalog, prices.Aliases, b.Provider, b.Model); ok {
			priced[key] = true
		} else {
			unpriced[key] = true
		}
	}
	// Comparison window: the equal-length span immediately before `from`
	// (one minute earlier so the inclusive minute bounds never overlap). A
	// degenerate window (to <= from) has no previous span — compare is null
	// and the UI hides the deltas. It carries the same unified derived block
	// (embedded; the UI's Δ% chips read the identical fields) plus the
	// window bounds.
	// Comparison window: the equal-length span immediately before `from`
	// (one minute earlier so the inclusive minute bounds never overlap). A
	// degenerate window (to <= from) has no previous span — compare is null
	// and the UI hides the deltas. It carries the same unified derived block
	// (embedded) plus the window bounds.
	type compareWindow struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
		observeanalytics.Totals
	}
	var compare *compareWindow
	if span := to - from; span > 0 {
		prevFrom, prevTo := from-span-60, from-60
		prev, err := s.reads.Analytics(appapi.AnalyticsQuery{From: prevFrom, To: prevTo, Provider: query.Provider, Model: query.Model, Agent: query.Agent, Granularity: g, By: by})
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "analytics compare: "+err.Error())
			return
		}
		compare = &compareWindow{From: prevFrom, To: prevTo, Totals: observeanalytics.FoldTotals(prev, prices.Overrides, prices.Catalog, prices.Aliases)}
	}
	// Usage heatmap: a FIXED window of the last twelve WHOLE months plus the
	// current month-to-date (the 1st of the month 12 months back through
	// now — independent of the toolbar's from/to; it's the GitHub-style
	// contribution overview), read through the same Analytics port at day
	// granularity so provider/model/agent filtering and virtual-provider
	// exclusion behave identically. The cells fold through the same unified
	// Totals block; the agent facet feeds the toolbar's suggestions.
	now := time.Now()
	yearY, yearM, _ := now.Date()
	yearFrom := time.Date(yearY, yearM, 1, 0, 0, 0, 0, time.Local).AddDate(0, -12, 0).Unix()
	heatBuckets, err := s.reads.Analytics(appapi.AnalyticsQuery{From: yearFrom, To: now.Unix(), Provider: query.Provider, Model: query.Model, Agent: query.Agent, Granularity: "day", By: "model"})
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "analytics heatmap: "+err.Error())
		return
	}
	agents := s.reads.AnalyticsAgentNames(query)
	if agents == nil {
		agents = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"granularity": g, "by": by, "from": from, "to": to, "series": seriesOut, "totals": totals, "compare": compare, "price_coverage": map[string]any{"priced": sortedProviderModels(priced), "unpriced": sortedProviderModels(unpriced)}, "heatmap": map[string]any{"from": yearFrom, "to": now.Unix(), "cells": observeanalytics.YearCells(heatBuckets, prices.Overrides, prices.Catalog, prices.Aliases)}, "agents": agents})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	d, err := s.reads.ConfigDocument()
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"yaml": d.YAML, "summary": d.Summary, "provider_models": d.ProviderModels, "provider_meta": d.ProviderMeta, "routes": d.Routes, "settings": d.Settings})
}

// handleSecurityBlocks serves GET /api/security/blocks: the persisted
// guard-adjudication session blocks (high verdicts), newest first.
func (s *Server) handleSecurityBlocks(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"blocks": s.reads.SecurityBlocks()})
}

// handleSecurityAllowed serves GET /api/security/allowed: the operator
// content overrides created by session-unblock cascades (hash-keyed; the
// hit bytes themselves never persist).
func (s *Server) handleSecurityAllowed(w http.ResponseWriter, _ *http.Request) {
	allowed := s.reads.SecurityAllowed()
	if allowed == nil {
		allowed = []appapi.SecurityAllowed{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"allowed": allowed})
}

// handleSecurityDisallow serves DELETE /api/security/allowed/<hash>:
// revokes one content override; the content returns to fresh adjudication
// on its next occurrence.
func (s *Server) handleSecurityDisallow(w http.ResponseWriter, r *http.Request) {
	hash := strings.TrimPrefix(r.URL.Path, "/api/security/allowed/")
	if hash == "" || strings.Contains(hash, "/") {
		writeJSONErr(w, http.StatusBadRequest, "content hash is required")
		return
	}
	if err := s.commands.SecurityDisallow(hash); err != nil {
		writePortErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "disallowed", "hash": hash})
}

// handleSecurityAdjudications serves GET /api/security/adjudications: the
// recent AI second-opinion verdicts (bounded ring, newest first, including
// suppressed low verdicts) plus the channel's LLM usage stats.
func (s *Server) handleSecurityAdjudications(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.reads.SecurityAdjudications())
}

// handleSecurityUnblock serves DELETE /api/security/blocks/<session>: clears
// one persisted session block; the session's requests are admitted again
// immediately.
func (s *Server) handleSecurityUnblock(w http.ResponseWriter, r *http.Request) {
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/security/blocks/")
	if sessionID == "" || strings.Contains(sessionID, "/") {
		writeJSONErr(w, http.StatusBadRequest, "session id is required")
		return
	}
	if err := s.commands.SecurityUnblock(sessionID); err != nil {
		writePortErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "unblocked", "session_id": sessionID})
}
