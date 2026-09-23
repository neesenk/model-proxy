package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/credstore"
	"model-proxy/internal/fusion"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/observe/seclog"
	observestats "model-proxy/internal/observe/stats"
	domainpresets "model-proxy/internal/presets"
	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
)

// Dashboard projects the captured generation-consistent state into the
// detached transport DTO. The port owns the lock capture; this method owns
// only the presentation mapping.
func (s *Service) Dashboard(now time.Time) appapi.Dashboard {
	state := s.ports.DashboardState(now)
	runtimeSnapshot := state.Runtime

	health := make(map[string]any, len(runtimeSnapshot.Providers))
	for name, providerState := range runtimeSnapshot.Providers {
		entry := map[string]any{
			"circuit_state": providerState.CircuitState,
			"available":     providerState.Available,
		}
		if providerState.Frozen {
			entry["frozen"] = true
		}
		if now.Before(providerState.CircuitOpenUntil) {
			entry["circuit_until"] = providerState.CircuitOpenUntil.UTC().Format(time.RFC3339)
		}
		if now.Before(providerState.RateLimitedUntil) {
			entry["rate_limited_until"] = providerState.RateLimitedUntil.UTC().Format(time.RFC3339)
			entry["rate_limit_kind"] = providerState.RateLimitKind.String()
		}
		health[name] = entry
	}
	modelLocks := map[string][]map[string]any{}
	for providerName, locks := range runtimeSnapshot.ModelLocks {
		for _, lock := range locks {
			modelLocks[providerName] = append(modelLocks[providerName], map[string]any{
				"model": lock.Model,
				"until": lock.LockedUntil.UTC().Format(time.RFC3339),
			})
		}
	}

	var quota map[string]any
	if state.QuotaEnabled {
		if snapshots := runtimeSnapshot.Quotas; snapshots != nil {
			quota = make(map[string]any, len(snapshots))
			for key, snapshot := range snapshots {
				if snapshot == nil {
					quota[key] = snapshot
					continue
				}
				// Project the current billing period's start onto a detached
				// copy (UsageWindow — the provider layer's single definition of
				// the quota-aligned usage window, same ultimate-window semantics
				// the scheduler paces against). Runtime state stays untouched;
				// zero UsageFrom = not resolvable.
				cp := *snapshot
				cp.UsageFrom, _ = snapshot.UsageWindow(now)
				quota[key] = cp
			}
		}
	}

	cacheInfo := map[string]any{"enabled": false}
	if stats := state.Cache.Stats(); stats.Entries > 0 || stats.Hits > 0 || state.Cache != nil {
		models := make([]map[string]any, 0, len(stats.Models))
		for _, m := range stats.Models {
			models = append(models, map[string]any{
				"model":   m.Name,
				"hits":    m.Hits,
				"misses":  m.Misses,
				"entries": m.Entries,
			})
		}
		cacheInfo = map[string]any{
			"enabled": true,
			"hits":    stats.Hits,
			"misses":  stats.Misses,
			// Live gauge, not cumulative: can be lower than misses (failed
			// requests miss without storing; TTL/expiry and eviction remove
			// entries while counters only grow).
			"entries": stats.Entries,
			"models":  models,
		}
	}

	counters := make(map[string]appapi.Metrics, len(state.Counters))
	for name, counter := range state.Counters {
		counters[name] = appapi.Metrics{
			Requests:       counter.Requests,
			Failovers:      counter.Failovers,
			RateLimited429: counter.RateLimited429,
			Failures:       counter.Failures,
			LastRequestAt:  counter.LastRequestAt,
			LatencySum:     counter.LatencySum,
			TTFTSum:        counter.TTFTSum,
		}
	}

	return appapi.Dashboard{
		// Truncate to whole seconds: Go's Duration.String() would render
		// nanosecond precision ("4m26.428520875s") — noise for a header label.
		Uptime:          time.Since(state.StartedAt).Truncate(time.Second).String(),
		Listen:          state.Listen,
		Health:          health,
		ModelLocks:      modelLocks,
		Quota:           quota,
		Schedule:        append(json.RawMessage(nil), state.Schedule...),
		Counters:        counters,
		Cache:           cacheInfo,
		Warnings:        append([]string(nil), state.RouteWarnings...),
		CredentialStore: string(credstore.ResolvedMode()),
	}
}

func (s *Service) LogFile() string {
	return s.ports.LogFile()
}

func (s *Service) RequestLogDirectory() string {
	return s.ports.RequestLogDirectory()
}

// RequestLogQueries returns the request-log query port: the tailing SQLite
// index when the composition root started one, the directory scan otherwise
// (index open failure degrades, never fails closed), decorated with the
// guard↔request correlation join when any guard source is available (the
// security audit store or the live adjudication ring). A nil result means the
// request log is disabled — the handlers answer their {enabled:false} shape.
func (s *Service) RequestLogQueries() appapi.RequestLogQueries {
	if s.ports.RequestLogDirectory == nil {
		return nil
	}
	dir := s.ports.RequestLogDirectory()
	if dir == "" {
		return nil
	}
	mcpDir := ""
	if s.ports.MCPRequestLogDirectory != nil {
		mcpDir = s.ports.MCPRequestLogDirectory()
	}
	var index *requestlog.Indexer
	if s.ports.RequestLogIndex != nil {
		index = s.ports.RequestLogIndex()
	}
	base := requestLogQueries{dir: dir, mcpDir: mcpDir, index: index}
	if !s.guardJoinAvailable() {
		return base
	}
	return guardJoinQueries{requestLogQueries: base, service: s}
}

