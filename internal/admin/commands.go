package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/login"
	"model-proxy/internal/observe/logx"
	domainpresets "model-proxy/internal/presets"
	"model-proxy/internal/probe"
	"model-proxy/internal/provider"
	"model-proxy/internal/routing"
)

var (
	_ appapi.ReadAPI    = (*Service)(nil)
	_ appapi.CommandAPI = (*Service)(nil)
)

func (s *Service) ResetStats() error {
	return s.ports.ResetStats()
}

// RefreshQuota synchronously refreshes one provider when name is non-empty, or
// every provider otherwise. It returns false only for an unknown named key.
func (s *Service) RefreshQuota(provider string) bool {
	if !s.ports.QuotaEnabled() {
		return true
	}
	if provider != "" {
		return s.ports.QuotaPollOne(provider)
	}
	s.ports.QuotaPollAll(time.Now())
	return true
}

func (s *Service) ResetHealth(provider string) ([]string, int, error) {
	cleared, locks := s.ports.ResetHealth(provider)
	if s.ports.QuotaEnabled() {
		if err := s.ports.QuotaPersist(); err != nil {
			return cleared, locks, err
		}
	}
	return cleared, locks, nil
}

func (s *Service) SetPin(route, provider string, ttl time.Duration) (appapi.Pin, bool) {
	expiresAt, ok := s.ports.SetPin(route, provider, ttl)
	if !ok {
		return appapi.Pin{}, false
	}
	return appapi.Pin{
		Route:     route,
		Provider:  provider,
		ExpiresAt: expiresAt,
	}, true
}

func (s *Service) ClearPin(route string) bool {
	return s.ports.ClearPin(route)
}

func (s *Service) SaveConfig(data []byte) error {
	return s.saveAndReload(data)
}

// ValidateConfig lints candidate config bytes via the shared
// internal/config pipeline without touching disk or runtime state.
func (s *Service) ValidateConfig(data []byte) []appapi.ValidationIssue {
	issues := configdomain.ValidateYAML(data)
	out := make([]appapi.ValidationIssue, 0, len(issues))
	for _, issue := range issues {
		out = append(out, appapi.ValidationIssue{Line: issue.Line, Message: issue.Message})
	}
	return out
}

func (s *Service) EditConfig(request appapi.EditRequest) error {
	switch request.Kind {
	case "general":
		return s.editGeneral(request.Data)
	case "scheduling":
		return s.editScheduling(request.Data)
	case "provider", "route", "claude_mapping":
		return s.editStructured(request.Kind, request.Name, request.Data)
	default:
		return appapi.NewHTTPError(
			http.StatusBadRequest,
			"unknown edit kind: "+request.Kind,
		)
	}
}

func (s *Service) AddAccount(
	ctx context.Context,
	name string,
	input appapi.AccountInput,
) (appapi.MutationResult, error) {
	_ = ctx // validation helpers own their request timeouts.
	providerConfig, ok := s.ports.ProviderConfig(name)
	if !ok {
		return appapi.MutationResult{}, appapi.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp", "codex":
		return appapi.MutationResult{}, appapi.NewHTTPError(
			http.StatusBadRequest,
			name+" uses the async login flow: POST /api/login/"+name+"/start",
		)
	}
	credential := accounts.Credentials{
		APIKey:    input.APIKey,
		AccessKey: input.AccessKey,
		SecretKey: input.SecretKey,
	}
	config := s.ports.Config()
	var (
		id  string
		err error
	)
	if providerConfig.Provider == "volcengine" {
		id, err = login.AddVolcengineAccount(
			config,
			name,
			providerConfig,
			credential,
			input.Label,
			input.Replace,
		)
	} else {
		id, err = login.AddApikeyAccount(
			config,
			name,
			providerConfig,
			credential,
			input.Label,
			input.Replace,
		)
	}
	if err != nil {
		return appapi.MutationResult{}, appapi.NewHTTPError(
			http.StatusBadRequest,
			err.Error(),
		)
	}
	return appapi.MutationResult{
		ID:      id,
		Warning: s.reloadAfterMutation(),
	}, nil
}

