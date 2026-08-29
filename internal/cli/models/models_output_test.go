package models_test

import (
	"io"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
	"os"
	"strings"
	"testing"
)

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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-5.2", "glm-4.5"}},
		},
	}
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "", nil, nil) })
	if !strings.Contains(out, "zhipu") || !strings.Contains(out, "glm-5.2") || !strings.Contains(out, "glm-4.5") {
		t.Errorf("printAllModels missing content:\n%s", out)
	}
}

func TestPrintAllModels_Filter(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: "static", Models: []string{"m1"}},
			"b": {Provider: "static", Models: []string{"m2"}},
		},
	}
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "a", nil, nil) })
	if strings.Contains(out, "m2") {
		t.Errorf("filter should exclude m2:\n%s", out)
	}
	if !strings.Contains(out, "m1") {
		t.Errorf("filter should include m1:\n%s", out)
	}
}

func TestPrintAllModels_EmptyContext(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: "static", Models: []string{"m1"}}, // no meta → ctx/out shown as —
		},
	}
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "", nil, nil) })
	if !strings.Contains(out, "—") {
		t.Errorf("no metadata should show ctx/out as —:\n%s", out)
	}
}
