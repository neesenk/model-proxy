package takeover

import (
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
