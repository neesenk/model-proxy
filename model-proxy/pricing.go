package main

import (
	_ "embed"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// ModelPricing mirrors a row of AIS Switch's model_pricing table.
type ModelPricing struct {
	ModelID                  string `json:"model_id"`
	DisplayName              string `json:"display_name"`
	InputCostPerMillion      string `json:"input_cost_per_million"`
	OutputCostPerMillion     string `json:"output_cost_per_million"`
	CacheReadCostPerMillion  string `json:"cache_read_cost_per_million"`
	CacheCreationCostPerMillion string `json:"cache_creation_cost_per_million"`
}

// embeddedPricing is the default pricing table shipped with the binary, exported
// from AIS Switch's cc-switch.db model_pricing table (147 default rows seeded by
// the desktop app at schema init). Runtime does NOT read cc-switch.db; refresh it
// with `model-proxy import-pricing`.
//
//go:embed data/models_pricing.json
var embeddedPricing []byte

// pricingTable is the in-memory lookup: model_id → ModelPricing.
var pricingTable map[string]ModelPricing

func init() {
	pricingTable = loadPricingTable()
}

// loadPricingTable loads pricing from a local models_pricing.json (written by
// import-pricing) if present, falling back to the embedded default. Runtime
// never touches cc-switch.db. Silent on success; logs only on failure.
func loadPricingTable() map[string]ModelPricing {
	t := make(map[string]ModelPricing)
	// Try local override next to the config / cache dir.
	for _, p := range localPricingCandidates() {
		if b, err := os.ReadFile(p); err == nil {
			var rows []ModelPricing
			if err := json.Unmarshal(b, &rows); err == nil {
				for _, r := range rows {
					t[r.ModelID] = r
				}
				return t
			}
		}
	}
	// Fallback: embedded default.
	var rows []ModelPricing
	if err := json.Unmarshal(embeddedPricing, &rows); err != nil {
		log.Printf("[pricing] warn: failed to parse embedded pricing: %v", err)
		return t
	}
	for _, r := range rows {
		t[r.ModelID] = r
	}
	return t
}

// localPricingCandidates returns local paths to check for an imported pricing
// file, in priority order. The home-dir path is where import-pricing writes,
// so it goes first.
func localPricingCandidates() []string {
	var out []string
	home := homeDir()
	if home != "" {
		out = append(out, filepath.Join(home, ".model-proxy", "models_pricing.json"))
	}
	// Next to the config file (secondary).
	if abs, err := filepath.Abs("config.yaml"); err == nil {
		out = append(out, filepath.Join(filepath.Dir(abs), "models_pricing.json"))
	}
	return out
}

// lookupPricing returns pricing metadata for a model, or nil if unknown.
func lookupPricing(modelID string) *ModelPricing {
	if p, ok := pricingTable[modelID]; ok {
		return &p
	}
	return nil
}

// pricingDataPath is where import-pricing writes: next to the models cache dir.
func pricingDataPath(cfg *Config) string {
	// Write next to the models cache (same dir as sso_cookie_file by default),
	// so loadPricingTable picks it up at runtime.
	if cfg != nil && cfg.LogFile != "" {
		return filepath.Join(filepath.Dir(expandPath(cfg.LogFile)), "models_pricing.json")
	}
	home := homeDir()
	if home != "" {
		return filepath.Join(home, ".model-proxy", "models_pricing.json")
	}
	return "models_pricing.json"
}
