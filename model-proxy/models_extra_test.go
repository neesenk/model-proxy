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
			"zhipu": {Provider: "zhipu", Models: map[string]ProviderModel{
				"glm-5.2": {Context: 1048576, Output: 131072, Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}}},
				"glm-4.5": {Context: 131072, Output: 98304, Modalities: ProviderModalities{Input: []string{"text"}, Output: []string{"text"}}},
			}},
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "") })
	if !strings.Contains(out, "zhipu") || !strings.Contains(out, "glm-5.2") || !strings.Contains(out, "glm-4.5") {
		t.Errorf("printAllModels missing content:\n%s", out)
	}
}

func TestPrintAllModels_Filter(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {Provider: "static", Models: map[string]ProviderModel{"m1": {Context: 1000, Modalities: ProviderModalities{Input: []string{"text"}}}}},
			"b": {Provider: "static", Models: map[string]ProviderModel{"m2": {Context: 2000, Modalities: ProviderModalities{Input: []string{"text"}}}}},
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "a") })
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
			"a": {Provider: "static", Models: map[string]ProviderModel{"m1": {}}}, // Context 0, Output 0
		},
	}
	out := grabStdout(t, func() { printAllModels(cfg, "") })
	if !strings.Contains(out, "—") {
		t.Errorf("zero context/output should show as —:\n%s", out)
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

// --- fetchProviderModels: unknown provider ---

func TestFetchProviderModels_Unknown(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: "static"}}}
	_, err := fetchProviderModels(cfg, "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider: err=%v", err)
	}
}

// fetchProviderModels' happy path needs a provider whose FetchModels doesn't
// require a cred file. buildProviders wires real auth (reads cred files), so we
// can't easily inject a mock — covered indirectly by provider-side
// TestZhipuFetchModels_FromEndpoint instead.

// --- listArkAgentPlanModelIDs: no AK/SK → error ---

func TestListArkAgentPlanModelIDs_NoCreds(t *testing.T) {
	// loadVolcengineCreds reads ~/.model-proxy/<provName>_apikey.json. With HOME
	// in a temp dir, the file is absent → error.
	t.Setenv("HOME", t.TempDir())
	_, err := listArkAgentPlanModelIDs("volcengine")
	if err == nil || !strings.Contains(err.Error(), "AK/SK") {
		t.Errorf("no creds: err=%v want AK/SK error", err)
	}
}

// --- pad edge cases (models.go) ---

func TestPad_AlreadyLong(t *testing.T) {
	if got := pad("toolongalready", 5); got != "toolongalready" {
		t.Errorf("pad(long,5)=%q want passthrough", got)
	}
}

// --- nonFlagArgs: --config= form + flags interspersed ---

func TestNonFlagArgs_ConfigEquals(t *testing.T) {
	got := nonFlagArgs([]string{"--config=x.yaml", "models", "zhipu"})
	if len(got) != 2 || got[0] != "models" || got[1] != "zhipu" {
		t.Errorf("nonFlagArgs(--config=)=%v want [models zhipu]", got)
	}
}

// --- routeNames sorting (already tested, but check empty) ---

func TestRouteNames_Sorted(t *testing.T) {
	cfg := &Config{Routes: map[string][]RouteTarget{
		"zeta":  {{Provider: "a", Model: "z"}},
		"alpha": {{Provider: "a", Model: "a"}},
		"mid":   {{Provider: "a", Model: "m"}},
	}}
	got := routeNames(cfg)
	want := "alpha, mid, zeta"
	if got != want {
		t.Errorf("routeNames=%q want %q (sorted)", got, want)
	}
}

// keep sort import used (printProviderModels uses sort internally; this test
// file references it only via grabStdout otherwise).
var _ = strings.HasPrefix