// guardJoinAvailable reports whether any guard annotation source exists: the
// security audit store (audit on, directory present) or the live adjudication
// ring port. Without either the decorator would add allocation for no rows.
func (s *Service) guardJoinAvailable() bool {
	if s.ports.AdjudicationRecent != nil {
		return true
	}
	return s.auditDirIfEnabled() != ""
}

// auditDirIfEnabled derives the security audit directory when audit is on and
// the directory exists ("" otherwise). Best-effort, same convention as
// Security: fail-soft to no annotations rather than failing the request read.
func (s *Service) auditDirIfEnabled() string {
	if s.ports.Config == nil {
		return ""
	}
	cfg := s.ports.Config()
	if cfg == nil || !cfg.Guard.AuditEnabled() {
		return ""
	}
	dir := filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir()))
	if _, err := os.Stat(dir); err != nil {
		return ""
	}
	return dir
}

// requestLogQueries adapts the index (or the scan fallback) to
// appapi.RequestLogQueries. *requestlog.Indexer satisfies that interface
// structurally; this adapter only adds the nil-index degradation and the
// split-stream routing: when mcpDir is set (request_log.mcp_split on) kind=mcp
// lists read the split mcp- stream via directory scan (no index follows it),
// while every other listing is pinned to LLM rows (kind "" behaves as "llm")
// so legacy mcp rows still present in old requests- files stop polluting the
// requests view and its facets.
type requestLogQueries struct {
	dir    string
	mcpDir string
	index  *requestlog.Indexer
}

func (q requestLogQueries) SummariesWithFacets(filter requestlog.Filter) ([]requestlog.Summary, requestlog.Facets, error) {
	if q.mcpDir != "" && filter.Kind == "mcp" {
		return requestlog.QuerySummariesWithFacetsIn(q.mcpDir, requestlog.MCPFilePrefix, filter)
	}
	if q.mcpDir != "" && filter.Kind == "" {
		filter.Kind = "llm"
	}
	if q.index != nil {
		return q.index.SummariesWithFacets(filter)
	}
	return requestlog.QuerySummariesWithFacets(q.dir, filter)
}

func (q requestLogQueries) ShadowReport(filter requestlog.Filter) ([]requestlog.ShadowReportEntry, error) {
	// No mcp_split routing here: shadow evaluation rows are LLM-stream rows,
	// and both the index and the scan read exactly the requests- stream.
	if q.index != nil {
		return q.index.ShadowReport(filter)
	}
	return requestlog.ShadowReport(q.dir, filter)
}

func (q requestLogQueries) Detail(requestID, stream string) ([]requestlog.Record, error) {
	// stream hint: an MCP-stream row drills straight into the split stream —
	// its id is never in the requests index, and the hint-less URL first pays
	// the index's tail-scan fallback before the split-stream fallthrough.
	if stream == "mcp" && q.mcpDir != "" {
		return requestlog.QueryRecordsIn(q.mcpDir, requestlog.MCPFilePrefix, requestlog.Filter{RequestID: requestID, Limit: 50})
	}
	if q.index != nil {
		// The index seek already falls back to the scan on a miss, but an
		// indexing failure must be surfaced rather than hidden by the split-
		// stream fallthrough.
		records, err := q.index.Detail(requestID)
		if err != nil {
			return nil, err
		}
		if len(records) > 0 || q.mcpDir == "" || stream == "llm" {
			return records, nil
		}
	} else {
		records, err := requestlog.QueryRecords(q.dir, requestlog.Filter{RequestID: requestID, Limit: 50})
		if err != nil {
			return nil, err
		}
		if len(records) > 0 || q.mcpDir == "" || stream == "llm" {
			return records, nil
		}
	}
	// Split stream fallthrough: an id that lives in the mcp- files must stay
	// drill-downable (the UI opens /api/requests/<id> from any listing).
	return requestlog.QueryRecordsIn(q.mcpDir, requestlog.MCPFilePrefix, requestlog.Filter{RequestID: requestID, Limit: 50})
}

func (q requestLogQueries) SessionSummaries(scanLimit, limit int, costOf func(provider, model string, usage requestlog.Usage) float64) ([]requestlog.SessionSummary, error) {
	if q.index != nil {
		return q.index.SessionSummaries(scanLimit, limit, costOf)
	}
	return requestlog.SessionSummaries(q.dir, scanLimit, limit, costOf)
}

// GuardAnnotations on the raw port: the request log store itself knows
// nothing about guard state — the decorated port below owns the join.
func (q requestLogQueries) GuardAnnotations([]string) map[string][]requestlog.GuardMark { return nil }

// guardJoinQueries decorates the request-log query port with the
// guard↔request correlation: every summary page and detail read is joined
// with the security audit trail by request id (synchronous interceptions,
// async LLM verdicts, unblocks), so the requests surfaces answer "why was
// this request blocked / how was it judged" in place. The join is read-only
// and best-effort — a failing audit source degrades to unannotated rows,
// never to a failed request read.
type guardJoinQueries struct {
	requestLogQueries
	service *Service
}

