// web_adapter.go — Web/admin composition adapter: the transport-owned Web server shell and the Proxy-to-admin narrow copy-by-value ports.
package app

import (
	"errors"
	"strings"

	"model-proxy/internal/accounts"
	"model-proxy/internal/admin"
	"model-proxy/internal/appapi"
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	"model-proxy/internal/login"
	obscounters "model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/upstreamproxy"
	webtransport "model-proxy/internal/web"
	"model-proxy/internal/webauth"
	"net/http"
	"time"
)

// webServer is the composition adapter around the transport-owned Web server.
// Mutable fields are retained only as construction/test seams; HTTP routing,
// sessions, assets, and background-task ownership live in internal/web.
type WebServer struct {
	server *webtransport.Server
	api    *admin.Service
	// adminAuth binds the proxy's S2 admin-auth source (reload-swapped); a
	// closure, not a *Proxy retention, keeps the root adapter composition-only.
	adminAuth  func() *webauth.Source
	configFile string
	logFile    string
	events     http.HandlerFunc

	newAqpClientFn  func(storePath string) *login.AqpClient
	newCodexOptions func() *login.CodexLoginServerOptions
}

func NewWebServer(proxy *Proxy, configFile string) *WebServer {
	runtime := proxy.SnapshotRuntime()
	browserListen := ""
	if runtime.Cfg != nil {
		browserListen = runtime.Cfg.Listen
	}
	server := &WebServer{
		adminAuth:       proxy.adminAuth.Load,
		configFile:      configFile,
		newAqpClientFn:  login.NewAqpClient,
		newCodexOptions: defaultCodexLoginOptions,
	}
	// Keep test endpoint overrides dynamic: tests replace these hooks after
	// construction, while the admin service reads them at login start.
	// The models.dev disk cache memo lives for the WebServer lifetime: the
	// Status page polls /api/models every 5s and the (mtime, size) keying
	// makes each poll one stat instead of a full cache decode (catalog owns
	// the models.dev source kernel).
	modelsCatalogCache := catalog.NewDiskCache()
	server.api = admin.New(proxy.adminPorts(
		func() string {
			return server.configFile
		},
		func(path string) *login.AqpClient {
			return server.newAqpClientFn(path)
		},
		func() *login.CodexLoginServerOptions {
			return server.newCodexOptions()
		},
		modelsCatalogCache.Load,
	))
	// The live SSE stream is served by the web transport's /api/ subtree; the
	// hub itself stays owned by the Proxy (snapshot/event producers publish
	// there), so only the handler is injected. See webtransport.Options.Events.
	server.events = func(w http.ResponseWriter, r *http.Request) {
		observeevents.ServeEvents(proxy.events, w, r)
	}
	server.server = mustNewWebTransport(server, browserListen, func() int64 {
		// Same lifetime discipline as the other stats port closures: p.stats
		// is bound once by initStats before serving starts and never swapped,
		// so it is read bare at call time.
		if proxy.stats == nil {
			return 0
		}
		return proxy.stats.EarliestMinuteAll()
	})
	return server
}

func mustNewWebTransport(server *WebServer, browserListen string, mcpStatsSince func() int64) *webtransport.Server {
	transport, err := webtransport.New(webtransport.Options{
		// reads decorates the admin service with the MCP-aware all-time
		// anchor (appapi.MCPStatsSinceReader): the oldest persisted bucket
		// across ALL stats tables, including the MCP pair that the LLM-only
		// StatsSince port does not see. Interface-optional because the reads
		// port itself lives behind admin.Ports; embedding keeps every other
		// method on the unchanged delegation path.
		Reads:    mcpAllTimeReads{ReadAPI: server.api, mcpStatsSince: mcpStatsSince},
		Commands: server.api,
		Version:  Version,
		Events:   server.events,
		// S2 admin-surface auth follows the proxy's config generation (the
		// transport is built once; the closure picks up reload-swapped sources).
		AdminAuth:     server.adminAuth,
		BrowserListen: browserListen,
		LogFile: func() string {
			return server.logFile
		},
	})
	if err != nil {
		panic("construct Web transport: " + err.Error())
	}
	return transport
}

// mcpAllTimeReads is the appapi.ReadAPI handed to the web transport: the
// admin service plus the MCP-aware StatsSince companion. Composition-only
// (one field-holding wrapper, no logic beyond the delegation closure).
type mcpAllTimeReads struct {
	appapi.ReadAPI
	mcpStatsSince func() int64
}

// MCPStatsSince implements appapi.MCPStatsSinceReader: the oldest persisted
// bucket minute across all stats tables (0 when nothing is persisted yet).
func (r mcpAllTimeReads) MCPStatsSince() int64 {
	if r.mcpStatsSince == nil {
		return 0
	}
	return r.mcpStatsSince()
}

