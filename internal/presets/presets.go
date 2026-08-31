// Package presets owns the provider preset catalog as a pure config-level
// concern: the catalog DERIVED from the annotated built-in template
// (configdomain.DefaultConfigYAML — the single authoritative source for
// provider endpoints and default models), the merge of a template provider
// block into a user config.yaml (via configedit, preserving structure), and
// the implicit-routing ambiguity computation. Both serving surfaces build on
// it: the CLI `add` command (internal/cli/presets, which adds login + reload
// orchestration) and the Web preset endpoints (internal/app via appapi).
package presets

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/provider"
)

// Preset is one catalog entry: a ready-to-use config providers block.
type Preset struct {
	// Name is the preset id AND the config providers: key.
	Name       string   `json:"name"`
	ProviderID string   `json:"provider_id"`
	BaseURL    string   `json:"base_url"`
	UsageURL   string   `json:"usage_url,omitempty"`
	Billing    string   `json:"billing,omitempty"`
	Models     []string `json:"models"`
}

// excludedPresets are template entries that must not surface in the public
// catalog: aqp's SSO mint endpoint is internal-network only (design doc
// design-s3-preset-wizard.md §3.1). Everything else with a registered
// implementation is listed.
var excludedPresets = map[string]bool{"aqp": true}

// List derives the catalog from the built-in annotated template, sorted by
// name. Returns an error if the shipped template itself fails to load — that
// is a build-time defect, never user input.
func List() ([]Preset, error) {
	tpl, err := loadTemplate()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(tpl.Providers))
	for name := range tpl.Providers {
		if !excludedPresets[name] && provider.IsRegistered(tpl.Providers[name].Provider) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]Preset, 0, len(names))
	for _, name := range names {
		p := tpl.Providers[name]
		models := append([]string(nil), p.Models...)
		sort.Strings(models)
		out = append(out, Preset{
			Name:       name,
			ProviderID: p.Provider,
			BaseURL:    p.OpenAIBaseURL,
			UsageURL:   p.UsageURL,
			Billing:    p.Billing,
			Models:     models,
		})
	}
	return out, nil
}

func loadTemplate() (*configdomain.Config, error) {
	tpl, err := configdomain.LoadConfigFromBytes("config.yaml", []byte(configdomain.DefaultConfigYAML))
	if err != nil {
		return nil, fmt.Errorf("built-in config template is invalid: %w", err)
	}
	return tpl, nil
}

// Lookup resolves one preset by name; ok=false when absent or unregistered.
func Lookup(name string) (configdomain.Provider, bool) {
	tpl, err := loadTemplate()
	if err != nil {
		return configdomain.Provider{}, false
	}
	p, ok := tpl.Providers[name]
	if !ok || !provider.IsRegistered(p.Provider) {
		return configdomain.Provider{}, false
	}
	return p, true
}

// MergeBlock copies the template's providers.<preset> node into the user
// config (creating the providers: mapping when missing), validates the merged
// YAML before writing (fail-closed), and writes back preserving file mode.
// Idempotent: an existing block is left untouched. Returns whether a write
// happened.
func MergeBlock(cfgPath, presetName string) (bool, error) {
	// Preview + write run as ONE locked read-modify-write: the daemon's web
	// config editor RMWs the same config.yaml (from another process for the
	// CLI `add` path) — an unlocked preview-then-write would let a concurrent
	// writer's change be silently discarded by the rename.
	var changed bool
	err := configedit.WithConfigLock(cfgPath, func() error {
		merged, merr := []byte(nil), error(nil)
		changed, merged, merr = PreviewMergeBlock(cfgPath, presetName)
		if merr != nil || !changed {
			return merr
		}
		return writeConfigAtomic(cfgPath, merged)
	})
	return changed, err
}

