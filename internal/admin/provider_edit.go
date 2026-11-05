package admin

// provider_edit.go — provider mutations against the provider.json store
// (internal/providerstore), the single provider authority. The structured
// config editor's provider kind and the model-refresh write-back route here;
// routes and every non-provider config section keep their yaml.Node path in
// config_edit.go.

import (
	"context"
	"errors"
	"fmt"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/observe/logx"
	"model-proxy/internal/providerstore"
)

// mutateProvidersAndReload runs the provider-store mutation, hot-reload and
// any rollback under one shared config-lock span. A reload that failed to
// apply rolls the store back before another writer can commit; an
// applied-with-warning reload keeps the new store and returns the warning.
func (s *Service) mutateProvidersAndReload(ctx context.Context, fn func(providers map[string]configdomain.Provider) error) error {
	configFile := s.currentConfigFile()
	if configFile == "" {
		return fmt.Errorf("config file path is unavailable; cannot mutate providers")
	}
	storePath := providerstore.PathUnder(s.homeDir())
	return configedit.WithConfigLockContext(ctx, configFile, func() error {
		var previous map[string]configdomain.Provider
		err := providerstore.MutateContextUnderLock(ctx, configFile, storePath, func(providers map[string]configdomain.Provider) error {
			previous = providerstore.Clone(providers)
			return fn(providers)
		})
		if err != nil {
			var strip *providerstore.StripWarning
			if !errors.As(err, &strip) {
				return err
			}
			// The store was persisted; only the yaml strip failed (it retries on
			// the next load). Reload anyway and surface the degradation.
			logx.Warnf("[admin] ⚠ %v", err)
		}
		if err := s.ports.Reload(configFile); err != nil {
			var applied *ReloadAppliedWarning
			if errors.As(err, &applied) {
				return err
			}
			if previous != nil {
				if rollbackErr := providerstore.Save(storePath, previous); rollbackErr != nil {
					return fmt.Errorf("%w (provider store rollback failed: %v)", err, rollbackErr)
				}
			}
			return err
		}
		return nil
	})
}

// editProvider applies the structured editor's provider payload to the store:
// the same field whitelist the yaml path exposed (provider_id, base URLs,
// usage_url, billing, models, alias, catalog_alias) plus delete. Creating a
// provider starts from an empty entry — the effective-config validation inside
// the mutation rejects one without a base URL or provider_id.
func (s *Service) editProvider(name string, data map[string]any) error {
	return s.mutateProvidersAndReload(context.Background(), func(providers map[string]configdomain.Provider) error {
		if data["delete"] == true {
			delete(providers, name) // absent name: idempotent no-op, like the yaml path
			return nil
		}
		entry, ok := providers[name]
		if !ok {
			// New provider: provider_id is an implementation detail the forms
			// no longer ask for — a hand-filled entry is a generic
			// OpenAI-compatible endpoint (static); vendor protocols come from
			// a preset template (AddProvider) or this same default.
			entry = configdomain.Provider{Provider: "static"}
		}
		for _, key := range []string{"provider_id", "openai_base_url", "anthropic_base_url", "usage_url", "billing"} {
			if value, present := data[key]; present {
				applyProviderScalar(&entry, key, fmt.Sprint(value))
			}
		}
		if models, present := data["models"]; present {
			entry.Models = stringSlice(models)
		}
		// alias: full-map replace (upstream real model id → exposed name); an
		// empty map clears the override. Keys must be in the models list and
		// exposed names must stay unique per provider — enforced fail-closed by
		// ValidateProviders inside providerstore.Save.
		if alias, present := data["alias"]; present {
			entry.Alias = stringMap(alias)
		}
		// catalog_alias: full-map replace; an empty map clears the override.
		if catalogAlias, present := data["catalog_alias"]; present {
			entry.CatalogAlias = stringMap(catalogAlias)
		}
		providers[name] = entry
		return nil
	})
}

// applyProviderScalar maps a whitelist key to its Provider field.
func applyProviderScalar(entry *configdomain.Provider, key, value string) {
	switch key {
	case "provider_id":
		entry.Provider = value
	case "openai_base_url":
		entry.OpenAIBaseURL = value
	case "anthropic_base_url":
		entry.AnthropicBaseURL = value
	case "usage_url":
		entry.UsageURL = value
	case "billing":
		entry.Billing = value
	}
}

// stringSlice coerces a JSON-decoded list ([]any of strings) to []string;
// nil for anything absent or not a list (validation rejects surprises).
func stringSlice(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil
		}
		out = append(out, s)
	}
	return out
}

// stringMap coerces a JSON-decoded object to map[string]string; an empty or
// non-map value clears the override (nil).
func stringMap(value any) map[string]string {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return nil
	}
	out := make(map[string]string, len(object))
	for k, v := range object {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		out[k] = s
	}
	return out
}