func (s *Service) ProbeAccount(
	ctx context.Context,
	name string,
	id string,
) (appapi.ProbeResult, error) {
	result, err := s.accountProbe(ctx, name, id)
	if err != nil {
		status := http.StatusNotFound
		var probeError *accountProbeError
		if errors.As(err, &probeError) && probeError.kind == accountProbeMissingModel {
			status = http.StatusBadRequest
		}
		return appapi.ProbeResult{}, appapi.NewHTTPError(status, err.Error())
	}
	return appapi.ProbeResult{
		OK:         result.ok,
		HTTPStatus: result.httpStatus,
		Reason:     result.reason,
		Provider:   result.provider,
		AccountID:  result.accountID,
		Model:      result.model,
		Latency:    result.latency,
	}, nil
}

func (s *Service) RemoveAccount(name, id string) (appapi.MutationResult, error) {
	providerConfig, ok := s.ports.ProviderConfig(name)
	if !ok {
		return appapi.MutationResult{}, appapi.NewHTTPError(
			http.StatusNotFound,
			"unknown provider: "+name,
		)
	}
	switch providerConfig.Provider {
	case "aqp":
		if err := provider.ClearAqpAccount(accounts.AuthFilePath(name, "oauth_auth")); err != nil {
			return appapi.MutationResult{}, appapi.NewHTTPError(
				http.StatusInternalServerError,
				err.Error(),
			)
		}
	case "codex":
		if err := provider.ClearCodexAccount(accounts.AuthFilePath(name, "oauth_auth")); err != nil {
			return appapi.MutationResult{}, appapi.NewHTTPError(
				http.StatusInternalServerError,
				err.Error(),
			)
		}
	default:
		if err := login.RemoveApikeyAccount(name, providerConfig.Provider, id); err != nil {
			return appapi.MutationResult{}, appapi.NewHTTPError(
				http.StatusBadRequest,
				err.Error(),
			)
		}
	}
	return appapi.MutationResult{Warning: s.reloadAfterMutation()}, nil
}

// reloadAfterMutation hot-reloads after a credential/config mutation and
// returns the warning to surface ("" on success). Either way the mutation is
// already durable; the warning tells the caller whether the runtime picked it
// up.
func (s *Service) reloadAfterMutation() string {
	err := s.ports.Reload(s.currentConfigFile())
	if err == nil {
		return ""
	}
	var applied *ReloadAppliedWarning
	if errors.As(err, &applied) {
		logx.Warnf(
			"[accounts] %v — credentials and runtime config are live, but runtime-state durability is degraded",
			err,
		)
		return err.Error()
	}
	logx.Warnf(
		"[accounts] reload after mutation failed: %v — credentials persisted, but the runtime keeps the old set until config.yaml is fixed and reloaded",
		err,
	)
	return err.Error()
}

type accountProbeErrorKind uint8

const (
	accountProbeUnknownProvider accountProbeErrorKind = iota
	accountProbeUnknownAccount
	accountProbeMissingModel
	accountProbeUnavailableProvider
)

type accountProbeError struct {
	kind accountProbeErrorKind
	msg  string
}

func (e *accountProbeError) Error() string {
	return e.msg
}

type accountProbeResult struct {
	ok         bool
	httpStatus int
	reason     string
	provider   string
	accountID  string
	model      string
	latency    time.Duration
}