func (q guardJoinQueries) SummariesWithFacets(filter requestlog.Filter) ([]requestlog.Summary, requestlog.Facets, error) {
	summaries, facets, err := q.requestLogQueries.SummariesWithFacets(filter)
	if err != nil {
		return summaries, facets, err
	}
	ids := make([]string, 0, len(summaries))
	for i := range summaries {
		ids = append(ids, summaries[i].RequestID)
	}
	for id, marks := range q.service.GuardAnnotations(ids) {
		for i := range summaries {
			if summaries[i].RequestID == id {
				summaries[i].Guard = marks
			}
		}
	}
	return summaries, facets, nil
}

func (q guardJoinQueries) GuardAnnotations(ids []string) map[string][]requestlog.GuardMark {
	return q.service.GuardAnnotations(ids)
}

// GuardAnnotations correlates one batch of request ids with the guard trail.
// Sources, merged and deduped (newest first per request):
//
//   - the live adjudication ring (ports.AdjudicationRecent): the freshest
//     verdicts including the ring-only low tier and cached-replay attribution;
//   - the security audit store (security.db, indexed by request_id): the
//     durable rows — synchronous interception records, high/medium/error/
//     skipped verdicts, and unblock trail entries.
//
// Ring and store overlap on every stored verdict; the ring entry wins the
// collision (it carries model + cached attribution), keyed by
// (kind, names, verdict, reason).
func (s *Service) GuardAnnotations(ids []string) map[string][]requestlog.GuardMark {
	out := make(map[string][]requestlog.GuardMark, len(ids))
	if len(ids) == 0 {
		return out
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 {
		return out
	}
	type markKey struct {
		kind    string
		names   string
		verdict string
		reason  string
	}
	seen := make(map[string]map[markKey]int)
	add := func(id string, m requestlog.GuardMark, enriched bool) {
		key := markKey{kind: m.Kind, names: strings.Join(m.Names, "\x00"), verdict: m.Verdict, reason: m.Reason}
		if byKey := seen[id]; byKey != nil {
			if i, ok := byKey[key]; ok {
				// The ring and the store describe the same verdict; prefer the
				// ring entry (model/cached attribution).
				if enriched {
					out[id][i] = m
				}
				return
			}
		} else {
			seen[id] = map[markKey]int{}
		}
		seen[id][key] = len(out[id])
		out[id] = append(out[id], m)
	}
	if s.ports.AdjudicationRecent != nil {
		for _, r := range s.ports.AdjudicationRecent() {
			if r.RequestID == "" || !want[r.RequestID] {
				continue
			}
			add(r.RequestID, requestlog.GuardMark{
				Ts: r.Ts, Kind: r.Kind, Names: []string{r.Rule}, Action: r.Action,
				Verdict: r.Verdict, Reason: r.Reason, Evidence: r.Evidence,
				Model: r.Model, Cached: r.Cached, Source: "judge",
			}, true)
		}
	}
	if dir := s.auditDirIfEnabled(); dir != "" {
		if byRequest, err := seclog.QueryByRequestIDs(dir, ids); err == nil {
			for id, records := range byRequest {
				if !want[id] {
					continue
				}
				for _, rec := range records {
					// Unblock is a session-level operator action, not a
					// property of the request it points back at — it stays
					// on the Security feed and the blocked-sessions card,
					// never on request rows.
					if rec.Kind == seclog.KindUnblock {
						continue
					}
					add(id, requestlog.GuardMark{
						Ts: rec.Ts, Kind: rec.Kind, Names: append([]string(nil), rec.Names...),
						Action: rec.Action, Verdict: rec.Verdict, Reason: rec.Reason,
						Evidence: rec.Evidence, Model: rec.Model, Detail: rec.Detail, Source: "audit",
					}, false)
				}
			}
		}
	}
	for id := range out {
		marks := out[id]
		sort.SliceStable(marks, func(i, j int) bool { return marks[i].Ts > marks[j].Ts })
	}
	return out
}

func (s *Service) Accounts() []appapi.ProviderAccounts {
	configs := s.ports.ProviderConfigs()
	out := make([]appapi.ProviderAccounts, 0, len(configs))
	for name, config := range configs {
		item := appapi.ProviderAccounts{
			Name:       name,
			ProviderID: config.Provider,
			Billing:    config.Billing,
			Accounts:   []appapi.Account{},
		}
		switch config.Provider {
		case "aqp":
			account, _ := provider.LoadAqpAccount(accounts.AuthFilePath(name, "oauth_auth"))
			if account != nil && account.AccountID != "" {
				item.Accounts = append(item.Accounts, appapi.Account{
					ID:      account.AccountID,
					Label:   account.Email,
					AddedAt: time.Unix(account.CreatedAt, 0).UTC().Format(time.RFC3339),
					Email:   account.Email,
				})
			}
		case "codex":
			account, _ := provider.LoadCodexAccount(accounts.AuthFilePath(name, "oauth_auth"))
			if account != nil && account.AccountID != "" {
				item.Accounts = append(item.Accounts, appapi.Account{
					ID:    account.AccountID,
					Label: account.Email,
					Email: account.Email,
				})
			}
		default:
			pool, _ := accounts.NewStore(accounts.HomeDir()).Load(name, config.Provider)
			for _, account := range pool.Accounts {
				item.Accounts = append(item.Accounts, appapi.Account{
					ID:      account.ID,
					Label:   account.Label,
					AddedAt: account.AddedAt,
				})
			}
		}
		out = append(out, item)
	}
	return out
}

// Tokens projects the per-(provider, model) token usage rows. from <= 0 &&
// to <= 0 is the cumulative hot-counter view; any bound > 0 aggregates
// persisted minute buckets (Requests is the persisted token_requests count
// there).
func (s *Service) Tokens(from, to int64) ([]appapi.TokenUsage, error) {
	if from > 0 || to > 0 {
		snapshot, err := s.ports.TokenUsageRange(from, to)
		if err != nil {
			return nil, err
		}
		out := make([]appapi.TokenUsage, 0, len(snapshot))
		for key, counters := range snapshot {
			if obscounters.IsVirtualProvider(key.Provider) {
				continue
			}
			out = append(out, appapi.TokenUsage{
				Provider:      key.Provider,
				Model:         key.Model,
				Input:         counters.Input,
				Output:        counters.Output,
				CacheCreation: counters.CacheCreation,
				CacheRead:     counters.CacheRead,
				Total:         counters.Input + counters.Output + counters.CacheCreation + counters.CacheRead,
				Requests:      counters.TokenRequests,
			})
		}
		return out, nil
	}
	snapshot := s.ports.TokenUsage()
	out := make([]appapi.TokenUsage, 0, len(snapshot))
	for key, usage := range snapshot {
		if obscounters.IsVirtualProvider(key.Provider) {
			continue
		}
		out = append(out, appapi.TokenUsage{
			Provider:      key.Provider,
			Model:         key.Model,
			Input:         usage.Input,
			Output:        usage.Output,
			CacheCreation: usage.CacheCreation,
			CacheRead:     usage.CacheRead,
			Total:         usage.Input + usage.Output + usage.CacheCreation + usage.CacheRead,
			Requests:      usage.Requests,
		})
	}
	return out, nil
}

// Agents collapses the agent counters into one row per agent with a
// per-(provider, model) breakdown (the agent-dimension counterpart of Tokens;
// same reset semantics and the same range semantics: from <= 0 && to <= 0 is
// the cumulative view, any bound > 0 aggregates persisted minute buckets).
// Agents sort by total tokens desc (heaviest first, name tiebreak); each
// agent's models sort the same way.
func (s *Service) Agents(from, to int64) ([]appapi.AgentUsage, error) {
	if from > 0 || to > 0 {
		snapshot, err := s.ports.AgentUsageRange(from, to)
		if err != nil {
			return nil, err
		}
		counts := make(map[obscounters.AgentKey]obscounters.AgentCount, len(snapshot))
		for key, counters := range snapshot {
			counts[obscounters.AgentKey{Agent: key.Agent, Provider: key.Provider, Model: key.Model}] = obscounters.AgentCount{
				Requests:      counters.Requests,
				Input:         counters.Input,
				Output:        counters.Output,
				CacheCreation: counters.CacheCreation,
				CacheRead:     counters.CacheRead,
			}
		}
		return aggregateAgentUsage(counts), nil
	}
	return aggregateAgentUsage(s.ports.AgentUsage()), nil
}

// aggregateAgentUsage groups one (agent, provider, model) counter snapshot
// into the nested per-agent + per-model payload.
func aggregateAgentUsage(snapshot map[obscounters.AgentKey]obscounters.AgentCount) []appapi.AgentUsage {
	byAgent := map[string]*appapi.AgentUsage{}
	for key, usage := range snapshot {
		total := usage.Input + usage.Output + usage.CacheCreation + usage.CacheRead
		agent := byAgent[key.Agent]
		if agent == nil {
			agent = &appapi.AgentUsage{Agent: key.Agent}
			byAgent[key.Agent] = agent
		}
		agent.Requests += usage.Requests
		agent.Input += usage.Input
		agent.Output += usage.Output
		agent.CacheCreation += usage.CacheCreation
		agent.CacheRead += usage.CacheRead
		agent.Total += total
		agent.Models = append(agent.Models, appapi.AgentModelUsage{
			Provider:      key.Provider,
			Model:         key.Model,
			Requests:      usage.Requests,
			Input:         usage.Input,
			Output:        usage.Output,
			CacheCreation: usage.CacheCreation,
			CacheRead:     usage.CacheRead,
			Total:         total,
		})
	}
	out := make([]appapi.AgentUsage, 0, len(byAgent))
	for _, agent := range byAgent {
		sort.Slice(agent.Models, func(i, j int) bool {
			if agent.Models[i].Total != agent.Models[j].Total {
				return agent.Models[i].Total > agent.Models[j].Total
			}
			if agent.Models[i].Provider != agent.Models[j].Provider {
				return agent.Models[i].Provider < agent.Models[j].Provider
			}
			return agent.Models[i].Model < agent.Models[j].Model
		})
		out = append(out, *agent)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Agent < out[j].Agent
	})
	return out
}

