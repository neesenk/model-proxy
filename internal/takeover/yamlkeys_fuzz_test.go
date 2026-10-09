package takeover

import (
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

// FuzzYAMLSetGetRoundTrip pins the config editor against hostile client
// YAML: for any document loadYAMLDoc accepts, setting a dotted path and
// re-encoding must (a) still load back, (b) return exactly the value that
// was set, and (c) preserve every top-level key the document had — takeover
// rewrites real user config files and must never corrupt what it does not
// own. Non-mapping documents are rejected by loadYAMLDoc before any edit.
func FuzzYAMLSetGetRoundTrip(f *testing.F) {
	seeds := []string{
		`model: opus\n`,
		"a:\n  b: 1\n  c: [x, y]\nz: \"quoted\"\n",
		"multi: |\n  line1\n  line2\n",
		"num: 1.5\nbool: true\nnull_key: null\n",
		"# comment only\n",
		"",
		"fuzznode:\n  existing: keep\n",
		"weird key with spaces: v\n'quoted.key': v\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s), "seg", "value")
	}
	f.Fuzz(func(t *testing.T, data []byte, seg, value string) {
		if len(seg) > 64 {
			seg = seg[:64]
		}
		doc, err := loadYAMLDoc(data)
		if err != nil {
			return
		}
		root := doc.Content[0]
		before := topLevelYAMLKeys(root)
		path := []string{"fuzznode", seg}
		if err := yamlSetDotted(root, path, value); err != nil {
			return
		}
		// Container values carry the fuzz string NESTED inside a map/slice:
		// yaml.Marshal's block-scalar corner is lossy for newline-only strings
		// and the walk (yamlMakeStringsSafe) must fix it at any depth, not
		// just for top-level scalars.
		container := map[string]any{
			"inner": value,
			"list":  []any{value},
			"deep":  map[string]any{"leaf": value},
		}
		if err := yamlSetDotted(root, []string{"fuzznode2", seg}, container); err != nil {
			return
		}
		out, err := encodeYAMLDoc(doc)
		if err != nil {
			t.Fatalf("encodeYAMLDoc failed after set: %v", err)
		}
		doc2, err := loadYAMLDoc(out)
		if err != nil {
			t.Fatalf("edited document no longer loads: %v\nedited YAML:\n%s", err, out)
		}
		got, ok := yamlNestedString(doc2.Content[0], path...)
		if !ok || got != value {
			t.Fatalf("round-trip lost the set value: got (%q, %v), want %q\nedited YAML:\n%s", got, ok, value, out)
		}
		setNode := yamlMapGet(yamlMapGet(doc2.Content[0], "fuzznode2"), seg)
		if setNode == nil {
			t.Fatalf("container value missing after reload\nedited YAML:\n%s", out)
		}
		var gotContainer any
		if err := setNode.Decode(&gotContainer); err != nil {
			t.Fatalf("container value no longer decodes: %v\nedited YAML:\n%s", err, out)
		}
		if !reflect.DeepEqual(gotContainer, container) {
			t.Fatalf("nested container round-trip drifted:\ngot  %#v\nwant %#v\nedited YAML:\n%s", gotContainer, container, out)
		}
		after := topLevelYAMLKeys(doc2.Content[0])
		for k := range before {
			if _, present := after[k]; !present {
				t.Fatalf("edit dropped top-level key %q\nedited YAML:\n%s", k, out)
			}
		}
		if _, present := after["fuzznode"]; !present {
			t.Fatalf("edit did not create top-level key fuzznode\nedited YAML:\n%s", out)
		}
	})
}

func topLevelYAMLKeys(m *yaml.Node) map[string]bool {
	keys := make(map[string]bool)
	if m == nil || m.Kind != yaml.MappingNode {
		return keys
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		keys[m.Content[i].Value] = true
	}
	return keys
}
