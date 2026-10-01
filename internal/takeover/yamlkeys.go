package takeover

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// yamlkeys.go — comment-preserving YAML document editing for takeover
// templates with format: yaml (hermes' ~/.hermes/config.yaml). Clients save
// their own configs with a round-trip YAML library (hermes uses ruamel with
// preserved quotes/comments), so takeover must not strip comments or reorder
// keys either: the document is decoded into a yaml.v3 Node tree, edited in
// place (mapping key set/delete), and encoded back. Untouched subtrees keep
// their original style, comments and order; only the managed keys change.

// loadYAMLDoc decodes data into a DocumentNode whose root is a mapping.
// Empty input (missing/empty config file) yields an empty mapping so the
// rewrite can create the document. A non-mapping top level is fail-closed —
// rewriting it would destroy the file.
func loadYAMLDoc(data []byte) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, fmt.Errorf("not a YAML document")
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top level is not a mapping")
	}
	return &doc, nil
}

func encodeYAMLDoc(doc *yaml.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// yamlMapGet returns the value node for key in mapping m, or nil.
func yamlMapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlMapSet assigns value to key in mapping m: the first matching key is
// replaced in place (key order and the key node's own style survive), later
// duplicate keys are collapsed away (a hand-edited file must not grow
// duplicates on re-takeover), and a missing key appends.
func yamlMapSet(m *yaml.Node, key string, value *yaml.Node) {
	first := -1
	out := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			if first < 0 {
				first = i
				out = append(out, m.Content[i], value)
			}
			continue
		}
		out = append(out, m.Content[i], m.Content[i+1])
	}
	m.Content = out
	if first < 0 {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	}
}

// yamlMapDelete removes every entry named key from mapping m.
func yamlMapDelete(m *yaml.Node, key string) {
	out := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			continue
		}
		out = append(out, m.Content[i], m.Content[i+1])
	}
	m.Content = out
}

// yamlEnsureMapPath navigates root[path...] creating intermediate mappings.
// A non-mapping value in the way is replaced (the template owns that subtree,
// same contract as the JSON editor's dottedMap).
func yamlEnsureMapPath(root *yaml.Node, path []string) *yaml.Node {
	m := root
	for _, k := range path {
		next := yamlMapGet(m, k)
		if next == nil || next.Kind != yaml.MappingNode {
			next = &yaml.Node{Kind: yaml.MappingNode}
			yamlMapSet(m, k, next)
		}
		m = next
	}
	return m
}

// yamlValueNode renders a decoded-YAML-template value (map/slice/scalar from
// the template's yaml.set) as a node tree by re-marshalling it.
func yamlValueNode(v any) (*yaml.Node, error) {
	if n, ok := v.(*yaml.Node); ok {
		return n, nil
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0], nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, nil
}

// yamlSetDotted assigns value at root[path...], creating intermediate maps.
func yamlSetDotted(root *yaml.Node, path []string, value any) error {
	parent := yamlEnsureMapPath(root, path[:len(path)-1])
	n, err := yamlValueNode(value)
	if err != nil {
		return err
	}
	yamlMapSet(parent, path[len(path)-1], n)
	return nil
}

// yamlNestedString walks root along path (mappings only) and returns the
// terminal scalar's value.
func yamlNestedString(root *yaml.Node, path ...string) (string, bool) {
	m := root
	for i, k := range path {
		if m == nil || m.Kind != yaml.MappingNode {
			return "", false
		}
		v := yamlMapGet(m, k)
		if v == nil {
			return "", false
		}
		if i == len(path)-1 {
			if v.Kind != yaml.ScalarNode {
				return "", false
			}
			return v.Value, true
		}
		m = v
	}
	return "", false
}

// mergeMCPIntoYAML folds the gateway MCP surface into one YAML document —
// the same contract as mergeMCPIntoJSON: stale proxy entries (name in the
// generated namespace AND url under this proxy's /mcp/ prefix) are cleaned,
// the current surface is written, user-owned servers are preserved, and an
// explicit empty selection clears only proxy-managed entries.
func mergeMCPIntoYAML(root *yaml.Node, mcp *MCPTemplate, ctx renderContext) error {
	if mcp == nil {
		return nil
	}
	path := strings.Split(ctx.substitute(mcp.JSONPath), ".")
	servers := yamlEnsureMapPath(root, path)
	proxyPrefix := ctx.proxyURL + "/mcp/"
	generatedNames := make(map[string]bool, len(ctx.mcpAll))
	for _, e := range ctx.mcpAll {
		generatedNames[e.Name] = true
	}
	out := servers.Content[:0]
	for i := 0; i+1 < len(servers.Content); i += 2 {
		nameNode, valueNode := servers.Content[i], servers.Content[i+1]
		if generatedNames[nameNode.Value] {
			if u := yamlMapGet(valueNode, "url"); u != nil && strings.HasPrefix(u.Value, proxyPrefix) {
				continue
			}
		}
		out = append(out, nameNode, valueNode)
	}
	servers.Content = out
	for _, e := range ctx.mcp {
		entry := substituteValue(mcp.JSONEntry, func(s string) string { return ctx.substituteMCP(s, e) })
		n, err := yamlValueNode(entry)
		if err != nil {
			return err
		}
		yamlMapSet(servers, e.Name, n)
	}
	return nil
}
