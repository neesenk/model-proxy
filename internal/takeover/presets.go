package takeover

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed presets/*.yaml
var presetsFS embed.FS

// presetTemplates parses every embedded preset, keyed by template name.
func presetTemplates() (map[string]*Template, error) {
	entries, err := fs.ReadDir(presetsFS, "presets")
	if err != nil {
		return nil, fmt.Errorf("read embedded presets: %w", err)
	}
	out := map[string]*Template{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".yaml")
		data, err := presetsFS.ReadFile("presets/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("preset %s: %w", name, err)
		}
		t, err := ParseTemplate(name, "preset", data)
		if err != nil {
			return nil, err
		}
		out[name] = t
	}
	return out, nil
}

func sortTemplates(ts []*Template) {
	sort.Slice(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
