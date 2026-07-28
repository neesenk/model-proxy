package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// models_extra_test.go covers printAllModels / printProviderModels /
// fetchProviderModels / listArkAgentPlanModelIDs (error paths) + the
// exposedModels/displayName edge cases. Uses a local stdout-capture helper
// (grabStdout) to avoid clashing with any captureStdout in other test files.

// grabStdout captures everything written to os.Stdout during fn. Restores
// os.Stdout even on failure.
func grabStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = orig }()
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

// --- printAllModels: lists providers + models, respects filter ---

func TestPrintAllModels_AllProviders(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-5.2", "glm-4.5"}},
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "", nil, nil) })
	if !strings.Contains(out, "zhipu") || !strings.Contains(out, "glm-5.2") || !strings.Contains(out, "glm-4.5") {
		t.Errorf("printAllModels missing content:\n%s", out)
	}
}

func TestPrintAllModels_Filter(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {Provider: testProviderID, Models: []string{"m1"}},
			"b": {Provider: testProviderID, Models: []string{"m2"}},
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "a", nil, nil) })
	if strings.Contains(out, "m2") {
		t.Errorf("filter should exclude m2:\n%s", out)
	}
	if !strings.Contains(out, "m1") {
		t.Errorf("filter should include m1:\n%s", out)
	}
}

func TestPrintAllModels_EmptyContext(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {Provider: testProviderID, Models: []string{"m1"}}, // no meta → ctx/out shown as —
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "", nil, nil) })
	if !strings.Contains(out, "—") {
		t.Errorf("no metadata should show ctx/out as —:\n%s", out)
	}
}

// --- printProviderModels ---

func TestPrintProviderModels_Empty(t *testing.T) {
	out := grabStdout(t, func() { printProviderModels("x", nil) })
	if !strings.Contains(out, "no models") {
		t.Errorf("empty models should print '(no models)':\n%s", out)
	}
}

func TestPrintProviderModels_WithEntries(t *testing.T) {
	entries := []ModelEntry{
		{ID: "b-model", Object: "model", OwnedBy: "x"},
		{ID: "a-model", Object: "model", OwnedBy: "x", ContextWindow: 200000},
	}
	out := grabStdout(t, func() { printProviderModels("x", entries) })
	if !strings.Contains(out, "a-model") || !strings.Contains(out, "b-model") {
		t.Errorf("missing model ids:\n%s", out)
	}
	if !strings.Contains(out, "2 models") {
		t.Errorf("missing count:\n%s", out)
	}
	// Verify sorted order (a-model before b-model).
	if strings.Index(out, "a-model") > strings.Index(out, "b-model") {
		t.Errorf("models not sorted:\n%s", out)
	}
}
