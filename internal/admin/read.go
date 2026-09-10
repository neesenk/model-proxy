package admin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/credstore"
	"model-proxy/internal/fusion"
	obscounters "model-proxy/internal/observe/counters"
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
				quota[key] = snapshot
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

// Analytics projects the calendar-day/month buckets and drops the virtual
// counter namespaces (guard/attempts/routing/fusion): they are request
// counters, not upstream usage, so they would otherwise surface as billable
// models with zero tokens and an "unpriced" hint.
func (s *Service) Analytics(query appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	buckets, err := s.ports.Analytics(
		query.From,
		query.To,
		query.Provider,
		query.Model,
		query.Granularity,
	)
	if err != nil {
		return nil, err
	}
	out := make([]observestats.AnalyticsBucket, 0, len(buckets))
	for _, b := range buckets {
		if obscounters.IsVirtualProvider(b.Provider) {
			continue
		}
		out = append(out, b)
	}
	return out, nil
}

func (s *Service) Pricing() appapi.PricingSnapshot {
	catalog, overrides := s.ports.Pricing()
	return appapi.PricingSnapshot{
		Catalog:   catalog,
		Overrides: overrides,
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
// nil) models map so the UI can show "probed, no models recorded".
func (s *Service) ModelsDocument() appapi.ModelsDocument {
	document := appapi.ModelsDocument{Providers: map[string]appapi.ProviderModelCaps{}}
	if s.ports.ModelCapsSnapshot == nil {
		return document
	}
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
	return document
}

// Security projects the guard audit log (seclog) into transport DTOs. The
// audit directory derives from the current generation's guard config; audit
// off or a missing directory yields an empty, disabled result (same
// convention as the request log). The projection copies names/actions only —
// seclog records never carry matched content.
func (s *Service) Security(query appapi.SecurityQuery) (appapi.SecurityResult, error) {
	disabled := appapi.SecurityResult{Records: []appapi.SecurityRecord{}}
	cfg := s.ports.Config()
	if !cfg.Guard.AuditEnabled() {
		return disabled, nil
	}
	dir := filepath.Dir(cfg.Guard.AuditPathValue(accounts.HomeDir()))
	result, err := seclog.Query(dir, seclog.Filter{
		Kind:  query.Kind,
		From:  query.From,
		To:    query.To,
		Limit: query.Limit,
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
	for _, record := range result.Records {
		out.Records = append(out.Records, appapi.SecurityRecord{
			Ts:        record.Ts,
			Kind:      record.Kind,
			RequestID: record.RequestID,
			Agent:     record.Agent,
			Protocol:  record.Protocol,
			Exposed:   record.Exposed,
			Names:     append([]string(nil), record.Names...),
			Action:    record.Action,
			Detail:    record.Detail,
		})
	}
	return out, nil
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
	return appapi.ConfigSettings{
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
