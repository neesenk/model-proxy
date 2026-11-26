package admin

// provider_delete.go — the Accounts-page Delete Provider flow: remove an
// account-less provider from the provider.json store and strip it from every
// explicit route in config.yaml (a route left without targets is removed), in
// one locked mutation with a joint rollback.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// DeleteProvider implements DELETE /api/providers/<name> (docs/web-api.md).
// Only a provider with ZERO accounts can be removed — the guard reads the
// same projection as GET /api/accounts, so a stale UI can never orphan a live
// credential (409 instead). Explicit routes that target the provider lose
// those targets (RoutesUpdated); a route left empty is deleted
// (RoutesRemoved) and falls back to implicit routing. Other references
// (route_policy, mcp) are not rewritten: the effective-config validation
// refuses the delete and both files roll back. An applied-with-warning reload
// rides back as Warning, like the other provider mutations.
func (s *Service) DeleteProvider(ctx context.Context, name string) (appapi.ProviderDeleteResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return appapi.ProviderDeleteResult{}, appapi.NewHTTPError(http.StatusBadRequest, "provider name is required")
	}
	result := appapi.ProviderDeleteResult{RoutesUpdated: []string{}, RoutesRemoved: []string{}}
	// Pre-check against the live set so a 404/409 never touches config.yaml.
	entry, ok := s.ports.ProviderConfig(name)
	if !ok {
		return appapi.ProviderDeleteResult{}, appapi.NewHTTPError(http.StatusNotFound, "unknown provider: "+name)
	}
	if err := requireNoAccounts(name, entry); err != nil {
		return appapi.ProviderDeleteResult{}, err
	}
	err := s.mutateYAMLAndProvidersAndReload(ctx,
		func(root *yaml.Node) (bool, error) {
			result.RoutesUpdated, result.RoutesRemoved = pruneProviderFromRoutes(root, name)
			return len(result.RoutesUpdated)+len(result.RoutesRemoved) > 0, nil
		},
		func(providers map[string]configdomain.Provider) error {
			// Re-check under the lock: the store is the authority, and an
			// account may have landed since the pre-check.
			entry, ok := providers[name]
			if !ok {
				return appapi.NewHTTPError(http.StatusNotFound, "unknown provider: "+name)
			}
			if err := requireNoAccounts(name, entry); err != nil {
				return err
			}
			delete(providers, name)
			return nil
		})
	if err != nil {
		var applied *ReloadAppliedWarning
		if !errors.As(err, &applied) {
			return appapi.ProviderDeleteResult{}, err
		}
		result.Warning = applied.Err.Error()
	}
	return result, nil
}

func requireNoAccounts(name string, entry configdomain.Provider) error {
	if list, _ := providerAccountList(name, entry); len(list) > 0 {
		return appapi.NewHTTPError(http.StatusConflict, fmt.Sprintf(
			"provider %q still has %d account(s) — remove them first", name, len(list)))
	}
	return nil
}

// pruneProviderFromRoutes removes every target naming provider from the
// routes: mapping (list form and map-form targets:, compact "provider/model"
// and {provider, model} targets alike), editing the yaml.Node in place so the
// remaining targets keep their comments. A route left without targets is
// deleted. A target node that does not decode is left untouched — the
// validation that follows reports it. Both name lists come back sorted.
func pruneProviderFromRoutes(root *yaml.Node, provider string) (updated, removed []string) {
	updated, removed = []string{}, []string{}
	routes := configedit.LookupChildMap(root, "routes")
	if routes == nil {
		return updated, removed
	}
	var empty []string
	for i := 0; i+1 < len(routes.Content); i += 2 {
		routeName, def := routes.Content[i].Value, routes.Content[i+1]
		targets := def
		if def.Kind == yaml.MappingNode {
			targets = configedit.ChildNode(def, "targets")
		}
		if targets == nil || targets.Kind != yaml.SequenceNode {
			continue
		}
		kept := targets.Content[:0:0]
		for _, target := range targets.Content {
			var decoded configdomain.RouteTarget
			if target.Decode(&decoded) == nil && decoded.Provider == provider {
				continue
			}
			kept = append(kept, target)
		}
		if len(kept) == len(targets.Content) {
			continue
		}
		targets.Content = kept
		if len(kept) == 0 {
			empty = append(empty, routeName)
		} else {
			updated = append(updated, routeName)
		}
	}
	for _, routeName := range empty {
		configedit.DeleteKey(routes, routeName)
	}
	sort.Strings(updated)
	sort.Strings(empty)
	return updated, append(removed, empty...)
}