// StatsSince reports the oldest persisted bucket minute (unix seconds, 0 when
// no history) — the anchor for the cumulative usage "Since" label.
func (s *Service) StatsSince() int64 {
	return s.ports.StatsSince()
}

// Stats projects the stored minute buckets.
func (s *Service) Stats(query appapi.StatsQuery) ([]observestats.Bucket, error) {
	return s.ports.StatsRange(
		query.From,
		query.To,
		query.Provider,
		query.Model,
		query.BucketSecs,
	)
}

func (s *Service) AgentStats(query appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
	return s.ports.AgentStats(
		query.From,
		query.To,
		query.Agent,
		query.Provider,
		query.Model,
		query.BucketSecs,
	)
}

// Analytics projects the calendar-hour/day/month buckets and drops the virtual
// counter namespaces (guard/attempts/routing/fusion): they are request
// counters, not upstream usage, so they would otherwise surface as billable
// models with zero tokens and an "unpriced" hint. By="agent" switches to the
// agent_buckets dimension (grouped by agent/provider/model); a missing
// AnalyticsAgents port behaves like a disabled store (empty result). By=model
// with a non-empty Agent ALSO reads agent_buckets (the only table carrying
// the agent dimension — the equality filter makes its agent grouping a
// no-op); the projection clears Agent so the series still key by
// provider+model, and that table's absent failover/429 counters stay zero.
func (s *Service) Analytics(query appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	var (
		buckets []observestats.AnalyticsBucket
		err     error
	)
	if query.By == "agent" || query.Agent != "" {
		if s.ports.AnalyticsAgents == nil {
			return []observestats.AnalyticsBucket{}, nil
		}
		buckets, err = s.ports.AnalyticsAgents(
			query.From,
			query.To,
			query.Agent,
			query.Provider,
			query.Model,
			query.Granularity,
		)
	} else {
		buckets, err = s.ports.Analytics(
			query.From,
			query.To,
			query.Provider,
			query.Model,
			query.Granularity,
		)
	}
	if err != nil {
		return nil, err
	}
	out := make([]observestats.AnalyticsBucket, 0, len(buckets))
	for _, b := range buckets {
		if obscounters.IsVirtualProvider(b.Provider) {
			continue
		}
		if query.By != "agent" {
			b.Agent = ""
		}
		out = append(out, b)
	}
	return out, nil
}