func (server *WebServer) Register(mux *http.ServeMux) {
	server.server.Register(mux)
}

func (server *WebServer) Start() bool {
	return server.server.Start()
}

func (server *WebServer) Close() {
	server.server.Close()
}

// SetLogFile records the daemon log path shown in the UI.
func (server *WebServer) SetLogFile(path string) { server.logFile = path }

// defaultCodexLoginOptions wires production codex OAuth endpoints.
func defaultCodexLoginOptions() *login.CodexLoginServerOptions {
	options := &login.CodexLoginServerOptions{}
	options.Defaults()
	return options
}

// adminPorts adapts Proxy state to the admin service's narrow copy-by-value
// ports. Every closure owns lock discipline (p.mu / SnapshotRuntime stays
// here) and returns detached snapshots — never live map references into
// reload-owned state — so internal/admin never touches locks, generations, or
// the composition root directly.
func (p *Proxy) adminPorts(
	configFile func() string,
	newAqpClient func(storePath string) *login.AqpClient,
	newCodexOptions func() *login.CodexLoginServerOptions,
	modelsCatalogCache func(path string) *catalog.Catalog,
) admin.Ports {
	return admin.Ports{
		ConfigFile:         configFile,
		Config:             p.snapshotConfig,
		ModelsCatalogCache: modelsCatalogCache,
		ProviderConfig: func(name string) (configdomain.Provider, bool) {
			p.mu.RLock()
			defer p.mu.RUnlock()
			config, ok := p.cfg.Providers[name]
			return config, ok
		},
		ProviderConfigs: func() map[string]configdomain.Provider {
			p.mu.RLock()
			defer p.mu.RUnlock()
			configs := make(map[string]configdomain.Provider, len(p.cfg.Providers))
			for name, config := range p.cfg.Providers {
				configs[name] = config
			}
			return configs
		},
		LogFile: func() string {
			p.mu.RLock()
			defer p.mu.RUnlock()
			return p.cfg.LogFile
		},
		MCPState: func() admin.MCPState {
			// One generation capture for config + pool identity; session gauges
			// come from the process-lifetime table (cross-generation by design).
			p.mu.RLock()
			cfg := p.cfg
			providers := p.providers
			poolIndex := p.poolIndex
			p.mu.RUnlock()
			counts := p.mcpSessions.ServerCounts()
			stats := p.mcpStats.Snapshot()
			state := admin.MCPState{}
			for name, srv := range cfg.MCP {
				st := stats[name]
				transport := srv.Transport
				if transport == "" {
					transport = "streamable"
				}
				accounts := 0
				if srv.MCPAuthMode() == "provider" {
					if ids, ok := poolIndex[srv.Provider]; ok {
						accounts = len(ids)
					} else if _, ok := providers[srv.Provider]; ok {
						accounts = 1
					}
				}
				state.Servers = append(state.Servers, admin.MCPServerState{
					Name:         name,
					Enabled:      srv.MCPEffectiveEnabled(),
					Transport:    transport,
					Auth:         srv.MCPAuthMode(),
					Provider:     srv.Provider,
					URL:          srv.URL,
					Command:      strings.Join(srv.Command, " "),
					Accounts:     accounts,
					Sessions:     counts[name],
					Calls:        st.Calls,
					Errors:       st.Errors,
					AvgLatencyMs: st.AvgLatencyMs,
				})
			}
			for name, route := range cfg.MCPRoutes {
				st := stats[name]
				rs := admin.MCPRouteState{
					Name:         name,
					Enabled:      route.MCPRouteEffectiveEnabled(),
					Sessions:     counts[name],
					Calls:        st.Calls,
					Errors:       st.Errors,
					AvgLatencyMs: st.AvgLatencyMs,
				}
				for _, t := range route.Targets {
					rs.Targets = append(rs.Targets, admin.MCPRouteTargetState{Server: t.Server, Tools: len(t.Tools)})
				}
				state.Routes = append(state.Routes, rs)
			}
			return state
		},
		DashboardState: func(now time.Time) admin.DashboardState {
			// Capture reload-owned values and the Manager dashboard under the
			// repository lock order so config and generation-scoped state
			// cannot cross generations; the schedule preview derives from the
			// same capture (shared with /debug/schedule).
			p.mu.RLock()
			cfg := p.cfg
			warnings := append([]string(nil), p.routeWarnings...)
			cache := p.cache
			expanded := p.expandedRoutes
			parentOf := p.parentOf
			poolIndex := p.poolIndex
			runtimeSnapshot := p.runtimeState.Dashboard(now)
			p.mu.RUnlock()
			return admin.DashboardState{
				Listen:        cfg.Listen,
				RouteWarnings: warnings,
				Cache:         cache,
				Runtime:       runtimeSnapshot,
				QuotaEnabled:  p.quota != nil,
				StartedAt:     p.metrics.StartedAt(),
				Counters:      p.metrics.AggregateByProvider(),
				Schedule: scheduleStatusFromSnapshot(
					cfg,
					expanded,
					parentOf,
					poolIndex,
					runtimeSnapshot,
					now,
				),
			}
		},
		RequestLogDirectory: func() string {
			return p.reqLog.Directory()
		},
		MCPRequestLogDirectory: func() string {
			// Same restart-only lifetime as reqLog (set once in initRequestLog
			// before serving starts, never swapped) — no lock, same discipline.
			if p.mcpReqLog == nil {
				return ""
			}
			return p.mcpReqLog.Directory()
		},
		RequestLogIndex: func() *requestlog.Indexer {
			// Process-lifetime and never swapped (set once in initRequestLog
			// before serving starts), so no lock — same discipline as reqLog.
			return p.reqLogIndex
		},
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage {
			if p.tokens == nil {
				return nil
			}
			return p.tokens.Snapshot()
		},
		AgentUsage: func() map[obscounters.AgentKey]obscounters.AgentCount {
			if p.agents == nil {
				return nil
			}
			return p.agents.Snapshot()
		},
		TokenUsageRange: func(from, to int64) (map[observestats.Key]observestats.Counters, error) {
			if p.stats == nil {
				return map[observestats.Key]observestats.Counters{}, nil
			}
			return p.stats.LoadCumulativeRange(from, to)
		},
		AgentUsageRange: func(from, to int64) (map[observestats.AgentKey]observestats.AgentCounters, error) {
			if p.stats == nil {
				return map[observestats.AgentKey]observestats.AgentCounters{}, nil
			}
			return p.stats.LoadCumulativeAgentsRange(from, to)
		},
		StatsSince: func() int64 {
			if p.stats == nil {
				return 0
			}
			return p.stats.EarliestMinute()
		},
		StatsRange: func(from, to int64, provider, model string, bucketSecs int64) ([]observestats.Bucket, error) {
			if p.stats == nil {
				return []observestats.Bucket{}, nil
			}
			return p.stats.QueryRange(from, to, provider, model, bucketSecs)
		},
		AgentStats: func(from, to int64, agent, provider, model string, bucketSecs int64) ([]observestats.AgentBucket, error) {
			if p.stats == nil {
				return []observestats.AgentBucket{}, nil
			}
			return p.stats.QueryAgents(from, to, agent, provider, model, bucketSecs)
		},
		Analytics: func(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			if p.stats == nil {
				return []observestats.AnalyticsBucket{}, nil
			}
			return p.stats.QueryAnalytics(from, to, provider, model, granularity)
		},
		AnalyticsAgents: func(from, to int64, agent, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			if p.stats == nil {
				return []observestats.AnalyticsBucket{}, nil
			}
			return p.stats.QueryAnalyticsAgents(from, to, agent, provider, model, granularity)
		},
		AnalyticsAgentNames: func(from, to int64, provider, model string) []string {
			if p.stats == nil {
				return []string{}
			}
			names, err := p.stats.QueryAgentNames(from, to, provider, model)
			if err != nil {
				return []string{}
			}
			return names
		},
		MCPAnalytics: func(from, to int64, granularity, name, tool string) ([]observestats.MCPBucketRow, []observestats.MCPToolBucketRow, error) {
			if p.stats == nil {
				return nil, nil, errors.New("MCP stats store is not available")
			}
			rows, err := p.stats.QueryMCPBuckets(from, to, granularity)
			if err != nil {
				return nil, nil, err
			}
			if name != "" {
				filtered := make([]observestats.MCPBucketRow, 0, len(rows))
				for _, r := range rows {
					if r.Name != name {
						continue
					}
					filtered = append(filtered, r)
				}
				rows = filtered
			}
			toolRows, err := p.stats.QueryMCPToolBuckets(from, to, granularity)
			if err != nil {
				return nil, nil, err
			}
			if name != "" || tool != "" {
				filtered := make([]observestats.MCPToolBucketRow, 0, len(toolRows))
				for _, r := range toolRows {
					if name != "" && r.Name != name {
						continue
					}
					if tool != "" && r.Tool != tool {
						continue
					}
					filtered = append(filtered, r)
				}
				toolRows = filtered
			}
			return rows, toolRows, nil
		},
		FusionSnapshot: func(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
			return p.fusionReg.Snapshot(workflow, now)
		},
		Pins: func() map[string]admin.PinState {
			pins := p.listPins()
			out := make(map[string]admin.PinState, len(pins))
			for route, pin := range pins {
				out[route] = admin.PinState{Provider: pin.provider, ExpiresAt: pin.expiresAt}
			}
			return out
		},
		ModelCapsSnapshot: func() map[string]runtimewire.ProviderModelCaps {
			// modelCaps is process-lifetime (not reload-owned) and Snapshot owns
			// the store's leaf lock, so this closure deliberately takes no p.mu —
			// it cannot observe a mixed config generation.
			return p.modelCaps.Snapshot()
		},
		Pricing: func() (*pricing.Catalog, map[string]pricing.Override, map[string]string) {
			overrides, catalog, aliases := p.detachedPricing()
			return catalog, overrides, aliases
		},
		ResetStats:   p.resetStats,
		ResetHealth:  p.resetHealth,
		FreezeHealth: p.freezeHealth,
		SetModelDisabled: func(provider, model string, disabled bool) error {
			p.runtimeState.SetModelDisabled(provider, model, disabled)
			return p.persistDisabledModels()
		},
		DisabledModels: func() map[string][]string {
			return p.runtimeState.DisabledModels()
		},
		QuotaEnabled: func() bool {
			return p.quota != nil
		},
		QuotaPollOne: func(name string) bool {
			return p.quota.PollOne(name)
		},
		QuotaPollAll: func(now time.Time) {
			p.quota.PollAll(now)
		},
		QuotaPersist: func() error {
			return p.quota.Persist()
		},
		SetPin: func(route, provider string, ttl time.Duration) (time.Time, bool) {
			entry, ok := p.setPin(route, provider, ttl)
			return entry.expiresAt, ok
		},
		ClearPin: p.clearPin,
		Reload: func(configFile string) error {
			// Convert the root's applied-warning into the admin-visible type
			// at the port boundary so internal/admin never imports app; the
			// message format is unchanged.
			err := p.Reload(configFile)
			if err == nil {
				return nil
			}
			var applied *ReloadAppliedWarning
			if errors.As(err, &applied) {
				return &admin.ReloadAppliedWarning{Err: applied.Err}
			}
			return err
		},
		ProbeRuntime: func() (*configdomain.Config, map[string]provider.Provider) {
			runtime := p.SnapshotRuntime()
			return runtime.Cfg, runtime.Providers
		},
		RouteProbeImpl: func(name string) provider.Provider {
			// Same parent-or-first-pooled-virtual resolution as
			// ModelRefreshRuntime: the forward path binds the first pooled
			// virtual's credentials, so the probe must use the same impl.
			p.mu.RLock()
			defer p.mu.RUnlock()
			impl := p.providers[name]
			if impl == nil {
				if vids := p.poolIndex[name]; len(vids) > 0 {
					impl = p.providers[vids[0]]
				}
			}
			return impl
		},
		ProbeMCP:             p.probeMCP,
		LocateGuardHits:      p.locateGuardHits,
		AdjudicationBlocks:   p.adjudicationBlocks,
		AdjudicationUnblock:  p.adjudicationUnblock,
		AdjudicationAllowed:  p.adjudicationAllowed,
		AdjudicationDisallow: p.adjudicationDisallow,
		AdjudicationRecent:   p.adjudicationRecent,
		AdjudicationStats:    p.adjudicationStats,
		AdjudicationEnabled:  p.adjudicationEnabled,
		ModelRefreshRuntime: func(name string) admin.ModelRefreshRuntime {
			// Same parent-or-first-pooled-virtual resolution as the model-caps
			// probe pass: the model list is per-upstream, not per-account.
			p.mu.RLock()
			cfg := p.cfg
			impl := p.providers[name]
			if impl == nil {
				if vids := p.poolIndex[name]; len(vids) > 0 {
					impl = p.providers[vids[0]]
				}
			}
			p.mu.RUnlock()
			return admin.ModelRefreshRuntime{
				Config: cfg, Provider: impl,
				Fingerprint: providerbuild.ProtocolConfigFingerprint(cfg.Providers[name]),
				Client:      &http.Client{Timeout: cfg.Scheduling.Timeout(), Transport: upstreamproxy.AutoTransport()},
			}
		},
		ModelCapsReplace: func(name, fingerprint string, models map[string]runtimewire.ModelProtocols) bool {
			p.mu.RLock()
			provCfg, ok := p.cfg.Providers[name]
			if !ok || providerbuild.ProtocolConfigFingerprint(provCfg) != fingerprint {
				p.mu.RUnlock()
				return false
			}
			p.modelCaps.ReplaceProviderModels(name, fingerprint, models, time.Now())
			p.mu.RUnlock()
			p.persistModelCaps()
			return true
		},
		NewAqpClient:    newAqpClient,
		NewCodexOptions: newCodexOptions,
		// Takeover (user template dir + models.dev catalog cache) roots at the
		// same home the accounts store uses.
		HomeDir: accounts.HomeDir,
	}
}
