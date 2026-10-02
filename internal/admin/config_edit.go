package admin

import (
	"bytes"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

func (s *Service) editConfigNode(mutate func(*yaml.Node) error) error {
	// The whole load→mutate→write span runs under the config file lock:
	// AddPreset and the CLI `add` command RMW the same config.yaml (the CLI
	// from another process); without the lock the later writer's rename
	// silently discards the earlier writer's change.
	configFile := s.currentConfigFile()
	return configedit.WithConfigLock(configFile, func() error {
		root, err := configedit.LoadNode(configFile)
		if err != nil {
			return err
		}
		if configedit.MapNode(root) == nil {
			return fmt.Errorf("config is not a YAML mapping")
		}
		if err := mutate(root); err != nil {
			return err
		}
		var buffer bytes.Buffer
		encoder := yaml.NewEncoder(&buffer)
		encoder.SetIndent(2)
		if err := encoder.Encode(root); err != nil {
			return err
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		return s.saveAndReloadUnderLock(configFile, buffer.Bytes())
	})
}

func (s *Service) editGeneral(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		for _, key := range []string{"listen", "log_level", "log_file"} {
			if value, ok := data[key]; ok {
				applyScalar(root, key, value)
			}
		}
		return nil
	})
}

func (s *Service) editScheduling(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		scheduling := configedit.ChildMap(root, "scheduling")
		for _, key := range []string{
			"circuit_threshold",
			"circuit_cooldown",
			"rate_limit_backoff",
			"quota_cooldown",
			"model_lockout",
			"retry_wait",
			"upstream_timeout",
			"sticky_dwell",
			"quota_poll_interval",
			"quota_switch_margin",
			"quality_error_weight",
			"quality_ttft_weight",
		} {
			if value, ok := data[key]; ok {
				applyScalar(scheduling, key, value)
			}
		}
		return nil
	})
}

// editRequestLog / editStats / editCache mutate the scalar request_log, stats
// and cache blocks. The whole request_log and stats blocks are restart-only at
// runtime (logger/stats store are built once at startup), but the write itself
// is the same validate+save+reload pipeline as every other edit — the UI owns
// the "restart required" hint (docs/engineering/pitfalls.md #29/#31).
func (s *Service) editRequestLog(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		requestLog := configedit.ChildMap(root, "request_log")
		for _, key := range []string{"enabled", "dir", "max_file_size", "max_body_bytes", "retention", "mcp_split", "mcp_dir"} {
			if value, ok := data[key]; ok {
				applyScalar(requestLog, key, value)
			}
		}
		return nil
	})
}

func (s *Service) editStats(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		stats := configedit.ChildMap(root, "stats")
		for _, key := range []string{"db_path", "retention"} {
			if value, ok := data[key]; ok {
				applyScalar(stats, key, value)
			}
		}
		return nil
	})
}

// editGuard mutates the guard block: the scalar switches through the same
// applyScalar path as every other form, plus the two user-declared rule
// lists. A present list replaces the sequence wholesale (the UI always sends
// the full edited list); nil deletes the key (revert to none); an absent key
// is left untouched. guard.adjudicate is deliberately NOT editable here —
// it is an explicit opt-in exception (decision 36) and stays YAML-only.
func (s *Service) editGuard(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		guard := configedit.ChildMap(root, "guard")
		for _, key := range []string{"secrets", "paths", "known_secrets", "decode", "audit", "session_scan", "audit_path"} {
			if value, ok := data[key]; ok {
				applyScalar(guard, key, value)
			}
		}
		for _, key := range []string{"extra_patterns", "extra_paths"} {
			value, ok := data[key]
			if !ok {
				continue
			}
			if value == nil {
				configedit.DeleteKey(guard, key)
				continue
			}
			list, ok := value.([]any)
			if !ok {
				return appapi.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("guard %s must be a list", key))
			}
			configedit.SetChildNode(guard, key, configedit.MustEncode(list))
		}
		return nil
	})
}

func (s *Service) editCache(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) error {
		cache := configedit.ChildMap(root, "cache")
		for _, key := range []string{"enabled", "ttl", "max_entries", "max_body_bytes"} {
			if value, ok := data[key]; ok {
				applyScalar(cache, key, value)
			}
		}
		return nil
	})
}

// applyScalar writes a form value as a YAML scalar: nil deletes the key (the
// form's "revert to default" signal — the code default then applies again).
// parent may be the document root or a child mapping.
func applyScalar(parent *yaml.Node, key string, value any) {
	target := parent
	if target != nil && target.Kind == yaml.DocumentNode && len(target.Content) > 0 {
		target = target.Content[0]
	}
	if target == nil {
		return
	}
	if value == nil {
		configedit.DeleteKey(target, key)
		return
	}
	configedit.SetChildScalar(target, key, scalarString(value))
}