// AnalyticsAgentNames lists the distinct agents with traffic in the window
// (provider/model filtered; the agent filter itself is deliberately ignored
// by the store so the suggestion list keeps offering alternatives). A
// missing port behaves like a disabled store.
func (s *Service) AnalyticsAgentNames(query appapi.AnalyticsQuery) []string {
	if s.ports.AnalyticsAgentNames == nil {
		return []string{}
	}
	return s.ports.AnalyticsAgentNames(query.From, query.To, query.Provider, query.Model)
}

func (s *Service) Pricing() appapi.PricingSnapshot {
	catalog, overrides, aliases := s.ports.Pricing()
	return appapi.PricingSnapshot{
		Catalog:   catalog,
		Overrides: overrides,
		Aliases:   aliases,
	}
}

func (s *Service) Fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return s.ports.FusionSnapshot(workflow, now)
}

func (s *Service) Pins() []appapi.Pin {
	snapshot := s.ports.Pins()
	out := make([]appapi.Pin, 0, len(snapshot))
	for route, pin := range snapshot {
		out = append(out, appapi.Pin{
			Route:     route,
			Provider:  pin.Provider,
			ExpiresAt: pin.ExpiresAt,
		})
	}
	return out
}

// ModelsDocument projects the startup protocol probe's capability matrix into
// the transport DTO. The port already returns a detached deep copy; this method
// owns only the Verdict → string presentation mapping. Providers present in the
// snapshot with no models keep their fingerprint/probed_at with an empty (never
// nil) models map so the UI can show "probed, no models recorded". The catalog
// block is a disk-only read of the models.dev cache (LoadCache — never a
// network refresh); an unreadable/absent cache degrades to the zero status.
// The match list mirrors routing.HydrateModels on the live config against that
// same cache (catalog_alias applied), so the card shows exactly the metadata
// source the next reload/takeover will use; catalog_ids is the sorted picker
// list for assigning aliases to unmatched models.
func (s *Service) ModelsDocument() appapi.ModelsDocument {
	document := appapi.ModelsDocument{Providers: map[string]appapi.ProviderModelCaps{}, Match: []appapi.ModelMatchEntry{}}
	if s.ports.ModelCapsSnapshot != nil {
		for name, caps := range s.ports.ModelCapsSnapshot() {
			models := make(map[string]appapi.ModelProtocols, len(caps.Models))
			for model, mp := range caps.Models {
				models[model] = appapi.ModelProtocols{
					Chat:      mp.Chat.String(),
					Anthropic: mp.Anthropic.String(),
					Responses: mp.Responses.String(),
				}
			}
			document.Providers[name] = appapi.ProviderModelCaps{
				Fingerprint: caps.Fingerprint,
				ProbedAt:    caps.ProbedAt,
				Models:      models,
			}
		}
	}
	cat := s.modelsCatalogCache()
	document.Catalog = modelsCatalogStatus(cat)
	var cfg *configdomain.Config
	if s.ports.Config != nil {
		cfg = s.ports.Config()
	}
	document.Match = modelsMatchProjection(cfg, cat)
	if cat != nil && cat.Count() > 0 {
		document.CatalogIDs = cat.Names()
	}
	return document
}

// modelsCatalogCache fetches the models.dev cache through the memoized disk
// port (catalog.DiskCache — never a network refresh; the composition root owns
// the memo instance). Same HomeDir port policy as PullModelsCatalog. A nil
// port or unreadable cache yields nil (zero status, empty match projection).
func (s *Service) modelsCatalogCache() *catalog.Catalog {
	if s.ports.ModelsCatalogCache == nil {
		return nil
	}
	return s.ports.ModelsCatalogCache(configdomain.ModelsCatalogPath(s.homeDir()))
}

