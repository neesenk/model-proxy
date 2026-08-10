package main

import (
	"model-proxy/provider"
	"testing"

	cliframework "model-proxy/internal/cli/framework"

	climodels "model-proxy/internal/cli/models"
)

// --- pad edge cases (models.go) ---

func TestPad_AlreadyLong(t *testing.T) {
	if got := provider.Pad("toolongalready", 5); got != "toolongalready" {
		t.Errorf("provider.Pad(long,5)=%q want passthrough", got)
	}
}

// --- nonFlagArgs: --config= form + flags interspersed ---

func TestNonFlagArgs_ConfigEquals(t *testing.T) {
	got := climodels.NonFlagArgs([]string{"--config=x.yaml", "models", "zhipu"})
	if len(got) != 2 || got[0] != "models" || got[1] != "zhipu" {
		t.Errorf("climodels.NonFlagArgs(--config=)=%v want [models zhipu]", got)
	}
}

// --- routeNames sorting (already tested, but check empty) ---

func TestRouteNames_Sorted(t *testing.T) {
	cfg := &Config{Routes: map[string][]RouteTarget{
		"zeta":  {{Provider: "a", Model: "z"}},
		"alpha": {{Provider: "a", Model: "a"}},
		"mid":   {{Provider: "a", Model: "m"}},
	}}
	got := cliframework.RouteNames(cfg)
	want := "alpha, mid, zeta"
	if got != want {
		t.Errorf("routeNames=%q want %q (sorted)", got, want)
	}
}

// --- isVolcengineModelFiltered: moved to provider/probe_test.go
// (TestVolcengineFilterModelIDs) - the filter rules now live with the
// VolcengineProvider implementation, tested there directly. ---
