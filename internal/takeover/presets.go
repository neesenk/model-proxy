package takeover

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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
		parsed, err := ParseTemplate(name, "preset", data)
		if err != nil {
			return nil, err
		}
		for _, t := range parsed {
			out[t.Name] = t
		}
	}
	return out, nil
}

// PresetTemplateYAML returns the raw embedded YAML document for one preset
// template (the Web admin template editor shows/overrides the verbatim
// document, not the parsed struct). Unknown names report "unknown template".
func PresetTemplateYAML(name string) ([]byte, error) {
	data, err := presetsFS.ReadFile("presets/" + name + ".yaml")
	if err != nil {
		return nil, fmt.Errorf("unknown takeover template %q", name)
	}
	return data, nil
}

// PresetTemplateDocYAML resolves the raw embedded document CONTAINING one
// template — for a variant declared inside a merged variants: document
// (opencode-openai lives in opencode.yaml), that document is what the
// template editor should show/save; a plain document resolves to itself.
func PresetTemplateDocYAML(name string) ([]byte, error) {
	if data, err := presetsFS.ReadFile("presets/" + name + ".yaml"); err == nil {
		return data, nil
	}
	entries, err := fs.ReadDir(presetsFS, "presets")
	if err != nil {
		return nil, fmt.Errorf("read embedded presets: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := presetsFS.ReadFile("presets/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("preset %s: %w", strings.TrimSuffix(e.Name(), ".yaml"), err)
		}
		parsed, err := ParseTemplate(strings.TrimSuffix(e.Name(), ".yaml"), "preset", data)
		if err != nil {
			return nil, err
		}
		for _, t := range parsed {
			if t.Name == name {
				return data, nil
			}
		}
	}
	return nil, fmt.Errorf("unknown takeover template %q", name)
}

// UserTemplateDocYAML resolves the user-override document CONTAINING one
// template, mirroring PresetTemplateDocYAML for the user templates dir.
// Second return value is the document path ("" when absent).
func UserTemplateDocYAML(templatesDir, name string) ([]byte, string, error) {
	if data, err := os.ReadFile(filepath.Join(templatesDir, name+".yaml")); err == nil {
		return data, filepath.Join(templatesDir, name+".yaml"), nil
	}
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			return nil, "", err
		}
		parsed, err := ParseTemplate(strings.TrimSuffix(e.Name(), ".yaml"), "user", data)
		if err != nil {
			return nil, "", err
		}
		for _, t := range parsed {
			if t.Name == name {
				return data, filepath.Join(templatesDir, e.Name()), nil
			}
		}
	}
	return nil, "", nil
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
