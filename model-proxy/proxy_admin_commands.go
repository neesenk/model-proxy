package main

import (
	"context"
	"fmt"
	"model-proxy/internal/probe"
	"net/http"
	"time"

	"model-proxy/provider"
)

// proxyAdminCommands is the Web/API mutation and active-probe boundary. It
// keeps transport handlers from retaining or reaching through the composition
// root while preserving the existing application-level command semantics.
type proxyAdminCommands struct {
	proxy *Proxy
}

func (p *Proxy) adminCommands() proxyAdminCommands {
	return proxyAdminCommands{proxy: p}
}

func (commands proxyAdminCommands) resetStats() error {
	return commands.proxy.resetStats()
}

// refreshQuota synchronously refreshes one provider when name is non-empty, or
// every provider otherwise. It returns false only for an unknown named key.
func (commands proxyAdminCommands) refreshQuota(name string) bool {
	if commands.proxy.quota == nil {
		return true
	}
	if name != "" {
		return commands.proxy.quota.pollOne(name)
	}
	commands.proxy.quota.pollAll(time.Now())
	return true
}

func (commands proxyAdminCommands) resetHealthAndPersist(name string) ([]string, int, error) {
	cleared, locks := commands.proxy.resetHealth(name)
	if commands.proxy.quota != nil {
		if err := commands.proxy.quota.persist(); err != nil {
			return cleared, locks, err
		}
	}
	return cleared, locks, nil
}

func (commands proxyAdminCommands) setPin(route, provider string, ttl time.Duration) (pinEntry, bool) {
	return commands.proxy.setPin(route, provider, ttl)
}

func (commands proxyAdminCommands) clearPin(route string) bool {
	return commands.proxy.clearPin(route)
}

func (commands proxyAdminCommands) reload(configFile string) error {
	return commands.proxy.reload(configFile)
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
func (commands proxyAdminCommands) accountProbe(ctx context.Context, name, id string) (accountProbeResult, error) {
	runtime := commands.proxy.snapshotRuntime()
	cfg := runtime.cfg
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
		account, _ := provider.LoadAqpAccount(authFilePath(name, "oauth_auth"))
		if account == nil || account.AccountID != id {
			return accountProbeResult{}, &accountProbeError{
				kind: accountProbeUnknownAccount,
				msg:  "unknown account: " + id,
			}
		}
	case "codex":
		account, _ := provider.LoadCodexAccount(authFilePath(name, "oauth_auth"))
		if account == nil || account.AccountID != id {
			return accountProbeResult{}, &accountProbeError{
				kind: accountProbeUnknownAccount,
				msg:  "unknown account: " + id,
			}
		}
	default:
		pool, _ := loadPool(name, prov.Provider)
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
	if models := routeModelsForProvider(cfg, name); len(models) > 0 {
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
	impl := runtime.providers[key]
	if impl == nil {
		return accountProbeResult{}, &accountProbeError{
			kind: accountProbeUnavailableProvider,
			msg:  "provider " + key + " not available (reload pending?)",
		}
	}

	client := &http.Client{Timeout: cfg.Scheduling.Timeout()}
	start := time.Now()
	ok, status, reason := probe.Callable(ctx, client, prov, impl, model)
	return accountProbeResult{
		ok:         ok,
		httpStatus: status,
		reason:     reason,
		provider:   name,
		accountID:  id,
		model:      model,
		latency:    time.Since(start),
	}, nil
}
