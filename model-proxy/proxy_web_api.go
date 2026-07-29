package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"model-proxy/internal/fusion"
	observestats "model-proxy/internal/observe/stats"
	webtransport "model-proxy/internal/web"
	"model-proxy/provider"
)

// proxyWebAPI is the application adapter consumed by internal/web. It owns
// projection from root-private runtime values to JSON-safe DTOs and all
// credential/config mutations; it does not own HTTP routing or sessions.
type proxyWebAPI struct {
	reads      proxyReadView
	commands   proxyAdminCommands
	configFile func() string

	newAqpClientFn  func(storePath string) *AqpClient
	newCodexOptions func() *codexLoginServerOptions
}

func newProxyWebAPI(proxy *Proxy, configFile func() string) *proxyWebAPI {
	api := &proxyWebAPI{
		reads:          proxy.readView(),
		commands:       proxy.adminCommands(),
		configFile:     configFile,
		newAqpClientFn: newAqpClient,
	}
	api.newCodexOptions = func() *codexLoginServerOptions {
		options := &codexLoginServerOptions{}
		options.defaults()
		return options
	}
	return api
}

func (api *proxyWebAPI) Dashboard(now time.Time) webtransport.Dashboard {
	view := api.reads.dashboard(now)
	counters := make(map[string]webtransport.Metrics, len(view.counters))
	for name, counter := range view.counters {
		counters[name] = webtransport.Metrics{
			Requests:       counter.Requests,
			Failovers:      counter.Failovers,
			RateLimited429: counter.RateLimited429,
			Failures:       counter.Failures,
			LastRequestAt:  counter.LastRequestAt,
			LatencySum:     counter.LatencySum,
			TTFTSum:        counter.TTFTSum,
		}
	}
	return webtransport.Dashboard{
		Uptime:     view.uptime,
		Listen:     view.listen,
		Health:     view.health,
		ModelLocks: view.modelLocks,
		Quota:      view.quota,
		Schedule:   append(json.RawMessage(nil), view.schedule...),
		Counters:   counters,
		Cache:      view.cache,
		Warnings:   append([]string(nil), view.warnings...),
	}
}

func (api *proxyWebAPI) LogFile() string {
	return api.reads.logFile()
}

func (api *proxyWebAPI) RequestLogDirectory() string {
	return api.reads.requestLogDirectory()
}

