package takeover

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"strings"
	"time"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// ErrNoFile signals that a takeover client's config file is absent.
// RunTakeover treats this as a skip (warn + continue) for `all`/`""`, but as a
// hard error for a single named client.
var ErrNoFile = errors.New("takeover: client config file not present")

// atomicWriteFile writes data via a unique temp file in the target directory
// followed by rename, so a crash mid-write can never leave a truncated file
// in place of the user's client config (docs/engineering/pitfalls.md #18).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}

// backup copies file verbatim into bakDir/<name>.bak (a clean copy, easy to
// restore), and writes meta to bakDir/<name>.bak.meta. An existing backup is
// not overwritten → idempotent. A missing source file returns ErrNoFile
// so RunTakeover can skip the client (for `all`) rather than abort the batch.
// Both files are written atomically: a truncated .bak would poison every
// future takeover (the "existing backup is kept" idempotency rule would keep
// the corrupt copy forever) and lose the user's original client config.
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
		// Only "backup does not exist" enters the create path. Any other
		// Stat error (permissions, I/O, invalid path) must fail closed —
		// treating it as "no backup" would overwrite an existing backup
		// with the post-takeover file and lose the user's original config.
		if !os.IsNotExist(err) {
			return fmt.Errorf("stat backup %s: %w", bak, err)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if err := atomicWriteFile(bak, data, 0o600); err != nil {
			return err
		}
		meta := map[string]any{
			"backed_up_at": time.Now().Format(time.RFC3339),
			"sha256":       Sha256hex(data),
			"path":         file,
		}
		mb, err := json.MarshalIndent(meta, "", "  ")
		if err != nil {
			return err
		}
		if err := atomicWriteFile(bak+".meta", mb, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// restore copies bakDir/<name>.bak verbatim back to file. A missing backup
// returns ErrNoFile so RunRestore can skip it (for `all`) symmetrically
// with RunTakeover's skip of a client whose config was never present.
// Before writing, the backup is verified against the sha256 Backup recorded
// in <bak>.meta — restoring a corrupted/tampered backup over the user's last
// copy must fail closed. A MISSING or unreadable/corrupt meta degrades to a
// warning and proceeds: backups taken before meta existed (or whose meta was
// lost) must stay restorable.
// The restored file is written atomically — a crash mid-restore must not
// destroy the last copy of the user's config.
// A successful restore ends the takeover: the .bak marker and its .meta are
// removed, so the drift check (which treats .bak existence as the taken-over
// marker) reports the client as not taken over, and a later takeover takes a
// fresh backup instead of keeping a stale one. Marker removal failures are
// non-fatal — the config is already restored — and only warned about.
func Restore(file, bakDir, name string) error {
	bak := filepath.Join(bakDir, name+".bak")
	data, err := os.ReadFile(bak)
	if err != nil {
		if os.IsNotExist(err) {
			return ErrNoFile
		}
		return fmt.Errorf("no backup for %s in %s: %w", name, bakDir, err)
	}
	if err := verifyBackupIntegrity(bak, data); err != nil {
		return err
	}
	if err := atomicWriteFile(file, data, preserveMode(file, 0o600)); err != nil {
		return err
	}
	for _, marker := range []string{bak, bak + ".meta"} {
		if err := os.Remove(marker); err != nil && !os.IsNotExist(err) {
			logx.Warnf("takeover: restore %s: could not remove backup marker %s: %v", name, marker, err)
		}
	}
	return nil
}

// preserveMode returns the existing file's permission bits, or fallback when
// the file does not exist yet. Takeover rewrites client configs that may hold
// real API keys next to the injected proxy entry (claude settings.json, codex
// config.toml, opencode.json): a hardcoded 0644 would widen a 0600 original to
// world-readable, so the existing mode always wins and new files start at 0600.
func preserveMode(path string, fallback os.FileMode) os.FileMode {
	if info, err := os.Stat(path); err == nil {
		return info.Mode().Perm()
	}
	return fallback
}

// verifyBackupIntegrity checks `data` against the sha256 Backup wrote to
// <bak>.meta. Meta problems (absent, unreadable, unparseable, sha256 field
// empty) warn and pass — the meta is an integrity AID, not a restore
// prerequisite; a sha256 that is present but does not match the backup bytes
// is corruption and must block the restore.
func verifyBackupIntegrity(bak string, data []byte) error {
	mb, err := os.ReadFile(bak + ".meta")
	if err != nil {
		logx.Warnf("takeover: restore %s: no readable meta (%v) — restoring without integrity check", bak, err)
		return nil
	}
	var meta struct {
		Sha256 string `json:"sha256"`
	}
	if err := json.Unmarshal(mb, &meta); err != nil || meta.Sha256 == "" {
		logx.Warnf("takeover: restore %s: meta has no usable sha256 — restoring without integrity check", bak)
		return nil
	}
	if got := Sha256hex(data); got != meta.Sha256 {
		return fmt.Errorf("backup %s integrity check failed: sha256 mismatch (meta=%s content=%s) — refusing to restore a corrupted backup", bak, meta.Sha256, got)
	}
	return nil
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

// ModelFacts carries the application-computed route table (derived routes
// aggregated from provider model lists, explicit routes overriding) and
// models.dev metadata used by metadata-writing clients. ModelFactsFor
// computes the facts (catalog loading, hydration, source markers); the CLI
// callers pass them in, and RunTakeover only rewrites client files and emits
// warnings from the supplied facts.
type ModelFacts struct {
	Routes  map[string][]configdomain.RouteTarget
	Meta    map[string]map[string]catalog.Model
	Sources map[string]map[string]int
	// SourceDefault is the application's "metadata came from conservative
	// defaults" marker value in Sources; a negative value disables warnings.
	SourceDefault int
	// DefaultContext/DefaultOutput describe the conservative fallback metadata
	// referenced by warnings.
	DefaultContext int64
	DefaultOutput  int
}

func RunTakeover(cfg *configdomain.Config, which, bakDir string, facts ModelFacts, templatesDir string) error {
	clients, err := ResolveClients(cfg, which, templatesDir)
	if err != nil {
		return err
	}
	routes := facts.Routes
	meta := facts.Meta
	EmitTakeoverWarnings(clients, cfg, meta, facts)

	// `all`/`""` expands to every client; a client whose config file isn't
	// present (e.g. that agent isn't installed) is skipped with a warning
	// rather than aborting the whole batch. A single named client still errors
	// - the user asked for that one specifically.
	batch := which == "" || which == "all"

	for _, c := range clients {
		logx.Infof("takeover %s: %s (backup -> %s/)", c.Name, c.File, bakDir)
		if c.Note != "" {
			logx.Infof("  %s", c.Note)
		}
		if err := Backup(c.File, bakDir, c.Name); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				logx.Infof("  ~ %s skipped (config not present: %s)", c.Name, c.File)
				continue
			}
			return fmt.Errorf("%s backup: %w", c.Name, err)
		}
		if err := c.Rewrite(cfg, meta, routes); err != nil {
			return fmt.Errorf("%s rewrite: %w", c.Name, err)
		}
		logx.Infof("  ✓ %s done", c.Name)
	}
	return nil
}

// EmitTakeoverWarnings prints a stderr warning for each default-sourced model
// that a metadata-writing client (opencode, pi, kimi) in this takeover set will emit.
// claude/codex don't write per-model metadata, so they are skipped to avoid noise.
func EmitTakeoverWarnings(clients []ClientSpec, cfg *configdomain.Config, meta map[string]map[string]catalog.Model, facts ModelFacts) {
	if !WritesMetadata(clients) || facts.SourceDefault < 0 {
		return
	}
	for _, m := range ExposedModels(cfg, meta, facts.Routes) {
		if facts.Sources[m.Provider] != nil && facts.Sources[m.Provider][m.RealModel] == facts.SourceDefault {
			fmt.Fprintf(os.Stderr, "warning: model %s at %s: no models.dev metadata — wrote defaults (ctx=%d out=%d text-only)\n",
				m.RealModel, m.Provider, facts.DefaultContext, facts.DefaultOutput)
		}
	}
}

func RunRestore(cfg *configdomain.Config, which, bakDir, templatesDir string) error {
	clients, batch, err := restoreClients(cfg, which, templatesDir)
	if err != nil {
		return err
	}
	for _, c := range clients {
		logx.Infof("restore %s: %s (from %s/)", c.Name, c.File, bakDir)
		if err := Restore(c.File, bakDir, c.Name); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				logx.Infof("  ~ %s skipped (no backup in %s/)", c.Name, bakDir)
				continue
			}
			return fmt.Errorf("%s restore: %w", c.Name, err)
		}
		logx.Infof("  ✓ %s restored", c.Name)
	}
	return nil
}

