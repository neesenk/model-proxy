// Package presets owns the provider preset catalog as a pure config-level
// concern: the catalog DERIVED from the annotated built-in template
// (configdomain.DefaultConfigYAML — the single authoritative source for
// provider endpoints and default models), the merge of a template provider
// entry into the user's provider store (provider.json, the single provider
// authority), and the implicit-routing ambiguity computation. Both serving
// surfaces build on it: the CLI `add` command (internal/cli/presets, which
// adds login + reload orchestration) and the Web preset endpoints
// (internal/app via appapi).
package presets

import (
	"fmt"
	"os"
	"sort"
	"strings"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerstore"
)

// Preset is one catalog entry: a ready-to-use config providers block.
type Preset struct {
	// Name is the preset id AND the config providers: key.
	Name             string   `json:"name"`
	ProviderID       string   `json:"provider_id"`
	BaseURL          string   `json:"base_url"`
	AnthropicBaseURL string   `json:"anthropic_base_url,omitempty"`
	UsageURL         string   `json:"usage_url,omitempty"`
	Billing          string   `json:"billing,omitempty"`
	Priority         int      `json:"priority,omitempty"`
	Models           []string `json:"models"`
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
		baseURL := p.OpenAIBaseURL
		if baseURL == "" {
			baseURL = p.DecisionsBaseURL // pure-decisions provider (typesafe)
		}
		out = append(out, Preset{
			Name:             name,
			ProviderID:       p.Provider,
			BaseURL:          baseURL,
			AnthropicBaseURL: p.AnthropicBaseURL,
			UsageURL:         p.UsageURL,
			Billing:          p.Billing,
			Priority:         p.Priority,
			Models:           models,
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

// MergeBlock copies the preset's template provider entry into the provider
// store (the single provider authority), validating the resulting effective
// config before writing (fail-closed — providerstore.Mutate enforces this).
// Idempotent: an existing store entry is left untouched. Returns whether a
// write happened.
func MergeBlock(cfgPath, storePath, presetName string) (bool, error) {
	var changed bool
	err := providerstore.Mutate(cfgPath, storePath, func(providers map[string]configdomain.Provider) error {
		if _, exists := providers[presetName]; exists {
			return nil // already configured — nothing to merge
		}
		tpl, ok := Lookup(presetName)
		if !ok {
			return fmt.Errorf("unknown preset %q", presetName)
		}
		providers[presetName] = tpl
		changed = true
		return nil
	})
	return changed, err
}

// PreviewMergeBlock is the dry-run form of MergeBlock: the SAME store merge
// and effective-config validation, returned as the would-be merged config
// without touching disk. changed=false means the preset entry already exists
// and MergeBlock would be a no-op. Callers that gate on the merged result
// (the CLI add ambiguity gate) preview first, then persist via MergeBlock
// only after the gate passes — a refusal must leave the store untouched.
func PreviewMergeBlock(cfgPath, storePath, presetName string) (changed bool, merged *configdomain.Config, err error) {
	effective, err := providerstore.Resolve(cfgPath, storePath)
	if err != nil {
		return false, nil, err
	}
	proposed := effective
	if _, exists := proposed[presetName]; !exists {
		tpl, ok := Lookup(presetName)
		if !ok {
			return false, nil, fmt.Errorf("unknown preset %q", presetName)
		}
		proposed = providerstore.Clone(effective)
		proposed[presetName] = tpl
		changed = true
	}
	data, rerr := os.ReadFile(cfgPath)
	if rerr != nil {
		return false, nil, rerr
	}
	// Validate: a merged set that fails to load must never reach the store
	// (fail-closed) — MergeBlock writes only what passed this check.
	cfg, verr := configdomain.LoadConfigEffective(cfgPath, data, proposed)
	if verr != nil {
		return changed, nil, fmt.Errorf("merged config invalid: %w", verr)
	}
	return changed, cfg, nil
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