// PreviewMergeBlock is the dry-run form of MergeBlock: the SAME merge and
// validation, returned as the would-be config content without touching the
// file. changed=false (merged nil) means the preset block already exists and
// MergeBlock would be a no-op. Callers that gate on the merged result (the
// CLI add ambiguity gate) preview first, then persist via MergeBlock only
// after the gate passes — a refusal must leave config.yaml byte-identical.
func PreviewMergeBlock(cfgPath, presetName string) (changed bool, merged []byte, err error) {
	userRoot, err := configedit.LoadNode(cfgPath)
	if err != nil {
		return false, nil, err
	}
	if configedit.MapNode(userRoot) == nil {
		return false, nil, errors.New("config is not a YAML mapping")
	}
	providersMap := configedit.ChildMap(userRoot, "providers")

	existing := configedit.LookupChildMap(providersMap, presetName)
	if existing != nil {
		return false, nil, nil // already configured — nothing to merge
	}

	tplNode, err := loadTemplateProviderNode(presetName)
	if err != nil {
		return false, nil, err
	}
	configedit.SetChildNode(providersMap, presetName, tplNode)

	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(userRoot); err != nil {
		return true, nil, err
	}
	if err := encoder.Close(); err != nil {
		return true, nil, err
	}
	// Validate: a merged config that fails to load must never hit disk
	// (fail-closed) — MergeBlock writes only what passed this check.
	if _, verr := configdomain.LoadConfigFromBytes(cfgPath, buffer.Bytes()); verr != nil {
		return true, nil, fmt.Errorf("merged config invalid: %w", verr)
	}
	return true, buffer.Bytes(), nil
}

// writeConfigAtomic persists merged config content via a same-directory temp
// file + rename, preserving the original file mode.
func writeConfigAtomic(cfgPath string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, serr := os.Stat(cfgPath); serr == nil {
		mode = info.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(cfgPath), "."+filepath.Base(cfgPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		tmp.Close()
		os.Remove(tmpName)
		return werr
	}
	if cerr := tmp.Close(); cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	if cerr := os.Chmod(tmpName, mode); cerr != nil {
		os.Remove(tmpName)
		return cerr
	}
	return os.Rename(tmpName, cfgPath)
}

// loadTemplateProviderNode extracts providers.<preset> from the built-in
// template as a deep-copied yaml.Node (round-trip through bytes — yaml.Node
// has no Clone). The node carries the template's structure for that block.
func loadTemplateProviderNode(presetName string) (*yaml.Node, error) {
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(configdomain.DefaultConfigYAML), &root); err != nil {
		return nil, fmt.Errorf("parse built-in template: %w", err)
	}
	mapping := root.Content[0]
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != "providers" || mapping.Content[i+1].Kind != yaml.MappingNode {
			continue
		}
		providers := mapping.Content[i+1]
		for j := 0; j+1 < len(providers.Content); j += 2 {
			if providers.Content[j].Value == presetName {
				block := providers.Content[j+1]
				encoded, err := yaml.Marshal(block)
				if err != nil {
					return nil, err
				}
				var clone yaml.Node
				if err := yaml.Unmarshal(encoded, &clone); err != nil {
					return nil, err
				}
				return clone.Content[0], nil
			}
		}
	}
	return nil, fmt.Errorf("preset %q not found in built-in template", presetName)
}

// AmbiguousModels returns the preset's models that appear in OTHER configured
// providers' model lists while no explicit routes: entry maps them (implicit
// routing would pick whichever provider sorts first).
func AmbiguousModels(merged *configdomain.Config, presetName string) []string {
	target, ok := merged.Providers[presetName]
	if !ok {
		return nil
	}
	others := map[string]bool{}
	for name, p := range merged.Providers {
		if name == presetName {
			continue
		}
		for _, m := range p.Models {
			others[m] = true
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range target.Models {
		if others[m] && !seen[m] {
			seen[m] = true
			if _, routed := merged.Routes[m]; !routed {
				out = append(out, m)
			}
		}
	}
	sort.Strings(out)
	return out
}

// Names renders the sorted catalog ids (help/usage strings).
func Names(catalog []Preset) string {
	out := make([]string, len(catalog))
	for i, p := range catalog {
		out[i] = p.Name
	}
	return strings.Join(out, "|")
}