// restoreClients resolves the client set for restore — deliberately WITHOUT
// protocol auto-selection: a backup marker belongs to whichever variant a
// past takeover applied, and the config may have changed since, so re-running
// selection could pick a variant that was never taken over. A multi-variant
// family name (or ""/"all") expands to EVERY variant of the family with
// batch semantics (missing backups are skipped); any other exact name
// restores that one strictly (missing backup is a hard error).
func restoreClients(cfg *configdomain.Config, which, templatesDir string) ([]ClientSpec, bool, error) {
	clients, err := ListClients(cfg, "", templatesDir)
	if err != nil {
		return nil, false, err
	}
	if which == "" || which == "all" {
		return clients, true, nil
	}
	families := map[string][]ClientSpec{}
	for _, c := range clients {
		f := c.Template.ClientFamily()
		families[f] = append(families[f], c)
	}
	if variants, ok := families[which]; ok && len(variants) > 1 {
		return variants, true, nil
	}
	for _, c := range clients {
		if c.Name == which {
			return []ClientSpec{c}, false, nil
		}
	}
	if variants, ok := families[which]; ok {
		return variants, false, nil // single-variant family named differently
	}
	return nil, false, fmt.Errorf("unknown takeover client %q — available templates: %s", which, clientNames(clients))
}

