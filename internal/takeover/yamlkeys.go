package takeover

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

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

// yamlScalarText returns a scalar node's logical text. yaml.v3 encodes
// strings with invalid UTF-8 as !!binary and then carries the base64 TEXT in
// Value — reading or matching such scalars by Value alone loses the original
// bytes (found by FuzzYAMLSetGetRoundTrip). Decode binary-tagged scalars so
// set→encode→reload→get round-trips arbitrary strings.
func yamlScalarText(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!binary" {
		if raw, err := base64.StdEncoding.DecodeString(n.Value); err == nil {
			return string(raw)
		}
	}
	return n.Value
}

// yamlMapGet returns the value node for key in mapping m, or nil.
func yamlMapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if yamlScalarText(m.Content[i]) == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// yamlSafeScalar builds a scalar node for an arbitrary string whose
// encode→reload is lossless. yaml.v3's default style picks a literal block
// for newline-only strings (`|4+`), whose reparse DROPS the newline — force
// double quotes (escape-complete) for valid-UTF-8 strings that contain
// newlines. Invalid UTF-8 keeps the default style: the encoder emits
// !!binary and yamlScalarText decodes it back on read. Everything else
// keeps the pre-existing tagless node, so rendered output is unchanged.
func yamlSafeScalar(s string) *yaml.Node {
	n := &yaml.Node{Kind: yaml.ScalarNode, Value: s}
	if utf8.ValidString(s) && strings.ContainsAny(s, "\n\r") {
		n.Style = yaml.DoubleQuotedStyle
	}
	return n
}

// yamlMapSet assigns value to key in mapping m: the first matching key is
// replaced in place (key order and the key node's own style survive), later
// duplicate keys are collapsed away (a hand-edited file must not grow
// duplicates on re-takeover), and a missing key appends.
func yamlMapSet(m *yaml.Node, key string, value *yaml.Node) {
	first := -1
	out := m.Content[:0]
	for i := 0; i+1 < len(m.Content); i += 2 {
		if yamlScalarText(m.Content[i]) == key {
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
		m.Content = append(m.Content, yamlSafeScalar(key), value)
	}
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

// yamlMakeStringsSafe walks a freshly marshalled node tree and forces
// double-quoted style on every string scalar containing newlines — including
// ones nested inside maps/slices and mapping KEYS. Container values normally
// bypass yaml.Marshal entirely (goToYAMLNode), so this only guards the Marshal
// fallback path for scalar types it still serves.
func yamlMakeStringsSafe(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" &&
		utf8.ValidString(n.Value) && strings.ContainsAny(n.Value, "\n\r") {
		n.Style = yaml.DoubleQuotedStyle
	}
	for _, c := range n.Content {
		yamlMakeStringsSafe(c)
	}
}

// marshalScalarNode renders one scalar through yaml.Marshal and re-parses it
// into a node: Marshal makes the quoting decision (strings that would
// otherwise reparse as numbers/bools/null get quoted — "0" must stay a
// string), which direct node construction cannot know.
func marshalScalarNode(v any) (*yaml.Node, error) {
	b, err := yaml.Marshal(v)
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		out := doc.Content[0]
		yamlMakeStringsSafe(out)
		return out, nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null"}, nil
}

// yamlStringNode builds a scalar node for one template string whose
// encode→reload is lossless AND type-faithful: newline-bearing strings force
// double quotes (yamlSafeScalar — Marshal's block-scalar corner loses the
// newline on reparse); everything else delegates the quoting decision to
// yaml.Marshal.
func yamlStringNode(s string) (*yaml.Node, error) {
	if utf8.ValidString(s) && strings.ContainsAny(s, "\n\r") {
		return yamlSafeScalar(s), nil
	}
	return marshalScalarNode(s)
}

// goToYAMLNode builds a node tree from a decoded template value without
// round-tripping yaml.Marshal for containers: Marshal itself is lossy for
// pure-newline strings nested in maps/slices (it emits a literal block whose
// reparse drops the newline — found by FuzzYAMLSetGetRoundTrip), so the two
// decoded container shapes (map[string]any / []any) construct nodes directly,
// routing every string (values AND keys) through yamlStringNode. Map keys are
// sorted to match Marshal's deterministic ordering (rendered output must be
// byte-stable across renders). Non-string scalars fall back to a single-value
// Marshal, which is lossless for them.
func goToYAMLNode(v any) (*yaml.Node, error) {
	switch t := v.(type) {
	case string:
		return yamlStringNode(t)
	case map[string]any:
		m := &yaml.Node{Kind: yaml.MappingNode}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			keyNode, err := yamlStringNode(k)
			if err != nil {
				return nil, err
			}
			value, err := goToYAMLNode(t[k])
			if err != nil {
				return nil, err
			}
			m.Content = append(m.Content, keyNode, value)
		}
		return m, nil
	case []any:
		s := &yaml.Node{Kind: yaml.SequenceNode}
		for _, e := range t {
			value, err := goToYAMLNode(e)
			if err != nil {
				return nil, err
			}
			s.Content = append(s.Content, value)
		}
		return s, nil
	default:
		return marshalScalarNode(v)
	}
}

// yamlValueNode renders a decoded-YAML-template value (map/slice/scalar from
// the template's yaml.set) as a node tree. Strings at ANY depth take the
// yamlSafeScalar path: yaml.Marshal's block-scalar corner is lossy for
// pure-newline strings (see yamlSafeScalar), including nested inside
// containers.
func yamlValueNode(v any) (*yaml.Node, error) {
	if n, ok := v.(*yaml.Node); ok {
		return n, nil
	}
	return goToYAMLNode(v)
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
			return yamlScalarText(v), true
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
