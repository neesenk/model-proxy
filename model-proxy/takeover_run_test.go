package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// --- listClients ---

func TestListClients_All(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d"}}
	all := listClients(cfg, "")
	if len(all) != 4 {
		t.Errorf("listClients('') len=%d want 4", len(all))
	}
	all = listClients(cfg, "all")
	if len(all) != 4 {
		t.Errorf("listClients('all') len=%d want 4", len(all))
	}
}

func TestListClients_OneByName(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a", Opencode: "b", Codex: "c", Pi: "d"}}
	one := listClients(cfg, "codex")
	if len(one) != 1 || one[0].name != "codex" {
		t.Errorf("listClients('codex')=%+v want [codex]", one)
	}
}

func TestListClients_UnknownReturnsNil(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a"}}
	if got := listClients(cfg, "nope"); got != nil {
		t.Errorf("listClients('nope')=%+v want nil", got)
	}
}

// --- runTakeover / runRestore end-to-end on a single client ---

func TestRunTakeover_AndRestore_Claude(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes:   map[string][]RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: Takeover{ProxyURL: "http://127.0.0.1:15721", Claude: filepath.Join(dir, "claude.json")},
	}
	bakDir := filepath.Join(dir, ".mp")

	// Seed an existing claude config, then takeover rewrites it (after backing up).
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OLD":"1"}}`), 0o644)
	if err := runTakeover(cfg, "claude", bakDir); err != nil {
		t.Fatal(err)
	}
	rewritten, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(rewritten), "ANTHROPIC_BASE_URL") {
		t.Errorf("takeover did not rewrite claude: %s", rewritten)
	}
	// Backup preserved the original.
	bak, _ := os.ReadFile(filepath.Join(bakDir, "claude.bak"))
	if !contains(string(bak), "OLD") {
		t.Errorf("backup did not preserve original: %s", bak)
	}
	// Restore brings the original back.
	if err := runRestore(cfg, "claude", bakDir); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(restored), "OLD") || contains(string(restored), "ANTHROPIC_BASE_URL") {
		t.Errorf("restore did not revert: %s", restored)
	}
}

func TestRunTakeover_UnknownClientNoOps(t *testing.T) {
	cfg := &Config{Takeover: Takeover{Claude: "a"}}
	// "nope" → listClients returns nil → loop body never runs → nil error.
	if err := runTakeover(cfg, "nope", t.TempDir()); err != nil {
		t.Errorf("runTakeover unknown client: want nil, got %v", err)
	}
}

// --- runTakeover all: skips a client whose config file is absent ---

func TestRunTakeover_AllSkipsMissingFiles(t *testing.T) {
	// Keep the models.dev catalog fetch offline: runTakeover("all") refreshes
	// the catalog for opencode/pi model metadata — point it at a local stub.
	md := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{}`)
	}))
	defer md.Close()
	t.Setenv("MP_MODELSDEV_URL", md.URL)

	dir := t.TempDir()
	bakDir := filepath.Join(dir, ".mp")
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		// Only claude exists; opencode/codex/pi point at non-existent paths.
		Takeover: Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Claude:   filepath.Join(dir, "claude.json"),
			Opencode: filepath.Join(dir, "opencode.json"),
			Codex:    filepath.Join(dir, "codex.toml"),
			Pi:       filepath.Join(dir, "pi.json"),
		},
	}
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OLD":"1"}}`), 0o644)

	if err := runTakeover(cfg, "all", bakDir); err != nil {
		t.Fatalf("runTakeover all with missing files: want nil, got %v", err)
	}
	// claude was rewritten (backup + rewrite succeeded).
	b, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(b), "ANTHROPIC_BASE_URL") {
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
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Pi:       filepath.Join(dir, "nonexistent.json"),
		},
	}
	if err := runTakeover(cfg, "pi", dir); err == nil {
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
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
		Takeover: Takeover{
			ProxyURL: "http://127.0.0.1:15721",
			Claude:   filepath.Join(dir, "claude.json"),
			Opencode: filepath.Join(dir, "opencode.json"),
			Codex:    filepath.Join(dir, "codex.toml"),
			Pi:       filepath.Join(dir, "pi.json"),
		},
	}
	if err := runRestore(cfg, "all", bakDir); err != nil {
		t.Fatalf("runRestore all with missing backups: want nil, got %v", err)
	}
	b, _ := os.ReadFile(cfg.Takeover.Claude)
	if !contains(string(b), "OLD") {
		t.Errorf("claude not restored from backup: %s", b)
	}
}