// accountProbe runs a one-shot upstream probe using config and implementation
// references captured by exactly one runtime snapshot. Reload may proceed while
// credential files are checked or the network request is in flight, but the
// probe can never pair one config generation with another generation's impl.
func (s *Service) accountProbe(ctx context.Context, name, id string) (accountProbeResult, error) {
	cfg, providers := s.ports.ProbeRuntime()
	if cfg == nil {
		return accountProbeResult{}, &accountProbeError{
			kind: accountProbeUnknownProvider,
			msg:  "unknown provider: " + name,
		}
	}
	prov, ok := cfg.Providers[name]
	if !ok {
		return accountProbeResult{}, &accountProbeError{
			kind: accountProbeUnknownProvider,
			msg:  "unknown provider: " + name,
		}
	}

	// Resolve the account id to the runtime provider key. These sources mirror
	// the account-list API: OAuth providers are single-account; API-key
	// providers use a virtual name only when their pool has at least two rows.
	key := name
	switch prov.Provider {
	case "aqp":
		account, _ := provider.LoadAqpAccount(accounts.AuthFilePath(name, "oauth_auth"))
		if account == nil || account.AccountID != id {
			return accountProbeResult{}, &accountProbeError{
				kind: accountProbeUnknownAccount,
				msg:  "unknown account: " + id,
			}
		}
	case "codex":
		account, _ := provider.LoadCodexAccount(accounts.AuthFilePath(name, "oauth_auth"))
		if account == nil || account.AccountID != id {
			return accountProbeResult{}, &accountProbeError{
				kind: accountProbeUnknownAccount,
				msg:  "unknown account: " + id,
			}
		}
	default:
		pool, _ := accounts.NewStore(accounts.HomeDir()).Load(name, prov.Provider)
		found := false
		for _, account := range pool.Accounts {
			if account.ID == id {
				found = true
				break
			}
		}
		if !found {
			return accountProbeResult{}, &accountProbeError{
				kind: accountProbeUnknownAccount,
				msg:  "unknown account: " + id,
			}
		}
		if len(pool.Accounts) >= 2 {
			key = name + "#" + id
		}
	}

	model := ""
	if models := routing.RouteModelsForProvider(cfg, name); len(models) > 0 {
		model = models[0]
	} else if len(prov.Models) > 0 {
		model = prov.Models[0]
	}
	if model == "" {
		return accountProbeResult{}, &accountProbeError{
			kind: accountProbeMissingModel,
			msg:  fmt.Sprintf("%s has no model to probe (no route targets it and its models: list is empty)", name),
		}
	}
	impl := providers[key]
	if impl == nil {
		return accountProbeResult{}, &accountProbeError{
			kind: accountProbeUnavailableProvider,
			msg:  "provider " + key + " not available (reload pending?)",
		}
	}

	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}
	start := time.Now()
	probeOK, status, reason := probe.Callable(ctx, client, prov, impl, model)
	return accountProbeResult{
		ok:         probeOK,
		httpStatus: status,
		reason:     reason,
		provider:   name,
		accountID:  id,
		model:      model,
		latency:    time.Since(start),
	}, nil
}

// AddPreset merges the preset's template provider block into the live config
// and hot-reloads (same reload pipeline as every other mutation). warnings
// are the ambiguity model names (models also served by other providers
// without explicit routes); a reload failure is reported separately as
// reloadWarning — the merged block is already persisted at that point, and
// callers must not render it as a model name.
func (s *Service) AddPreset(name string) (warnings []string, reloadWarning string, err error) {
	catalog, err := domainpresets.List()
	if err != nil {
		return nil, "", err
	}
	known := false
	for _, p := range catalog {
		if p.Name == name {
			known = true
			break
		}
	}
	if !known {
		return nil, "", fmt.Errorf("unknown preset %q", name)
	}
	cfgPath := s.currentConfigFile()
	written, err := domainpresets.MergeBlock(cfgPath, name)
	if err != nil {
		return nil, "", err
	}
	_ = written // idempotent: an existing block is fine, login still proceeds
	merged, err := configdomain.LoadConfig(cfgPath)
	if err != nil {
		return nil, "", fmt.Errorf("reload merged config: %w", err)
	}
	// Same hot-reload as every other config mutation path: without it the
	// daemon would keep serving the old generation (new models 502, the new
	// provider missing from the UI) until some later mutation happened to
	// reload.
	return domainpresets.AmbiguousModels(merged, name), s.reloadAfterMutation(), nil
}
