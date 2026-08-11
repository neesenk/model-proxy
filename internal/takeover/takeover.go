package takeover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// ErrNoFile signals that a takeover client's config file is absent.
// RunTakeover treats this as a skip (warn + continue) for `all`/`""`, but as a
// hard error for a single named client.
var ErrNoFile = errors.New("takeover: client config file not present")

// backup copies file verbatim into bakDir/<name>.bak (a clean copy, easy to
// restore), and writes meta to bakDir/<name>.bak.meta. An existing backup is
// not overwritten → idempotent. A missing source file returns ErrNoFile
// so RunTakeover can skip the client (for `all`) rather than abort the batch.
func Backup(file, bakDir, name string) error {
	if _, err := os.Stat(file); err != nil {
		if os.IsNotExist(err) {
			return ErrNoFile
		}
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
			"sha256":       Sha256hex(data),
			"path":         file,
		}
		mb, _ := json.MarshalIndent(meta, "", "  ")
		os.WriteFile(bak+".meta", mb, 0o600)
	}
	return nil
}

// restore copies bakDir/<name>.bak verbatim back to file. A missing backup
// returns ErrNoFile so RunRestore can skip it (for `all`) symmetrically
// with RunTakeover's skip of a client whose config was never present.
func Restore(file, bakDir, name string) error {
	bak := filepath.Join(bakDir, name+".bak")
	data, err := os.ReadFile(bak)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoFile
		}
		return fmt.Errorf("no backup for %s in %s: %w", name, bakDir, err)
	}
	return os.WriteFile(file, data, 0o644)
}

func Sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// BackupDir returns the backup directory: <configDir>/.model-proxy/
func BackupDir(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), ".model-proxy")
}

// ---- top-level dispatch ----

// ModelFacts carries the application-computed implicit routes and models.dev
// metadata used by metadata-writing clients. The application owns catalog
// loading, hydration and its source markers; this package only rewrites client
// files and emits warnings from the supplied facts.
type ModelFacts struct {
	Implicit map[string]configdomain.RouteTarget
	Meta     map[string]map[string]catalog.Model
	Sources  map[string]map[string]int
	// SourceDefault is the application's "metadata came from conservative
	// defaults" marker value in Sources; a negative value disables warnings.
	SourceDefault int
	// DefaultContext/DefaultOutput describe the conservative fallback metadata
	// referenced by warnings.
	DefaultContext int64
	DefaultOutput  int
}

func RunTakeover(cfg *configdomain.Config, which, bakDir string, facts ModelFacts) error {
	clients := ListClients(cfg, which)
	implicit := facts.Implicit
	meta := facts.Meta
	EmitTakeoverWarnings(clients, cfg, meta, facts)

	// `all`/`""` expands to every client; a client whose config file isn't
	// present (e.g. that agent isn't installed) is skipped with a warning
	// rather than aborting the whole batch. A single named client still errors
	// - the user asked for that one specifically.
	batch := which == "" || which == "all"

	for _, c := range clients {
		log.Printf("takeover %s: %s (backup -> %s/)", c.Name, c.File, bakDir)
		if err := Backup(c.File, bakDir, c.Name); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				log.Printf("  ~ %s skipped (config not present: %s)", c.Name, c.File)
				continue
			}
			return fmt.Errorf("%s backup: %w", c.Name, err)
		}
		if err := c.Rewrite(cfg, meta, implicit); err != nil {
			return fmt.Errorf("%s rewrite: %w", c.Name, err)
		}
		log.Printf("  ✓ %s done", c.Name)
	}
	return nil
}

// EmitTakeoverWarnings prints a stderr warning for each default-sourced model
// that a metadata-writing client (opencode, pi) in this takeover set will emit.
// claude/codex don't write per-model metadata, so they are skipped to avoid noise.
func EmitTakeoverWarnings(clients []ClientSpec, cfg *configdomain.Config, meta map[string]map[string]catalog.Model, facts ModelFacts) {
	if !WritesMetadata(clients) || facts.SourceDefault < 0 {
		return
	}
	for _, m := range ExposedModels(cfg, meta, facts.Implicit) {
		if facts.Sources[m.Provider] != nil && facts.Sources[m.Provider][m.RealModel] == facts.SourceDefault {
			fmt.Fprintf(os.Stderr, "warning: model %s at %s: no models.dev metadata — wrote defaults (ctx=%d out=%d text-only)\n",
				m.RealModel, m.Provider, facts.DefaultContext, facts.DefaultOutput)
		}
	}
}

func RunRestore(cfg *configdomain.Config, which, bakDir string) error {
	clients := ListClients(cfg, which)
	batch := which == "" || which == "all"
	for _, c := range clients {
		log.Printf("restore %s: %s (from %s/)", c.Name, c.File, bakDir)
		if err := Restore(c.File, bakDir, c.Name); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				log.Printf("  ~ %s skipped (no backup in %s/)", c.Name, bakDir)
				continue
			}
			return fmt.Errorf("%s restore: %w", c.Name, err)
		}
		log.Printf("  ✓ %s restored", c.Name)
	}
	return nil
}

type ClientSpec struct {
	Name    string
	File    string
	Rewrite func(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, implicit map[string]configdomain.RouteTarget) error
}

func ListClients(cfg *configdomain.Config, which string) []ClientSpec {
	all := []ClientSpec{
		{Name: "claude", File: cfg.Takeover.Claude, Rewrite: func(c *configdomain.Config, _ map[string]map[string]catalog.Model, _ map[string]configdomain.RouteTarget) error {
			return RewriteClaude(c)
		}},
		{Name: "opencode", File: cfg.Takeover.Opencode, Rewrite: func(c *configdomain.Config, m map[string]map[string]catalog.Model, imp map[string]configdomain.RouteTarget) error {
			return RewriteOpencode(c, m, imp)
		}},
		{Name: "codex", File: cfg.Takeover.Codex, Rewrite: func(c *configdomain.Config, _ map[string]map[string]catalog.Model, _ map[string]configdomain.RouteTarget) error {
			return RewriteCodex(c)
		}},
		{Name: "pi", File: cfg.Takeover.Pi, Rewrite: func(c *configdomain.Config, m map[string]map[string]catalog.Model, imp map[string]configdomain.RouteTarget) error {
			return RewritePi(c, m, imp)
		}},
	}
	if which == "" || which == "all" {
		return all
	}
	for _, c := range all {
		if c.Name == which {
			return []ClientSpec{c}
		}
	}
	return nil
}

// WritesMetadata reports whether any client in the set writes per-model
// metadata (opencode, pi). Used to skip the models.dev fetch for claude/codex.
func WritesMetadata(clients []ClientSpec) bool {
	for _, c := range clients {
		if c.Name == "opencode" || c.Name == "pi" {
			return true
		}
	}
	return false
}

// ReadJSONConfig reads a JSON config file (returns an empty map if absent).
func ReadJSONConfig(file string) (map[string]any, error) {
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

func WriteJSONConfig(file string, v map[string]any) error {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(file)
	os.MkdirAll(dir, 0o755)
	return os.WriteFile(file, out, 0o644)
}
