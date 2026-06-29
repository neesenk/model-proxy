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

// Backup suffix.
const bakSuffix = ".ais-switch-proxy.bak"

// backup copies file verbatim to file.ais-switch-proxy.bak (a clean copy, easy to restore
// directly), and writes meta to file.ais-switch-proxy.bak.meta. An existing bak is not
// overwritten → idempotent.
func backup(file string) error {
	if _, err := os.Stat(file); err != nil {
		return fmt.Errorf("config file %s: %w", file, err)
	}
	bak := file + bakSuffix
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

// restore copies file.ais-switch-proxy.bak verbatim back to file.
func restore(file string) error {
	bak := file + bakSuffix
	data, err := os.ReadFile(bak)
	if err != nil {
		return fmt.Errorf("no backup for %s: %w", file, err)
	}
	return os.WriteFile(file, data, 0o644)
}

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ---- top-level dispatch ----

func runTakeover(cfg *Config, which string) error {
	clients := listClients(cfg, which)
	for _, c := range clients {
		log.Printf("takeover %s: %s", c.name, c.file)
		if err := backup(c.file); err != nil {
			return fmt.Errorf("%s backup: %w", c.name, err)
		}
		if err := c.rewrite(cfg); err != nil {
			return fmt.Errorf("%s rewrite: %w", c.name, err)
		}
		log.Printf("  ✓ %s done", c.name)
	}
	return nil
}

func runRestore(cfg *Config, which string) error {
	clients := listClients(cfg, which)
	for _, c := range clients {
		log.Printf("restore %s: %s", c.name, c.file)
		if err := restore(c.file); err != nil {
			return fmt.Errorf("%s restore: %w", c.name, err)
		}
		log.Printf("  ✓ %s restored", c.name)
	}
	return nil
}

type clientSpec struct {
	name    string
	file    string
	rewrite func(cfg *Config) error
}

func listClients(cfg *Config, which string) []clientSpec {
	all := []clientSpec{
		{name: "claude", file: cfg.Takeover.ClaudeFile, rewrite: func(c *Config) error { return rewriteClaude(c) }},
		{name: "opencode", file: cfg.Takeover.OpencodeFile, rewrite: func(c *Config) error { return rewriteOpencode(c) }},
		{name: "codex", file: cfg.Takeover.CodexFile, rewrite: func(c *Config) error { return rewriteCodex(c) }},
		{name: "pi", file: cfg.Takeover.PiFile, rewrite: func(c *Config) error { return rewritePi(c) }},
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