func (api *proxyWebAPI) Accounts() []webtransport.ProviderAccounts {
	configs := api.reads.providerConfigs()
	out := make([]webtransport.ProviderAccounts, 0, len(configs))
	for name, config := range configs {
		item := webtransport.ProviderAccounts{
			Name:       name,
			ProviderID: config.Provider,
			Billing:    config.Billing,
			Accounts:   []webtransport.Account{},
		}
		switch config.Provider {
		case "aqp":
			account, _ := provider.LoadAqpAccount(authFilePath(name, "oauth_auth"))
			if account != nil && account.AccountID != "" {
				item.Accounts = append(item.Accounts, webtransport.Account{
					ID:      account.AccountID,
					Label:   account.Email,
					AddedAt: time.Unix(account.CreatedAt, 0).UTC().Format(time.RFC3339),
					Email:   account.Email,
				})
			}
		case "codex":
			account, _ := provider.LoadCodexAccount(authFilePath(name, "oauth_auth"))
			if account != nil && account.AccountID != "" {
				item.Accounts = append(item.Accounts, webtransport.Account{
					ID:    account.AccountID,
					Label: account.Email,
					Email: account.Email,
				})
			}
		default:
			pool, _ := loadPool(name, config.Provider)
			for _, account := range pool.Accounts {
				item.Accounts = append(item.Accounts, webtransport.Account{
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

func (api *proxyWebAPI) Tokens() []webtransport.TokenUsage {
	snapshot := api.reads.tokenUsage()
	out := make([]webtransport.TokenUsage, 0, len(snapshot))
	for key, usage := range snapshot {
		out = append(out, webtransport.TokenUsage{
			Provider:      key.Provider,
			Model:         key.Model,
			Input:         usage.Input,
			Output:        usage.Output,
			CacheCreation: usage.CacheCreation,
			CacheRead:     usage.CacheRead,
			Requests:      usage.Requests,
		})
	}
	return out
}

func (api *proxyWebAPI) Stats(query webtransport.StatsQuery) ([]observestats.Bucket, error) {
	return api.reads.stats(
		query.From,
		query.To,
		query.Provider,
		query.Model,
		query.BucketSecs,
	)
}

func (api *proxyWebAPI) AgentStats(query webtransport.AgentStatsQuery) ([]observestats.AgentBucket, error) {
	return api.reads.agentStats(
		query.From,
		query.To,
		query.Agent,
		query.Provider,
		query.Model,
		query.BucketSecs,
	)
}

func (api *proxyWebAPI) Analytics(query webtransport.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	return api.reads.analytics(
		query.From,
		query.To,
		query.Provider,
		query.Model,
		query.Granularity,
	)
}

func (api *proxyWebAPI) Pricing() webtransport.PricingSnapshot {
	view := api.reads.pricing()
	return webtransport.PricingSnapshot{
		Catalog:   view.catalog,
		Overrides: view.overrides,
	}
}

func (api *proxyWebAPI) Fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	return api.reads.fusion(workflow, now)
}

func (api *proxyWebAPI) Pins() []webtransport.Pin {
	snapshot := api.reads.pins()
	out := make([]webtransport.Pin, 0, len(snapshot))
	for route, pin := range snapshot {
		out = append(out, webtransport.Pin{
			Route:     route,
			Provider:  pin.provider,
			ExpiresAt: pin.expiresAt,
		})
	}
	return out
}

func (api *proxyWebAPI) ConfigDocument() (webtransport.ConfigDocument, error) {
	path := api.currentConfigFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return webtransport.ConfigDocument{}, err
	}
	config, err := LoadConfigFromBytes(path, data)
	if err != nil {
		return webtransport.ConfigDocument{}, err
	}
	providerModels := make(map[string][]string, len(config.Providers))
	for name, providerConfig := range config.Providers {
		providerModels[name] = append([]string(nil), providerConfig.Models...)
	}
	routes := make(map[string][]webtransport.ConfigRouteTarget, len(config.Routes))
	for exposed, targets := range config.Routes {
		row := make([]webtransport.ConfigRouteTarget, 0, len(targets))
		for _, target := range targets {
			row = append(row, webtransport.ConfigRouteTarget{
				Provider: target.Provider,
				Model:    target.Model,
				Priority: target.Priority,
			})
		}
		routes[exposed] = row
	}
	return webtransport.ConfigDocument{
		YAML: string(data),
		Summary: webtransport.ConfigSummary{
			Listen:        config.Listen,
			ProviderCount: len(config.Providers),
			RouteCount:    len(config.Routes),
		},
		ProviderModels: providerModels,
		Routes:         routes,
	}, nil
}

func (api *proxyWebAPI) ResetStats() error {
	return api.commands.resetStats()
}

func (api *proxyWebAPI) RefreshQuota(provider string) bool {
	return api.commands.refreshQuota(provider)
}

func (api *proxyWebAPI) ResetHealth(provider string) ([]string, int, error) {
	return api.commands.resetHealthAndPersist(provider)
}

func (api *proxyWebAPI) SetPin(route, provider string, ttl time.Duration) (webtransport.Pin, bool) {
	pin, ok := api.commands.setPin(route, provider, ttl)
	if !ok {
		return webtransport.Pin{}, false
	}
	return webtransport.Pin{
		Route:     route,
		Provider:  provider,
		ExpiresAt: pin.expiresAt,
	}, true
}

func (api *proxyWebAPI) ClearPin(route string) bool {
	return api.commands.clearPin(route)
}

func (api *proxyWebAPI) SaveConfig(data []byte) error {
	return api.saveAndReload(data)
}

func (api *proxyWebAPI) EditConfig(request webtransport.EditRequest) error {
	switch request.Kind {
	case "general":
		return api.editGeneral(request.Data)
	case "scheduling":
		return api.editScheduling(request.Data)
	case "provider", "route", "claude_mapping":
		return api.editStructured(request.Kind, request.Name, request.Data)
	default:
		return webtransport.NewHTTPError(
			http.StatusBadRequest,
			"unknown edit kind: "+request.Kind,
		)
	}
}

func (api *proxyWebAPI) AddAccount(
	ctx context.Context,
	name string,
	input webtransport.AccountInput,
) (webtransport.MutationResult, error) {
	_ = ctx // validation helpers own their request timeouts.
	providerConfig, ok := api.reads.providerConfig(name)
	if !ok {
		return webtransport.MutationResult{}, webtransport.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp", "codex":
		return webtransport.MutationResult{}, webtransport.NewHTTPError(
			http.StatusBadRequest,
			name+" uses the async login flow: POST /api/login/"+name+"/start",
		)
	}
	credential := accountCred{
		APIKey:    input.APIKey,
		AccessKey: input.AccessKey,
		SecretKey: input.SecretKey,
	}
	config := api.reads.config()
	var (
		id  string
		err error
	)
	if providerConfig.Provider == "volcengine" {
		id, err = addVolcengineAccount(
			config,
			name,
			providerConfig,
			credential,
			input.Label,
			input.Replace,
		)
	} else {
		id, err = addApikeyAccount(
			config,
			name,
			providerConfig,
			credential,
			input.Label,
			input.Replace,
		)
	}
	if err != nil {
		return webtransport.MutationResult{}, webtransport.NewHTTPError(
			http.StatusBadRequest,
			err.Error(),
		)
	}
	return webtransport.MutationResult{
		ID:      id,
		Warning: api.reloadAfterMutation(),
	}, nil
}

func (api *proxyWebAPI) ProbeAccount(
	ctx context.Context,
	name string,
	id string,
) (webtransport.ProbeResult, error) {
	result, err := api.commands.accountProbe(ctx, name, id)
	if err != nil {
		status := http.StatusNotFound
		var probeError *accountProbeError
		if errors.As(err, &probeError) && probeError.kind == accountProbeMissingModel {
			status = http.StatusBadRequest
		}
		return webtransport.ProbeResult{}, webtransport.NewHTTPError(status, err.Error())
	}
	return webtransport.ProbeResult{
		OK:         result.ok,
		HTTPStatus: result.httpStatus,
		Reason:     result.reason,
		Provider:   result.provider,
		AccountID:  result.accountID,
		Model:      result.model,
		Latency:    result.latency,
	}, nil
}

func (api *proxyWebAPI) RemoveAccount(name, id string) (webtransport.MutationResult, error) {
	providerConfig, ok := api.reads.providerConfig(name)
	if !ok {
		return webtransport.MutationResult{}, webtransport.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp":
		if err := provider.ClearAqpAccount(authFilePath(name, "oauth_auth")); err != nil {
			return webtransport.MutationResult{}, webtransport.NewHTTPError(
				http.StatusInternalServerError,
				err.Error(),
			)
		}
	case "codex":
		err := os.Remove(authFilePath(name, "oauth_auth"))
		if err != nil && !os.IsNotExist(err) {
			return webtransport.MutationResult{}, webtransport.NewHTTPError(
				http.StatusInternalServerError,
				err.Error(),
			)
		}
	default:
		if err := removeApikeyAccount(name, providerConfig.Provider, id); err != nil {
			return webtransport.MutationResult{}, webtransport.NewHTTPError(
				http.StatusBadRequest,
				err.Error(),
			)
		}
	}
	return webtransport.MutationResult{Warning: api.reloadAfterMutation()}, nil
}

func (api *proxyWebAPI) currentConfigFile() string {
	if api == nil || api.configFile == nil {
		return ""
	}
	return api.configFile()
}

func (api *proxyWebAPI) reloadAfterMutation() string {
	err := api.commands.reload(api.currentConfigFile())
	if err == nil {
		return ""
	}
	var applied *reloadAppliedWarning
	if errors.As(err, &applied) {
		log.Printf(
			"[accounts] %v — credentials and runtime config are live, but runtime-state durability is degraded",
			err,
		)
		return err.Error()
	}
	log.Printf(
		"[accounts] reload after mutation failed: %v — credentials persisted, but the runtime keeps the old set until config.yaml is fixed and reloaded",
		err,
	)
	return err.Error()
}

type aqpLoginJob struct {
	api    *proxyWebAPI
	name   string
	client *AqpClient
	detail string
}

func (job *aqpLoginJob) Run(ctx context.Context) webtransport.LoginUpdate {
	fail := func(err error) webtransport.LoginUpdate {
		return webtransport.LoginUpdate{
			State:  "error",
			Detail: job.detail,
			Result: err.Error(),
		}
	}
	if _, err := job.client.PollSessionContext(ctx, 3*time.Minute); err != nil {
		return fail(err)
	}
	keyData, err := job.client.fetchAPIKeyContext(ctx)
	if err != nil {
		return fail(errors.New("api key provisioning: " + err.Error()))
	}
	account := &provider.AqpAccountData{
		AccountID:        keyData.EmployeeEmail,
		Email:            keyData.EmployeeEmail,
		ProjectID:        keyData.ProjectID,
		SSOSessionCookie: job.client.SessionCookie(),
		LastRefreshAt:    time.Now().Unix(),
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := provider.SaveAqpAccount(
		authFilePath(job.name, "oauth_auth"),
		account,
	); err != nil {
		return fail(err)
	}
	return webtransport.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  account.Email,
		Warning: job.api.reloadAfterMutation(),
	}
}

type codexLoginJob struct {
	api    *proxyWebAPI
	name   string
	state  codexLoginState
	detail string
}

// codexLoginState is application-owned device-flow state. The Web transport
// retains only the resulting LoginUpdate and never sees OAuth client details.
type codexLoginState struct {
	deviceAuthID string
	userCode     string
	interval     int
	opts         *codexLoginServerOptions
}

func (job *codexLoginJob) Run(ctx context.Context) webtransport.LoginUpdate {
	fail := func(err error) webtransport.LoginUpdate {
		return webtransport.LoginUpdate{
			State:  "error",
			Detail: job.detail,
			Result: err.Error(),
		}
	}
	authCode, err := pollForTokenContext(
		ctx,
		job.state.opts,
		job.state.deviceAuthID,
		job.state.userCode,
		job.state.interval,
	)
	if err != nil {
		return fail(err)
	}
	authFile, err := exchangeCodeForTokensContext(
		ctx,
		job.state.opts,
		provider.CodexOAuthClientID,
		authCode.AuthorizationCode,
		authCode.CodeVerifier,
	)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	path := authFilePath(job.name, "oauth_auth")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fail(err)
	}
	data, _ := json.MarshalIndent(authFile, "", "  ")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fail(err)
	}
	return webtransport.LoginUpdate{
		State:   "done",
		Detail:  job.detail,
		Result:  authFile.Tokens.AccountID,
		Warning: job.api.reloadAfterMutation(),
	}
}

func (api *proxyWebAPI) BeginLogin(
	ctx context.Context,
	name string,
) (webtransport.LoginStart, error) {
	providerConfig, ok := api.reads.providerConfig(name)
	if !ok {
		return webtransport.LoginStart{}, webtransport.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp":
		client := api.newAqpClientFn(authFilePath(name, "oauth_auth"))
		loginURL, err := client.BootstrapLoginURLContext(ctx)
		if err != nil {
			return webtransport.LoginStart{}, webtransport.NewHTTPError(
				http.StatusBadGateway,
				err.Error(),
			)
		}
		return webtransport.LoginStart{
			Provider: "aqp",
			LoginURL: loginURL,
			Job: &aqpLoginJob{
				api:    api,
				name:   name,
				client: client,
				detail: loginURL,
			},
		}, nil
	case "codex":
		options := api.newCodexOptions()
		userCode, err := requestUserCodeContext(
			ctx,
			options,
			provider.CodexOAuthClientID,
		)
		if err != nil {
			return webtransport.LoginStart{}, webtransport.NewHTTPError(
				http.StatusBadGateway,
				err.Error(),
			)
		}
		intervalSeconds, _ := strconv.Atoi(userCode.Interval)
		detail := codexOAuthVerifyURL + "  code: " + userCode.UserCode
		return webtransport.LoginStart{
			Provider:  "codex",
			VerifyURL: codexOAuthVerifyURL,
			UserCode:  userCode.UserCode,
			Job: &codexLoginJob{
				api:  api,
				name: name,
				state: codexLoginState{
					deviceAuthID: userCode.DeviceAuthID,
					userCode:     userCode.UserCode,
					interval:     intervalSeconds,
					opts:         options,
				},
				detail: detail,
			},
		}, nil
	default:
		return webtransport.LoginStart{}, webtransport.NewHTTPError(
			http.StatusBadRequest,
			name+" has no async login flow",
		)
	}
}

var (
	_ webtransport.ReadAPI    = (*proxyWebAPI)(nil)
	_ webtransport.CommandAPI = (*proxyWebAPI)(nil)
)