func modelsCatalogStatus(cat *catalog.Catalog) appapi.ModelsCatalogStatus {
	if cat == nil {
		return appapi.ModelsCatalogStatus{}
	}
	status := appapi.ModelsCatalogStatus{Count: cat.Count(), ETag: cat.ETag()}
	if fetched := cat.FetchedAt(); !fetched.IsZero() {
		status.FetchedAt = &fetched
	}
	return status
}

// modelsMatchProjection lists every model HydrateModels hydrates (provider
// models ∪ route targets, deduplicated per provider) with its catalog-match
// state. The matched flag reuses HydrateModels' own source verdict so the card
// can never disagree with runtime hydration; catalog_id is the effective lookup
// id (catalog_alias target when set, else the model id).
func modelsMatchProjection(cfg *configdomain.Config, cat *catalog.Catalog) []appapi.ModelMatchEntry {
	out := []appapi.ModelMatchEntry{}
	if cfg == nil {
		return out
	}
	_, sources := routing.HydrateModels(cfg, cat)
	providers := make([]string, 0, len(sources))
	for name := range sources {
		providers = append(providers, name)
	}
	sort.Strings(providers)
	for _, name := range providers {
		models := make([]string, 0, len(sources[name]))
		for model := range sources[name] {
			models = append(models, model)
		}
		sort.Strings(models)
		for _, model := range models {
			entry := appapi.ModelMatchEntry{
				Provider:  name,
				Model:     model,
				CatalogID: model,
				Matched:   sources[name][model] == routing.SrcModelsDev,
			}
			if alias := cfg.Providers[name].CatalogAlias[model]; alias != "" {
				entry.CatalogID = alias
				entry.Aliased = true
			}
			out = append(out, entry)
		}
	}
	return out
}

// Security projects the guard audit log (seclog) into transport DTOs. The
// audit directory derives from the current generation's guard config; audit
// off or a missing directory yields an empty, disabled result (same
// convention as the request log). The projection copies names/actions and
// the scrubbed verdict fields only — seclog records never carry matched
// content.
func (s *Service) Security(query appapi.SecurityQuery) (appapi.SecurityResult, error) {
	disabled := appapi.SecurityResult{Records: []appapi.SecurityRecord{}}
	cfg := s.ports.Config()
	if cfg == nil || !cfg.Guard.AuditEnabled() {
		return disabled, nil
	}
	dir := filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir()))
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return disabled, nil
		}
		return appapi.SecurityResult{}, err
	}
	result, err := seclog.Query(dir, seclog.Filter{
		Kind:      query.Kind,
		From:      query.From,
		To:        query.To,
		Limit:     query.Limit,
		RequestID: query.RequestID,
	})
	if err != nil {
		if os.IsNotExist(err) {
			return disabled, nil
		}
		return appapi.SecurityResult{}, err
	}
	out := appapi.SecurityResult{
		Enabled: true,
		Records: make([]appapi.SecurityRecord, 0, len(result.Records)),
		Skipped: result.Skipped,
	}
	// Server-side verdict aggregation over the same window: the KPI tiles read
	// this instead of counting the client-merged feed (which drifted with the
	// in-memory ring). Low never enters the store — its cumulative counter
	// rides the adjudication stats port.
	if vc, err := seclog.Counts(dir, seclog.Filter{
		Kind: query.Kind, From: query.From, To: query.To,
	}); err == nil {
		counts := &appapi.SecurityVerdictCounts{
			High:    vc["high"],
			Medium:  vc["medium"],
			Error:   vc["error"],
			Skipped: vc["skipped"],
		}
		if s.ports.AdjudicationStats != nil {
			counts.Low = s.ports.AdjudicationStats().LowVerdicts
		}
		out.Counts = counts
	} else {
		// Records loaded but the verdict aggregation failed: surface the
		// failure instead of letting the KPI tiles fall back to silent zeros.
		// The error may embed local paths, so the client gets a fixed marker.
		logx.Warnf("[admin] security verdict counts unavailable: %v", err)
		out.CountsError = "verdict counts unavailable"
	}
	for _, record := range result.Records {
		out.Records = append(out.Records, appapi.SecurityRecord{
			Ts:        record.Ts,
			Kind:      record.Kind,
			RequestID: record.RequestID,
			SessionID: record.SessionID,
			Agent:     record.Agent,
			Protocol:  record.Protocol,
			Exposed:   record.Exposed,
			Names:     append([]string(nil), record.Names...),
			Action:    record.Action,
			Verdict:   record.Verdict,
			Reason:    record.Reason,
			Evidence:  record.Evidence,
			Model:     record.Model,
			Detail:    record.Detail,
		})
	}
	return out, nil
}

// redactPlaceholder mirrors guard.RedactPlaceholder ("[REDACTED]") — the
// literal is embedded in persisted request logs, so it is a stable wire
// constant. It is duplicated here because internal/admin must not import
// internal/guard (archtest DAG); the explain port reports through it.
const redactPlaceholder = "[REDACTED]"

