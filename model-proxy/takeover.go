package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"
)

// backup copies file verbatim into bakDir/<name>.bak (a clean copy, easy to
// restore), and writes meta to bakDir/<name>.bak.meta. An existing backup is
// not overwritten → idempotent.
func backup(file, bakDir, name string) error {
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("config file %s: %w", file, err)
	}
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		return fmt.Errorf("create backup dir %s: %w", bakDir, err)
	}
	bak := filepath.Join(bakDir, name+".bak")
	if _, err := os.Stat(bak); err != nil {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := os.WriteFile(bak, data, 0o600); err != nil {
			return err
		}
		meta := map[string]any{
			"backed_up_at": time.Now().Format(time.RFC3339),
			"sha256":       sha256hex(data),
			"path":         file,
		}
		mb, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(bak+".meta", mb, 0o600)
	}
	return nil
}

// restore copies bakDir/<name>.bak verbatim back to file.
func restore(file, bakDir, name string) error {
	bak := filepath.Join(bakDir, name+".bak")
	data, err := os.ReadFile(bak)
	if err != nil {
		return fmt.Errorf("no backup for %s in %s: %w", name, bakDir, err)
	}
	return os.WriteFile(file, data, 0o644)
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// backupDir returns the backup directory: <configDir>/.model-proxy/
func backupDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".model-proxy")
}

// ---- top-level dispatch ----

func runTakeover(cfg *Config, which, bakDir string) error {
	clients := listClients(cfg, which)
	// Implicit routes (auto-derived from logged-in providers' model lists) are
	// callable through the proxy and listed in /v1/models — include them so the
	// takeover client config matches. Cheap (loadPool file I/O, no network).
	implicit, _ := synthesizeImplicitRoutes(cfg)
	// Hydrate models.dev metadata only if a client in this set writes per-model
	// metadata (opencode/pi). claude/codex ignore it, so skip the (≤10s) fetch.
	var meta map[string]map[string]ProviderModel
	var sources map[string]map[string]modelSource
	if takeoverWritesMetadata(clients) {
		cat, _ := ensureCatalogFresh(cachePath(), modelsDevEndpoint(), realModelsDevFetch, false)
		meta, sources = hydrateModels(cfg, cat)
	}
	emitTakeoverWarnings(clients, cfg, meta, sources, implicit)

	for _, c := range clients {
		log.Printf("takeover %s: %s (backup → %s/)", c.name, c.file, bakDir)
		if err := backup(c.file, bakDir, c.name); err != nil {
			return fmt.Errorf("%s backup: %w", c.name, err)
		}
		if err := c.rewrite(cfg, meta, implicit); err != nil {
			return fmt.Errorf("%s rewrite: %w", c.name, err)
		}
		log.Printf("  ✓ %s done", c.name)
	}
	return nil
}

// emitTakeoverWarnings prints a stderr warning for each default-sourced model
// that a metadata-writing client (opencode, pi) in this takeover set will emit.
// claude/codex don't write per-model metadata, so they are skipped to avoid noise.
func emitTakeoverWarnings(clients []clientSpec, cfg *Config, meta map[string]map[string]ProviderModel, sources map[string]map[string]modelSource, implicit map[string]RouteTarget) {
	if !takeoverWritesMetadata(clients) {
		return
	}
	for _, m := range exposedModels(cfg, meta, implicit) {
		if sources[m.provider] != nil && sources[m.provider][m.realModel] == srcDefault {
			fmt.Fprintf(os.Stderr, "warning: model %s at %s: no models.dev metadata — wrote defaults (ctx=%d out=%d text-only)\n",
				m.realModel, m.provider, defaultProviderModel.Context, defaultProviderModel.Output)
		}
	}
}

func runRestore(cfg *Config, which, bakDir string) error {
	clients := listClients(cfg, which)
	for _, c := range clients {
		log.Printf("restore %s: %s (from %s/)", c.name, c.file, bakDir)
		if err := restore(c.file, bakDir, c.name); err != nil {
			return fmt.Errorf("%s restore: %w", c.name, err)
		}
		log.Printf("  ✓ %s restored", c.name)
	}
	return nil
}

type clientSpec struct {
	name    string
	file    string
	rewrite func(cfg *Config, meta map[string]map[string]ProviderModel, implicit map[string]RouteTarget) error
}

func listClients(cfg *Config, which string) []clientSpec {
	all := []clientSpec{
		{name: "claude", file: cfg.Takeover.Claude, rewrite: func(c *Config, _ map[string]map[string]ProviderModel, _ map[string]RouteTarget) error {
			return rewriteClaude(c)
		}},
		{name: "opencode", file: cfg.Takeover.Opencode, rewrite: func(c *Config, m map[string]map[string]ProviderModel, imp map[string]RouteTarget) error {
			return rewriteOpencode(c, m, imp)
		}},
		{name: "codex", file: cfg.Takeover.Codex, rewrite: func(c *Config, _ map[string]map[string]ProviderModel, _ map[string]RouteTarget) error {
			return rewriteCodex(c)
		}},
		{name: "pi", file: cfg.Takeover.Pi, rewrite: func(c *Config, m map[string]map[string]ProviderModel, imp map[string]RouteTarget) error {
			return rewritePi(c, m, imp)
		}},
	}
	if which == "" || which == "all" {
		return all
	}
	for _, c := range all {
		if c.name == which {
			return []clientSpec{c}
		}
	}
	return nil
}

// takeoverWritesMetadata reports whether any client in the set writes per-model
// metadata (opencode, pi). Used to skip the models.dev fetch for claude/codex.
func takeoverWritesMetadata(clients []clientSpec) bool {
	for _, c := range clients {
		if c.name == "opencode" || c.name == "pi" {
			return true
		}
	}
	return false
}

// readJSONConfig reads a JSON config file (returns an empty map if absent).
func readJSONConfig(file string) (map[string]any, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, err
	}
	if v == nil {
		v = map[string]any{}
	}
	return v, nil
}

func writeJSONConfig(file string, v map[string]any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	os.MkdirAll(dir, 0o755)
	return os.WriteFile(file, out, 0o644)
}
