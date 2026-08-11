// Package configedit owns comment-preserving YAML config edits and validated
// atomic writes shared by the Web config editor and the models CLI.
package configedit

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// LoadNode parses a YAML file into a yaml.Node tree for comment-preserving
// edits (a targeted rewrite must not reformat unrelated sections).
func LoadNode(path string) (*yaml.Node, error) {
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

// MapNode returns the top-level mapping node of a parsed document.
func MapNode(root *yaml.Node) *yaml.Node {
	if root == nil || len(root.Content) == 0 {
		return nil
	}
	return root.Content[0]
}

// ScalarNode returns a plain scalar node for value.
func ScalarNode(value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Value: value}
}

// SetScalar sets key to a scalar value in the top-level mapping of root
// (replace or append).
func SetScalar(root *yaml.Node, key, value string) {
	mapping := MapNode(root)
	if mapping == nil {
		return
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content[index+1].Value = value
			return
		}
	}
	mapping.Content = append(mapping.Content, ScalarNode(key), ScalarNode(value))
}

// ChildMap returns the mapping node for key under parent (doc/mapping aware),
// creating and appending an empty mapping when missing.
func ChildMap(root *yaml.Node, key string) *yaml.Node {
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
	mapping.Content = append(mapping.Content, ScalarNode(key), child)
	return child
}

// LookupChildMap returns the existing mapping node for key under parent, or
// nil when absent. Unlike ChildMap it never mutates the tree.
func LookupChildMap(root *yaml.Node, key string) *yaml.Node {
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

// SetChildScalar sets key to a scalar value in a mapping node (replace or
// append).
func SetChildScalar(parent *yaml.Node, key, value string) {
	if parent == nil {
		return
	}
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			parent.Content[index+1].Value = value
			return
		}
	}
	parent.Content = append(parent.Content, ScalarNode(key), ScalarNode(value))
}

// DeleteKey removes key (and its value) from a mapping node when present.
func DeleteKey(mapping *yaml.Node, key string) {
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

// SetChildNode sets key to value in a mapping node (replace or append).
func SetChildNode(parent *yaml.Node, key string, value *yaml.Node) {
	if parent == nil {
		return
	}
	for index := 0; index+1 < len(parent.Content); index += 2 {
		if parent.Content[index].Value == key {
			parent.Content[index+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, ScalarNode(key), value)
}

// MustEncode encodes a Go value as a yaml.Node (panics on impossible types).
func MustEncode(value any) *yaml.Node {
	data, _ := yaml.Marshal(value)
	var node yaml.Node
	_ = yaml.Unmarshal(data, &node)
	if len(node.Content) > 0 {
		return node.Content[0]
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}
}