// SecurityExplain implements the on-demand guard-hit analysis behind
// GET /api/security/explain: the audit record's request body is fetched from
// the request log and re-scanned through the LocateGuardHits port (the
// composition root owns the current-generation scanner). Nothing is
// persisted; the surface derives from data the admin can already read via
// the request-detail endpoint. Status semantics:
//
//   - no_request_log: request log disabled — nothing to re-scan.
//   - not_found: no record with a request body for this request id (retention
//     pruned it, or the audit predates logging).
//   - redacted: the log holds the post-redact body (guard.secrets=redact), so
//     the matched bytes were destroyed before persistence.
//   - cross_request: the hit is known_secret_fragmented, a session-window
//     detection no single stored body can reproduce.
//   - ok: matches carry per-name Located flags (false = the current scanner
//     no longer finds it — a removed rule or rotated known secret).
func (s *Service) SecurityExplain(requestID, kind string, names []string) (appapi.SecurityExplainResult, error) {
	result := appapi.SecurityExplainResult{
		Status:    appapi.SecurityExplainOK,
		RequestID: requestID,
		Kind:      kind,
		Matches:   []appapi.SecurityMatch{},
	}
	if kind != seclog.KindSecret && kind != seclog.KindPath {
		return result, fmt.Errorf("kind must be %s or %s", seclog.KindSecret, seclog.KindPath)
	}
	if requestID == "" || len(names) == 0 {
		return result, errors.New("request_id and at least one name are required")
	}
	if s.ports.LocateGuardHits == nil {
		// Fail closed: without the scanner port no explain surface at all,
		// rather than a silently empty analysis.
		return result, errors.New("security explain is not wired")
	}
	queries := s.RequestLogQueries()
	if queries == nil {
		result.Status = appapi.SecurityExplainNoRequestLog
		return result, nil
	}
	records, err := queries.Detail(requestID, "")
	if err != nil {
		return appapi.SecurityExplainResult{}, err
	}
	var body string
	for _, record := range records {
		if record.RequestBody != "" {
			body = record.RequestBody
			break
		}
	}
	if body == "" {
		result.Status = appapi.SecurityExplainNotFound
		return result, nil
	}
	matches, err := s.ports.LocateGuardHits([]byte(body), kind, names)
	if err != nil {
		if errors.Is(err, appapi.ErrGuardScannerUnavailable) {
			result.Status = appapi.SecurityExplainScannerUnavailable
			return result, nil
		}
		return appapi.SecurityExplainResult{}, err
	}
	result.Matches = matches
	located := false
	for _, m := range matches {
		if m.Located {
			located = true
			break
		}
	}
	if !located {
		switch {
		case onlyFragmentedHits(names):
			result.Status = appapi.SecurityExplainCrossRequest
		case strings.Contains(body, redactPlaceholder):
			result.Status = appapi.SecurityExplainRedacted
		}
	}
	result.Adjudications = s.adjudicationsFor(requestID, kind, names)
	return result, nil
}

// adjudicationsFor collects the recorded AI second-opinion verdicts for one
// explained request: the audit store's verdict records are the durable source
// (high/medium/error/skipped land there, once per unique content; the
// ignored-tier low rows live in the ring only), and the live ring — when
// still resident — enriches them with model and cached attribution. The
// analyze view renders the LLM judgment next to the located matches.
func (s *Service) adjudicationsFor(requestID, kind string, names []string) []appapi.SecurityExplainAdjudication {
	if requestID == "" || len(names) == 0 {
		return nil
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		want[n] = true
	}
	type key struct{ rule, verdict, reason string }
	seen := map[key]int{}
	var out []appapi.SecurityExplainAdjudication
	add := func(a appapi.SecurityExplainAdjudication) {
		k := key{a.Rule, a.Verdict, a.Reason}
		if i, ok := seen[k]; ok {
			// Prefer the ring-enriched entry (model/cached attribution).
			if a.Model != "" {
				out[i] = a
			}
			return
		}
		seen[k] = len(out)
		out = append(out, a)
	}
	if s.ports.AdjudicationRecent != nil {
		for _, r := range s.ports.AdjudicationRecent() {
			if r.RequestID != requestID || r.Kind != kind || !want[r.Rule] {
				continue
			}
			add(appapi.SecurityExplainAdjudication{
				Rule: r.Rule, Verdict: r.Verdict, Reason: r.Reason, Evidence: r.Evidence,
				Model: r.Model, Ts: r.Ts, Cached: r.Cached,
			})
		}
	}
	// The audit-store enrichment is best-effort: SecurityExplain is also
	// served with a partial port set (no Config), in which case the ring —
	// when wired — is the only source (fail-soft, same convention as the
	// other adjudication read surfaces).
	if s.ports.Config != nil {
		cfg := s.ports.Config()
		if cfg != nil && cfg.Guard.AuditEnabled() {
			dir := filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir()))
			result, err := seclog.Query(dir, seclog.Filter{Kind: kind, RequestID: requestID, Limit: 500})
			if err == nil {
				for _, rec := range result.Records {
					if rec.Verdict == "" {
						continue
					}
					for _, n := range rec.Names {
						if !want[n] {
							continue
						}
						add(appapi.SecurityExplainAdjudication{
							Rule: n, Verdict: rec.Verdict, Reason: rec.Reason,
							Evidence: rec.Evidence, Model: rec.Model, Ts: rec.Ts,
						})
						break
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ts > out[j].Ts })
	return out
}

// onlyFragmentedHits reports whether every requested name is the
// cross-request fragmented-known-secret channel.
func onlyFragmentedHits(names []string) bool {
	for _, n := range names {
		if n != "known_secret_fragmented" {
			return false
		}
	}
	return len(names) > 0
}

func (s *Service) ConfigDocument() (appapi.ConfigDocument, error) {
	path := s.currentConfigFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return appapi.ConfigDocument{}, err
	}
	config, err := configdomain.LoadConfigFromBytes(path, data)
	if err != nil {
		return appapi.ConfigDocument{}, err
	}
	providerModels := make(map[string][]string, len(config.Providers))
	providerMeta := make(map[string]appapi.ConfigProviderMeta, len(config.Providers))
	for name, providerConfig := range config.Providers {
		providerModels[name] = append([]string(nil), providerConfig.Models...)
		providerMeta[name] = appapi.ConfigProviderMeta{
			Priority: providerConfig.Priority,
			Alias:    providerConfig.Alias,
		}
	}
	table := routing.RouteTable(config)
	routes := make(map[string][]appapi.ConfigRouteTarget, len(table))
	for exposed, targets := range table {
		row := make([]appapi.ConfigRouteTarget, 0, len(targets))
		for _, target := range targets {
			row = append(row, appapi.ConfigRouteTarget{
				Provider: target.Provider,
				Model:    target.Model,
				Priority: target.Priority,
			})
		}
		routes[exposed] = row
	}
	return appapi.ConfigDocument{
		YAML: string(data),
		Summary: appapi.ConfigSummary{
			Listen:        config.Listen,
			ProviderCount: len(config.Providers),
			RouteCount:    len(table),
		},
		ProviderModels: providerModels,
		ProviderMeta:   providerMeta,
		Routes:         routes,
		Settings:       configSettings(config),
	}, nil
}

