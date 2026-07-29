package main

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

func loadConfigNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	return &root, nil
}

func mapNode(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	return root.Content[0]
}

func scalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

func setScalar(root *yaml.Node, key, value string) {
	mapping := mapNode(root)
	if mapping == nil {
		return
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content, scalarNode(key), scalarNode(value))
}

func childMap(root *yaml.Node, key string) *yaml.Node {
	mapping := root
	if root != nil && root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		mapping = root.Content[0]
	}
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key &&
			mapping.Content[index+1].Kind == yaml.MappingNode {
			return mapping.Content[index+1]
		}
	}
	child := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content, scalarNode(key), child)
	return child
}

func setChildScalar(parent *yaml.Node, key, value string) {
	if parent == nil {
		return
	}
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			parent.Content[index+1].Value = value
			return
		}
	}
	parent.Content = append(parent.Content, scalarNode(key), scalarNode(value))
}

func deleteKey(mapping *yaml.Node, key string) {
	if mapping == nil {
		return
	}
	out := mapping.Content[:0]
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			continue
		}
		out = append(out, mapping.Content[index], mapping.Content[index+1])
	}
	mapping.Content = out
}

func setChildNode(parent *yaml.Node, key string, value *yaml.Node) {
	if parent == nil {
		return
	}
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			parent.Content[index+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, scalarNode(key), value)
}

func mustEncode(value any) *yaml.Node {
	data, _ := yaml.Marshal(value)
	var node yaml.Node
	_ = yaml.Unmarshal(data, &node)
	if len(node.Content) > 0 {
		return node.Content[0]
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
}

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