// scalarString renders a JSON-decoded form value without float artifacts:
// json numbers arrive as float64, and fmt.Sprint would turn 1073741824 into
// "1.073741824e+09" (which YAML would then store as a string). Integral floats
// become plain integers; everything else keeps its shortest exact form.
func scalarString(value any) string {
	if number, ok := value.(float64); ok {
		if number == math.Trunc(number) && math.Abs(number) < 1e15 {
			return strconv.FormatInt(int64(number), 10)
		}
		return strconv.FormatFloat(number, 'f', -1, 64)
	}
	return fmt.Sprint(value)
}

func (s *Service) editStructured(kind, name string, data map[string]any) error {
	if data["delete"] == true {
		return s.editConfigNode(func(root *yaml.Node) error {
			switch kind {
			case "provider":
				configedit.DeleteKey(configedit.ChildMap(root, "providers"), name)
			case "route":
				configedit.DeleteKey(configedit.ChildMap(root, "routes"), name)
			}
			return nil
		})
	}
	return s.editConfigNode(func(root *yaml.Node) error {
		switch kind {
		case "provider":
			providerConfig := configedit.ChildMap(configedit.ChildMap(root, "providers"), name)
			for _, key := range []string{
				"provider_id",
				"openai_base_url",
				"anthropic_base_url",
				"usage_url",
				"billing",
			} {
				if value, ok := data[key]; ok {
					configedit.SetChildScalar(providerConfig, key, fmt.Sprint(value))
				}
			}
			if models, ok := data["models"]; ok {
				configedit.SetChildNode(providerConfig, "models", configedit.MustEncode(models))
			}
			// catalog_alias: full-map replace (the client sends the provider's
			// complete desired map); an empty map deletes the key so the YAML
			// doesn't retain an empty mapping node.
			if catalogAlias, ok := data["catalog_alias"]; ok {
				if m, isMap := catalogAlias.(map[string]any); isMap && len(m) == 0 {
					configedit.DeleteKey(providerConfig, "catalog_alias")
				} else {
					configedit.SetChildNode(providerConfig, "catalog_alias", configedit.MustEncode(catalogAlias))
				}
			}
		case "route":
			// data.strategy semantics (docs/web-api.md): absent = preserve the
			// existing declaration (a targets-only edit must not reset strategy);
			// null or "" = strip the declaration (back to the default quota);
			// "quota" = explicit declaration, EXCEPT on a route that never
			// declared one (the list form stays untouched — quota IS the
			// default); "load_balance" = declare the rotation strategy.
			targets, hasTargets := data["targets"]
			strategyRaw, hasStrategy := data["strategy"]
			strategy, _ := strategyRaw.(string) // null stays "" = strip
			if !hasTargets && !hasStrategy {
				break
			}
			routes := configedit.ChildMap(root, "routes")
			existing := configedit.ChildNode(routes, name)
			existingStrategy := routeDeclaredStrategy(existing)
			if !hasTargets {
				// Strategy-only edit: rewrite only the strategy declaration and
				// keep the route's targets node verbatim (comments preserved).
				if existing == nil {
					return fmt.Errorf("route %q: not found — add targets first", name)
				}
				configedit.SetChildNode(routes, name, routeNodeWithStrategy(existing, strategy))
				break
			}
			node := configedit.MustEncode(targets)
			// Resolve the declaration the edited route carries.
			declared := existingStrategy
			if hasStrategy {
				declared = strategy
				if strategy == configdomain.RouteStrategyQuota && existingStrategy == "" {
					declared = "" // quota on an undeclared route changes nothing
				}
			}
			if declared != "" {
				mapped := yaml.Node{Kind: yaml.MappingNode}
				configedit.SetChildScalar(&mapped, "strategy", declared)
				configedit.SetChildNode(&mapped, "targets", node)
				node = &mapped
			}
			configedit.SetChildNode(routes, name, node)
		}
		return nil
	})
}

// routeDeclaredStrategy reads a route node's declared scheduling strategy
// ("" for the list form or a missing/empty strategy scalar).
func routeDeclaredStrategy(node *yaml.Node) string {
	if node == nil || node.Kind != yaml.MappingNode {
		return ""
	}
	if s := configedit.ChildNode(node, "strategy"); s != nil && s.Kind == yaml.ScalarNode && s.Value != "" {
		return s.Value
	}
	return ""
}

// routeNodeWithStrategy rewrites ONLY the strategy declaration of an existing
// route node, keeping its targets (and their comments) verbatim: map-form
// routes get the strategy scalar set/removed in place; list-form routes are
// wrapped into the map form around the untouched list.
func routeNodeWithStrategy(existing *yaml.Node, strategy string) *yaml.Node {
	if existing.Kind == yaml.MappingNode {
		if strategy == "" {
			configedit.DeleteKey(existing, "strategy")
		} else {
			configedit.SetChildScalar(existing, "strategy", strategy)
		}
		return existing
	}
	if strategy == "" {
		return existing
	}
	mapped := yaml.Node{Kind: yaml.MappingNode}
	configedit.SetChildScalar(&mapped, "strategy", strategy)
	configedit.SetChildNode(&mapped, "targets", existing)
	return &mapped
}