// configSettings projects the scalar blocks the Config tab edits through forms.
// Raw file values win; only log_level/log_file carry the loader's defaults (the
// form diffs against the loaded value, so an untouched field is never written).
func configSettings(config *configdomain.Config) appapi.ConfigSettings {
	settings := appapi.ConfigSettings{
		LogLevel: config.LogLevel,
		LogFile:  config.LogFile,
		Scheduling: appapi.ConfigScheduling{
			CircuitThreshold:   optionalInt(config.Scheduling.CircuitThreshold),
			CircuitCooldown:    config.Scheduling.CircuitCooldown,
			RateLimitBackoff:   config.Scheduling.RateLimitBackoff,
			QuotaCooldown:      config.Scheduling.QuotaCooldown,
			ModelLockout:       config.Scheduling.ModelLockout,
			RetryWait:          config.Scheduling.RetryWait,
			UpstreamTimeout:    config.Scheduling.UpstreamTimeout,
			StickyDwell:        config.Scheduling.StickyDwell,
			QuotaPollInterval:  config.Scheduling.QuotaPollInterval,
			QuotaSwitchMargin:  optionalInt(config.Scheduling.QuotaSwitchMargin),
			QualityErrorWeight: copyIntPtr(config.Scheduling.QualityErrorWeight),
			QualityTTFTWeight:  copyIntPtr(config.Scheduling.QualityTTFTWeight),
		},
		RequestLog: appapi.ConfigRequestLog{
			Enabled:      config.RequestLog.Enabled,
			Dir:          config.RequestLog.Dir,
			MaxFileSize:  config.RequestLog.MaxFileSize,
			MaxBodyBytes: config.RequestLog.MaxBodyBytes,
			Retention:    config.RequestLog.Retention,
			MCPSplit:     config.RequestLog.MCPSplit,
			MCPDir:       config.RequestLog.MCPDir,
		},
		Stats: appapi.ConfigStats{
			DBPath:    config.Stats.DBPath,
			Retention: config.Stats.Retention,
		},
		Cache: appapi.ConfigCache{
			Enabled:      config.Cache.Enabled,
			TTL:          config.Cache.TTL,
			MaxEntries:   config.Cache.MaxEntries,
			MaxBodyBytes: config.Cache.MaxBodyBytes,
		},
	}
	settings.Guard = appapi.ConfigGuard{
		Secrets:      config.Guard.Secrets,
		Paths:        config.Guard.Paths,
		KnownSecrets: config.Guard.KnownSecrets,
		Decode:       config.Guard.Decode,
		Audit:        config.Guard.Audit,
		SessionScan:  config.Guard.SessionScan,
		AuditPath:    config.Guard.AuditPath,
	}
	if len(config.Guard.ExtraPatterns) > 0 {
		settings.Guard.ExtraPatterns = make([]appapi.ConfigGuardPattern, 0, len(config.Guard.ExtraPatterns))
		for _, p := range config.Guard.ExtraPatterns {
			settings.Guard.ExtraPatterns = append(settings.Guard.ExtraPatterns, appapi.ConfigGuardPattern{
				Name: p.Name, Regex: p.Regex, Literal: p.Literal,
			})
		}
	}
	if len(config.Guard.ExtraPaths) > 0 {
		settings.Guard.ExtraPaths = append([]string(nil), config.Guard.ExtraPaths...)
	}
	return settings
}

// optionalInt maps the zero value ("key absent") to a nil JSON pointer.
func optionalInt(value int) *int {
	if value == 0 {
		return nil
	}
	return &value
}

// copyIntPtr detaches an optional config pointer from the loaded Config value.
func copyIntPtr(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// Presets exposes the shared preset catalog for the web Add-Provider wizard.
func (s *Service) Presets() []domainpresets.Preset {
	catalog, err := domainpresets.List()
	if err != nil {
		return nil // template drift surfaces through the CLI surface; UI degrades to empty
	}
	return catalog
}
