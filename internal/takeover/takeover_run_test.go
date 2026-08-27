package takeover_test

import (
	"fmt"
	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- listClients ---

func TestListClients_All(t *testing.T) {
	cfg := &configdomain.Config{Takeover: configdomain.Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d", Kimi: "e"}}
	all := takeover.ListClients(cfg, "")
	if len(all) != 5 {
		t.Errorf("takeover.ListClients('') len=%d want 5", len(all))
	}
	all = takeover.ListClients(cfg, "all")
	if len(all) != 5 {
		t.Errorf("takeover.ListClients('all') len=%d want 5", len(all))
	}
}

func TestListClients_OneByName(t *testing.T) {
	cfg := &configdomain.Config{Takeover: configdomain.Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d"}}
	one := takeover.ListClients(cfg, "codex")
	if len(one) != 1 || one[0].Name != "codex" {
		t.Errorf("takeover.ListClients('codex')=%+v want [codex]", one)
	}
}

func TestListClients_UnknownReturnsNil(t *testing.T) {
	cfg := &configdomain.Config{Takeover: configdomain.Takeover{Claude: "a"}}
	if got := takeover.ListClients(cfg, "nope"); got != nil {
		t.Errorf("takeover.ListClients('nope')=%+v want nil", got)
	}
}

// --- runTakeover / runRestore end-to-end on a single client ---

func TestRunTakeover_AndRestore_Claude(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes:   map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: configdomain.Takeover{ProxyURL: "http://127.0.0.1:15721", Claude: filepath.Join(dir, "claude.json")},
	}
	bakDir := filepath.Join(dir, ".mp")

	// Seed an existing claude config, then takeover rewrites it (after backing up).
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if err := takeover.RunTakeover(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := os.ReadFile(cfg.Takeover.Claude)
	if !strings.Contains(string(rewritten), "ANTHROPIC_BASE_URL") {
		t.Errorf("takeover did not rewrite claude: %s", rewritten)
	}
	// Backup preserved the original.
	bak, _ := os.ReadFile(filepath.Join(bakDir, "claude.bak"))
	if !strings.Contains(string(bak), "OLD") {
		t.Errorf("backup did not preserve original: %s", bak)
	}
	// Restore brings the original back.
	if err := takeover.RunRestore(cfg, "claude", bakDir); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(cfg.Takeover.Claude)
	if !strings.Contains(string(restored), "OLD") || strings.Contains(string(restored), "ANTHROPIC_BASE_URL") {
		t.Errorf("restore did not revert: %s", restored)
	}
}

func TestRunTakeover_UnknownClientNoOps(t *testing.T) {
	cfg := &configdomain.Config{Takeover: configdomain.Takeover{Claude: "a"}}
	// "nope" → listClients returns nil → loop body never runs → nil error.
	if err := takeover.RunTakeover(cfg, "nope", t.TempDir(), takeover.ModelFacts{SourceDefault: -1}); err != nil {
		t.Errorf("runTakeover unknown client: want nil, got %v", err)
	}
}

// --- runTakeover all: skips a client whose config file is absent ---

func TestRunTakeover_AllSkipsMissingFiles(t *testing.T) {
	// Keep the models.dev catalog fetch offline: takeover.RunTakeover("all") refreshes
	// the catalog for opencode/pi model metadata — point it at a local stub.
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer md.Close()
	t.Setenv("MP_MODELSDEV_URL", md.URL)

	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		// Only claude exists; opencode/codex/pi point at non-existent paths.
		Takeover: configdomain.Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Claude:   filepath.Join(dir, "claude.json"),
			Opencode: filepath.Join(dir, "opencode.json"),
			Codex:    filepath.Join(dir, "codex.toml"),
			Pi:       filepath.Join(dir, "pi.json"),
		},
	}
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OLD":"1"}}`), 0o644)

	cat, _ := app.LoadModelsCatalog(cliframework.HomeDir(), false)
	meta, sources := app.HydrateModels(cfg, cat)
	factsSources := make(map[string]map[string]int, len(sources))
	for provider, models := range sources {
		factsSources[provider] = make(map[string]int, len(models))
		for model, source := range models {
			factsSources[provider][model] = int(source)
		}
	}
	implicit, _ := app.SynthesizeImplicitRoutes(cfg, accounts.NewStore(cliframework.HomeDir()))
	facts := takeover.ModelFacts{
		Implicit:       implicit,
		Meta:           meta,
		Sources:        factsSources,
		SourceDefault:  int(app.SrcDefault),
		DefaultContext: app.DefaultModelMetadata.Context,
		DefaultOutput:  app.DefaultModelMetadata.Output,
	}
	if err := takeover.RunTakeover(cfg, "all", bakDir, facts); err != nil {
		t.Fatalf("runTakeover all with missing files: want nil, got %v", err)
	}
	// claude was rewritten (backup + rewrite succeeded).
	b, _ := os.ReadFile(cfg.Takeover.Claude)
	if !strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("claude not rewritten: %s", b)
	}
	// Missing clients' files were NOT created (skipped, not rewritten to defaults).
	for _, p := range []string{cfg.Takeover.Opencode, cfg.Takeover.Codex, cfg.Takeover.Pi} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("missing client file %s should not have been created", p)
		}
	}
}

// --- runTakeover single named client: missing file is a hard error (not skipped) ---

func TestRunTakeover_SingleMissingFileErrors(t *testing.T) {
	dir := t.TempDir()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: configdomain.Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Pi:       filepath.Join(dir, "nonexistent.json"),
		},
	}
	if err := takeover.RunTakeover(cfg, "pi", dir, takeover.ModelFacts{SourceDefault: -1}); err == nil {
		t.Error("runTakeover pi with missing file: want error, got nil (single client must not be skipped)")
	}
}

// --- runRestore all: skips a client with no backup (symmetric with takeover) ---

func TestRunRestore_AllSkipsMissingBackup(t *testing.T) {
	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	os.MkdirAll(bakDir, 0o700)
	// Only a claude backup exists.
	os.WriteFile(filepath.Join(bakDir, "claude.bak"), []byte(`{"env":{"OLD":"1"}}`), 0o600)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: configdomain.Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Claude:   filepath.Join(dir, "claude.json"),
			Opencode: filepath.Join(dir, "opencode.json"),
			Codex:    filepath.Join(dir, "codex.toml"),
			Pi:       filepath.Join(dir, "pi.json"),
		},
	}
	if err := takeover.RunRestore(cfg, "all", bakDir); err != nil {
		t.Fatalf("runRestore all with missing backups: want nil, got %v", err)
	}
	b, _ := os.ReadFile(cfg.Takeover.Claude)
	if !strings.Contains(string(b), "OLD") {
		t.Errorf("claude not restored from backup: %s", b)
	}
}
