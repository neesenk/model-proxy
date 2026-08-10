package app

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/configedit"
)

// YAML node helpers delegate to internal/configedit; these thin wrappers keep
// root web/test call sites stable while the web API migrates behind appapi.
func loadConfigNode(path string) (*yaml.Node, error) { return configedit.LoadNode(path) }
func mapNode(root *yaml.Node) *yaml.Node             { return configedit.MapNode(root) }
func scalarNode(value string) *yaml.Node             { return configedit.ScalarNode(value) }
func setScalar(root *yaml.Node, key, value string)   { configedit.SetScalar(root, key, value) }
func childMap(root *yaml.Node, key string) *yaml.Node {
	return configedit.ChildMap(root, key)
}
func setChildScalar(parent *yaml.Node, key, value string) {
	configedit.SetChildScalar(parent, key, value)
}
func deleteKey(mapping *yaml.Node, key string) { configedit.DeleteKey(mapping, key) }
func setChildNode(parent *yaml.Node, key string, value *yaml.Node) {
	configedit.SetChildNode(parent, key, value)
}
func mustEncode(value any) *yaml.Node { return configedit.MustEncode(value) }

func (api *proxyWebAPI) editConfigNode(mutate func(*yaml.Node)) error {
	root, err := loadConfigNode(api.currentConfigFile())
	if err != nil {
		return err
	}
	if mapNode(root) == nil {
		return fmt.Errorf("config is not a YAML mapping")
	}
	mutate(root)
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(root); err != nil {
		return err
	}
	if err := encoder.Close(); err != nil {
		return err
	}
	return api.saveAndReload(buffer.Bytes())
}

func (api *proxyWebAPI) editGeneral(data map[string]any) error {
	return api.editConfigNode(func(root *yaml.Node) {
		for _, key := range []string{"listen", "log_level", "log_file"} {
			if value, ok := data[key]; ok {
				setScalar(root, key, fmt.Sprint(value))
			}
		}
	})
}

func (api *proxyWebAPI) editScheduling(data map[string]any) error {
	return api.editConfigNode(func(root *yaml.Node) {
		scheduling := childMap(root, "scheduling")
		for _, key := range []string{
			"circuit_threshold",
			"circuit_cooldown",
			"rate_limit_backoff",
			"upstream_timeout",
			"sticky_dwell",
			"quota_poll_interval",
			"quota_switch_margin",
		} {
			if value, ok := data[key]; ok {
				setChildScalar(scheduling, key, fmt.Sprint(value))
			}
		}
	})
}

func (api *proxyWebAPI) editStructured(kind, name string, data map[string]any) error {
	if data["delete"] == true {
		return api.editConfigNode(func(root *yaml.Node) {
			switch kind {
			case "provider":
				deleteKey(childMap(root, "providers"), name)
			case "route":
				deleteKey(childMap(root, "routes"), name)
			case "claude_mapping":
				if alias, ok := data["alias"].(string); ok {
					deleteKey(childMap(root, "claude_mapping"), alias)
				}
			}
		})
	}
	return api.editConfigNode(func(root *yaml.Node) {
		switch kind {
		case "provider":
			providerConfig := childMap(childMap(root, "providers"), name)
			for _, key := range []string{
				"provider_id",
				"openai_base_url",
				"anthropic_base_url",
				"usage_url",
				"billing",
			} {
				if value, ok := data[key]; ok {
					setChildScalar(providerConfig, key, fmt.Sprint(value))
				}
			}
			if models, ok := data["models"]; ok {
				setChildNode(providerConfig, "models", mustEncode(models))
			}
		case "route":
			if targets, ok := data["targets"]; ok {
				setChildNode(childMap(root, "routes"), name, mustEncode(targets))
			}
		case "claude_mapping":
			alias, aliasOK := data["alias"].(string)
			route, routeOK := data["route"].(string)
			if aliasOK && routeOK {
				setChildScalar(childMap(root, "claude_mapping"), alias, route)
			}
		}
	})
}
