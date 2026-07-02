package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookupPricing_EmbeddedFallback(t *testing.T) {
	// With no local override, the embedded table is used. deepseek-v4-pro is in it.
	p := lookupPricing("deepseek-v4-pro")
	if p == nil {
		t.Fatal("expected pricing for deepseek-v4-pro from embedded data")
	}
	if p.DisplayName != "DeepSeek V4 Pro" {
		t.Errorf("display_name=%q", p.DisplayName)
	}
	if p.InputCostPerMillion != "0.435" {
		t.Errorf("input=%q", p.InputCostPerMillion)
	}
}

func TestLookupPricing_UnknownModel(t *testing.T) {
	if lookupPricing("does-not-exist-xyz") != nil {
		t.Error("expected nil for unknown model")
	}
}

func TestLoadPricingTable_LocalOverride(t *testing.T) {
	// Write a local override with a known model, point candidates at it via a
	// temp HOME, and verify loadPricingTable picks it up.
	dir := t.TempDir()
	overridePath := filepath.Join(dir, "models_pricing.json")
	rows := []ModelPricing{{ModelID: "test-model", DisplayName: "Test Model",
		InputCostPerMillion: "1.5", OutputCostPerMillion: "3"}}
	b := []byte(`[{"model_id":"test-model","display_name":"Test Model","input_cost_per_million":"1.5","output_cost_per_million":"3"}]`)
	if err := os.WriteFile(overridePath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	// Swap HOME so homeDir() → dir/.model-proxy. Write to the exact path the
	// loader checks under a temp HOME.
	proxyDir := filepath.Join(dir, ".model-proxy")
	os.MkdirAll(proxyDir, 0o755)
	target := filepath.Join(proxyDir, "models_pricing.json")
	os.WriteFile(target, b, 0o644)

	old := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	defer os.Setenv("HOME", old)

	tbl := loadPricingTable()
	if p := tbl["test-model"]; p.DisplayName != "Test Model" {
		t.Errorf("override not loaded: %+v", p)
	}
	// Embedded data should NOT be present when override is loaded.
	if _, ok := tbl["deepseek-v4-pro"]; ok {
		t.Error("embedded data leaked into override load")
	}
	_ = rows
}

func TestExportPricingFromDB_MissingFile(t *testing.T) {
	_, err := exportPricingFromDB("/nonexistent/path.db")
	if err == nil {
		t.Error("expected error for missing db")
	}
}
