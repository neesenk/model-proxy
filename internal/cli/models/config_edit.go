package models

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"model-proxy/internal/configedit"
)

// YAML node helpers delegate to internal/configedit (shared with the Web
// config editor); these wrappers keep local call sites stable.
func loadConfigNode(path string) (*yaml.Node, error) { return configedit.LoadNode(path) }

// childMap returns the existing mapping node for key under parent
// (doc/mapping aware), or nil. The models rewrite only touches existing
// provider entries, so it never creates mappings.
func childMap(root *yaml.Node, key string) *yaml.Node { return configedit.LookupChildMap(root, key) }

// setChildNode sets key to value in a mapping node (replace or append).
func setChildNode(parent *yaml.Node, key string, value *yaml.Node) {
	configedit.SetChildNode(parent, key, value)
}

// mustEncode encodes a Go value as a yaml.Node (panics on impossible types).
func mustEncode(value any) *yaml.Node {
	var node yaml.Node
	if err := node.Encode(value); err != nil {
		panic(err)
	}
	return &node
}

// writeConfigValidated writes config text after validating it parses, with a
// best-effort .bak of the previous file.
func writeConfigValidated(configFile, data string) (backup string, err error) {
	return configedit.WriteConfigValidated(configFile, data, func(_ string, raw []byte) error {
		var probe struct {
			Providers map[string]map[string]any `yaml:"providers"`
		}
		if err := yaml.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("invalid YAML: %w", err)
		}
		return nil
	})
}

var _ = os.Getenv // keep os import stable if env probing returns
