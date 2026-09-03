package models_test

import (
	"io"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
	runtimewire "model-proxy/internal/runtime/wirecap"
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
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "", nil, nil, nil) })
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
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "a", nil, nil, nil) })
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
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "", nil, nil, nil) })
	if !strings.Contains(out, "—") {
		t.Errorf("no metadata should show ctx/out as —:\n%s", out)
	}
}

// --- printAllModels: PROTOCOLS column from the probe matrix ---

func TestPrintAllModels_ProtocolsColumn(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-5.2", "glm-4.5", "uncached"}},
		},
	}
	protocols := map[string]map[string]runtimewire.ModelProtocols{
		"zhipu": {
			"glm-5.2": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes},
			"glm-4.5": {Chat: runtimewire.No, Anthropic: runtimewire.No, Responses: runtimewire.No},
			// "uncached" has no entry -> "-"
		},
	}
	out := grabStdout(t, func() { climodels.PrintAllModels(cfg, "", nil, nil, protocols) })
	if !strings.Contains(out, "PROTOCOLS") {
		t.Errorf("printAllModels missing PROTOCOLS header:\n%s", out)
	}
	if !strings.Contains(out, "chat/resp") {
		t.Errorf("glm-5.2 should render chat/resp:\n%s", out)
	}
	if !strings.Contains(out, "none") {
		t.Errorf("glm-4.5 (all legs No) should render none:\n%s", out)
	}
	// The model without a caps entry renders "-" in the PROTOCOLS column.
	var uncachedLine string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "uncached") {
			uncachedLine = line
		}
	}
	if uncachedLine == "" {
		t.Fatalf("uncached row missing:\n%s", out)
	}
	if !strings.Contains(uncachedLine, "-") || strings.Contains(uncachedLine, "chat") || strings.Contains(uncachedLine, "none") {
		t.Errorf("uncached row should render '-' in PROTOCOLS, got: %q", uncachedLine)
	}
}
