package models

import (
	"bytes"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// loadConfigNode parses a YAML file into a yaml.Node tree for comment-preserving
// edits (the models: sequence rewrite must not reformat unrelated sections).
func loadConfigNode(path string) (*yaml.Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &root, nil
}

// childMap returns the mapping node for key under parent (doc/mapping aware).
func childMap(root *yaml.Node, key string) *yaml.Node {
	if root == nil {
		return nil
	}
	node := root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}

// setChildNode sets key to value in a mapping node (replace or append).
func setChildNode(parent *yaml.Node, key string, value *yaml.Node) {
	if parent == nil {
		return
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
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
	var probe struct {
		Providers map[string]map[string]any `yaml:"providers"`
	}
	if err := yaml.Unmarshal([]byte(data), &probe); err != nil {
		return "", fmt.Errorf("refusing to write invalid YAML: %w", err)
	}
	if prev, readErr := os.ReadFile(configFile); readErr == nil {
		backup = configFile + ".bak"
		_ = os.WriteFile(backup, prev, 0o600)
	}
	if err := os.WriteFile(configFile, []byte(data), 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

var _ = bytes.MinRead // keep bytes import if buffer reuse changes