func clientNames(clients []ClientSpec) string {
	names := make([]string, 0, len(clients))
	for _, c := range clients {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

type ClientSpec struct {
	Name    string
	File    string
	Rewrite func(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) error
	// Template is the resolved client template (preset or user-defined) —
	// doctor's drift probe and facts' metadata decision read it.
	Template *Template
	// Note carries the auto-selection rationale when ResolveClients picked
	// this variant from a multi-template family ("" for exact-name or
	// single-variant resolution). RunTakeover logs it.
	Note string
}

// ListClients resolves the client set for which ("" / "all" = every template,
// sorted by name; otherwise the single named template) from embedded presets
// overridden by user templates in templatesDir ("" = DefaultTemplatesDir()).
// An unknown name is a hard error listing the available templates.
func ListClients(cfg *configdomain.Config, which, templatesDir string) ([]ClientSpec, error) {
	if templatesDir == "" {
		templatesDir = DefaultTemplatesDir()
	}
	templates, err := LoadTemplates(templatesDir)
	if err != nil {
		return nil, err
	}
	if which != "" && which != "all" {
		var found *Template
		for _, t := range templates {
			if t.Name == which {
				found = t
				break
			}
		}
		if found == nil {
			names := make([]string, 0, len(templates))
			for _, t := range templates {
				names = append(names, t.Name)
			}
			return nil, fmt.Errorf("unknown takeover client %q — available templates: %s", which, strings.Join(names, ", "))
		}
		templates = []*Template{found}
	}
	out := make([]ClientSpec, 0, len(templates))
	for _, t := range templates {
		t := t
		out = append(out, ClientSpec{
			Name:     t.Name,
			File:     t.File,
			Template: t,
			Rewrite:  t.Rewrite,
		})
	}
	return out, nil
}

// WritesMetadata reports whether any client in the set writes per-model
// metadata (templates with a models: block). Used to skip the models.dev fetch for
// claude/codex-only takeovers.
func WritesMetadata(clients []ClientSpec) bool {
	for _, c := range clients {
		if c.Template != nil && c.Template.Models != nil {
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
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return atomicWriteFile(file, out, preserveMode(file, 0o600))
}
