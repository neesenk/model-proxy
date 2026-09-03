package admin

import (
	"bytes"
	"fmt"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/configedit"
)

func (s *Service) editConfigNode(mutate func(*yaml.Node)) error {
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
		return s.saveAndReloadUnderLock(configFile, buffer.Bytes())
	})
}

func (s *Service) editGeneral(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) {
		for _, key := range []string{"listen", "log_level", "log_file"} {
			if value, ok := data[key]; ok {
				configedit.SetScalar(root, key, fmt.Sprint(value))
			}
		}
	})
}

func (s *Service) editScheduling(data map[string]any) error {
	return s.editConfigNode(func(root *yaml.Node) {
		scheduling := configedit.ChildMap(root, "scheduling")
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
				configedit.SetChildScalar(scheduling, key, fmt.Sprint(value))
			}
		}
	})
}

func (s *Service) editStructured(kind, name string, data map[string]any) error {
	if data["delete"] == true {
		return s.editConfigNode(func(root *yaml.Node) {
			switch kind {
			case "provider":
				configedit.DeleteKey(configedit.ChildMap(root, "providers"), name)
			case "route":
				configedit.DeleteKey(configedit.ChildMap(root, "routes"), name)
			}
		})
	}
	return s.editConfigNode(func(root *yaml.Node) {
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
		case "route":
			if targets, ok := data["targets"]; ok {
				configedit.SetChildNode(configedit.ChildMap(root, "routes"), name, configedit.MustEncode(targets))
			}
		}
	})
}
