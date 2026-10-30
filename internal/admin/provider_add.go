package admin

// provider_add.go — the Accounts-page Add Provider flow: persist a preset
// template or a static definition into the provider.json store, hot-reload,
// then route the optional token into the account store (never provider.json
// — red line 3). The models preview endpoint serves the form's Fetch button
// with a read-only upstream /models fetch.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	domainpresets "model-proxy/internal/presets"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerstore"
)

// AddProvider implements POST /api/providers (docs/web-api.md): preset or
// static definition → provider.json store (absorbing any yaml providers:
// section in the same mutation) → hot reload → optional token through
// AddAccount. A failed reload is extracted into ReloadWarning (the provider
// is already live when the warning is the applied-with-warning class); a
// failed ACCOUNT step after a successful provider add downgrades to
// AccountWarning — the provider itself stays.
func (s *Service) AddProvider(ctx context.Context, req appapi.ProviderAddRequest) (appapi.ProviderAddResult, error) {
	result := appapi.ProviderAddResult{Name: strings.TrimSpace(req.Name), Status: "added", Warnings: []string{}}
	if result.Name == "" {
		return appapi.ProviderAddResult{}, appapi.NewHTTPError(http.StatusBadRequest, "name is required")
	}
	if strings.ContainsAny(result.Name, " \t\r\n/") {
		return appapi.ProviderAddResult{}, appapi.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("provider name %q must not contain whitespace or slashes", result.Name))
	}
	if req.Priority < 0 {
		return appapi.ProviderAddResult{}, appapi.NewHTTPError(http.StatusBadRequest, "priority must be >= 0 (lower = tried first)")
	}
	entry, err := providerEntryFromRequest(req)
	if err != nil {
		return appapi.ProviderAddResult{}, err
	}

	err = s.mutateProvidersAndReload(ctx, func(providers map[string]configdomain.Provider) error {
		if _, exists := providers[result.Name]; exists {
			return appapi.NewHTTPError(http.StatusConflict, fmt.Sprintf("provider %q already exists — edit it instead", result.Name))
		}
		providers[result.Name] = entry
		return nil
	})
	if err != nil {
		var applied *ReloadAppliedWarning
		if errors.As(err, &applied) {
			result.ReloadWarning = applied.Err.Error()
		} else {
			return appapi.ProviderAddResult{}, err
		}
	}

	// Implicit-routing ambiguity warnings (same computation as AddPreset)
	// against the effective set that just went live.
	if cfg := s.effectiveProviderConfig(); cfg != nil {
		result.Warnings = domainpresets.AmbiguousModels(cfg, result.Name)
	}

	if req.Token != "" {
		account, accountErr := s.AddAccount(ctx, result.Name, appapi.AccountInput{APIKey: req.Token})
		if accountErr != nil {
			// The provider IS live; the credential follow-up degrades to a
			// warning instead of failing the whole request.
			result.AccountWarning = accountErr.Error()
		} else {
			result.AccountID = account.ID
			result.AccountWarning = account.Warning
		}
	}
	return result, nil
}

// providerEntryFromRequest resolves the form payload into a provider entry:
// a preset template as the base (unknown preset = 400) with every explicit
// non-empty field layered over it.
func providerEntryFromRequest(req appapi.ProviderAddRequest) (configdomain.Provider, error) {
	entry := configdomain.Provider{}
	if req.Preset != "" {
		tpl, ok := domainpresets.Lookup(req.Preset)
		if !ok {
			return entry, appapi.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("unknown preset %q", req.Preset))
		}
		entry = tpl
	}
	if req.ProviderID != "" {
		entry.Provider = req.ProviderID
	} else if entry.Provider == "" {
		entry.Provider = "static"
	}
	if req.OpenAIBaseURL != "" {
		entry.OpenAIBaseURL = req.OpenAIBaseURL
	}
	if req.AnthropicBaseURL != "" {
		entry.AnthropicBaseURL = req.AnthropicBaseURL
	}
	if req.UsageURL != "" {
		entry.UsageURL = req.UsageURL
	}
	if req.Billing != "" {
		entry.Billing = req.Billing
	}
	if req.ProxyURL != "" {
		entry.ProxyURL = req.ProxyURL
	}
	if req.Priority != 0 {
		entry.Priority = req.Priority
	}
	if len(req.Models) > 0 {
		entry.Models = append([]string(nil), req.Models...)
	}
	return entry, nil
}

// effectiveProviderConfig recomputes the effective config (yaml + store) for
// read-only projections. Nil when the config is unreadable — callers treat
// that as "no warnings" rather than failing the already-applied mutation.
func (s *Service) effectiveProviderConfig() *configdomain.Config {
	configFile := s.currentConfigFile()
	if configFile == "" {
		return nil
	}
	cfg, err := providerstore.LoadEffectiveConfigFrom(configFile, providerstore.PathUnder(s.homeDir()), false)
	if err != nil {
		return nil
	}
	return cfg
}

// PreviewProviderModels implements POST /api/providers/models-preview: fetch
// the upstream /models list for the form's transient fields, persisting
// NOTHING. OAuth-bound ids (aqp/codex) and AK/SK-signed volcengine cannot
// preview without an existing account and fail fast with the remedy.
func (s *Service) PreviewProviderModels(ctx context.Context, req appapi.ProviderModelsPreviewRequest) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entry, err := providerEntryFromRequest(appapi.ProviderAddRequest{
		Preset:           req.Preset,
		ProviderID:       req.ProviderID,
		OpenAIBaseURL:    req.OpenAIBaseURL,
		AnthropicBaseURL: req.AnthropicBaseURL,
	})
	if err != nil {
		return nil, err
	}
	if entry.OpenAIBaseURL == "" && entry.AnthropicBaseURL == "" {
		return nil, appapi.NewHTTPError(http.StatusBadRequest, "set a base URL before fetching models")
	}
	switch entry.Provider {
	case "aqp", "codex":
		return nil, appapi.NewHTTPError(http.StatusBadRequest, entry.Provider+" authenticates via OAuth — add the provider first, then log in and use models refresh")
	case "volcengine":
		return nil, appapi.NewHTTPError(http.StatusBadRequest, "volcengine model fetch needs Access Key signing — add the account first, then use models refresh")
	}
	impl, err := provider.New(&provider.Config{
		ProviderID:    entry.Provider,
		OpenAIBaseURL: entry.OpenAIBaseURL,
		BoundAPIKey:   req.Token,
	}, "preview")
	if err != nil {
		return nil, appapi.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("cannot build %s provider: %v", entry.Provider, err))
	}
	models, err := provider.FetchModelsContext(ctx, impl)
	if err != nil {
		return nil, appapi.NewHTTPError(http.StatusBadGateway, fmt.Sprintf("models endpoint unavailable: %v", err))
	}
	sort.Strings(models)
	return models, nil
}
