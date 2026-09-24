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
// BackupScoped copies file verbatim into bakDir/<name>.bak with the takeover
// scope recorded in the meta (ScopeMCP backups are exempt from the model
// drift probe — the model part was never written). Backup is the ScopeAll
// form kept for callers that do not choose.
func BackupScoped(file, bakDir, name string, scope RewriteScope) error {
	if err := backup(file, bakDir, name, scope); err != nil {
		return err
	}
	return nil
}

func Backup(file, bakDir, name string) error {
	return backup(file, bakDir, name, ScopeAll)
}

func backup(file, bakDir, name string, scope RewriteScope) error {
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
		if scope != ScopeAll {
			meta["scope"] = string(scope)
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
	// Unreachable lists exposed models dropped from Routes because no target
	// can serve them over a chat protocol (decisions-only providers, e.g.
	// typesafe's jev — no chat conversion exists). RunTakeoverReportOpts logs
	// them so the models' absence from written configs is explained.
	Unreachable []string
	// SourceDefault is the application's "metadata came from conservative
	// defaults" marker value in Sources; a negative value disables warnings.
	SourceDefault int
	// DefaultContext/DefaultOutput describe the conservative fallback metadata
	// referenced by warnings.
	DefaultContext int64
	DefaultOutput  int
}

// AppliedClient records one client config rewritten by a takeover run; Note
// carries the protocol auto-selection rationale when one applied.
type AppliedClient struct {
	Name string
	Note string
}

// TakeoverReport is the structured outcome of RunTakeoverReport: which client
// configs were rewritten, which were skipped (batch mode, config file absent),
// and the metadata-default warnings the run emitted.
type TakeoverReport struct {
	Applied  []AppliedClient
	Skipped  []string
	Warnings []string
}

// RestoreReport is the structured outcome of RunRestoreReport.
type RestoreReport struct {
	Restored []string
	Skipped  []string
}

func RunTakeover(cfg *configdomain.Config, which, bakDir string, facts ModelFacts, templatesDir string, mode ResolveMode) error {
	return RunTakeoverOpts(cfg, which, bakDir, facts, templatesDir, DefaultTakeoverOptions(mode))
}

// RunTakeoverOpts is RunTakeover with full options (scope + subset
// selections).
func RunTakeoverOpts(cfg *configdomain.Config, which, bakDir string, facts ModelFacts, templatesDir string, opts TakeoverOptions) error {
	_, err := RunTakeoverReportOpts(cfg, which, bakDir, facts, templatesDir, opts)
	return err
}

// RewriteScope selects what a takeover writes: the model/provider part, the
// MCP surface, or both (the default). Backups record the scope so drift and
// restore can tell a model-only takeover from a full one.
type RewriteScope string

const (
	ScopeAll   RewriteScope = ""      // provider/models + MCP (the CLI default)
	ScopeModel RewriteScope = "model" // provider/models only; MCP untouched
	ScopeMCP   RewriteScope = "mcp"   // MCP surface only; provider/models untouched
)

func (s RewriteScope) Valid() bool {
	switch s {
	case ScopeAll, ScopeModel, ScopeMCP:
		return true
	}
	return false
}

// TakeoverOptions carries the run/preview shape beyond client resolution:
// the protocol mode, the model/mcp scope, and optional SUBSET selections —
// nil MCP/Models means "everything the gateway exposes", a non-nil list
// writes only the named entries (the Web dialog's partial takeover).
type TakeoverOptions struct {
	Mode   ResolveMode
	Scope  RewriteScope
	MCP    []string // gateway server/route names to write (nil = all)
	Models []string // exposed model names to write (nil = all)
}

// DefaultTakeoverOptions is the CLI-equivalent run: unified, full scope,
// everything the gateway exposes.
func DefaultTakeoverOptions(mode ResolveMode) TakeoverOptions {
	return TakeoverOptions{Mode: mode}
}

func (o TakeoverOptions) validated() (TakeoverOptions, error) {
	if !o.Scope.Valid() {
		return o, fmt.Errorf("unknown takeover scope %q (want model|mcp|all)", o.Scope)
	}
	switch o.Mode {
	case ModeUnified, ModeSplit:
	default:
		if !o.Mode.IsProtocol() {
			return o, fmt.Errorf("unknown takeover mode %q (want unified|split|anthropic|openai|responses)", o.Mode)
		}
	}
	if len(o.MCP) > 0 {
		seen := map[string]bool{}
		for _, n := range o.MCP {
			if n == "" || seen[n] {
				return o, fmt.Errorf("invalid MCP selection entry %q", n)
			}
			seen[n] = true
		}
	}
	if len(o.Models) > 0 {
		seen := map[string]bool{}
		for _, n := range o.Models {
			if n == "" || seen[n] {
				return o, fmt.Errorf("invalid model selection entry %q", n)
			}
			seen[n] = true
		}
	}
	return o, nil
}

// RunTakeoverReport is RunTakeover plus a structured report for non-CLI
// callers (the Web admin API): the logging/progress behavior is identical,
// but applied/skipped clients and warnings also come back as data.
func RunTakeoverReport(cfg *configdomain.Config, which, bakDir string, facts ModelFacts, templatesDir string, mode ResolveMode, scope RewriteScope) (TakeoverReport, error) {
	return RunTakeoverReportOpts(cfg, which, bakDir, facts, templatesDir, TakeoverOptions{Mode: mode, Scope: scope})
}

// RunTakeoverReportOpts is RunTakeoverReport with full options.
func RunTakeoverReportOpts(cfg *configdomain.Config, which, bakDir string, facts ModelFacts, templatesDir string, opts TakeoverOptions) (TakeoverReport, error) {
	var report TakeoverReport
	opts, err := opts.validated()
	if err != nil {
		return report, err
	}
	clients, err := ResolveClientsMode(cfg, which, templatesDir, opts.Mode)
	if err != nil {
		return report, err
	}
	routes := facts.Routes
	meta := facts.Meta
	report.Warnings = MetadataWarnings(clients, cfg, facts)
	EmitTakeoverWarnings(clients, cfg, meta, facts)
	if len(facts.Unreachable) > 0 {
		logx.Infof("  ~ excluded (no chat-protocol route — cannot be served to chat clients): %s",
			strings.Join(facts.Unreachable, ", "))
	}

	// `all`/`""` expands to every client; a client whose config file isn't
	// present (e.g. that agent isn't installed) is skipped with a warning
	// rather than aborting the whole batch. A single named client still errors
	// - the user asked for that one specifically.
	batch := which == "" || which == "all"

	// Two phases: ALL backups first, then all rewrites. Split mode emits
	// several specs sharing one client file (one per protocol variant) —
	// interleaved backup→rewrite would capture the already-rewritten file as
	// the next variant's "original", and a later restore would resurrect
	// another variant's provider entry.
	var survivors []ClientSpec
	for _, c := range clients {
		logx.Infof("takeover %s: %s (backup -> %s/)", c.Name, c.File, bakDir)
		if c.Note != "" {
			logx.Infof("  %s", c.Note)
		}
		if err := BackupScoped(c.File, bakDir, c.Name, opts.Scope); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				logx.Infof("  ~ %s skipped (config not present: %s)", c.Name, c.File)
				report.Skipped = append(report.Skipped, c.Name)
				continue
			}
			return report, fmt.Errorf("%s backup: %w", c.Name, err)
		}
		if aux := mcpAuxFile(c.Template); aux != "" {
			// A separate MCP storage file is part of the same takeover — its
			// backup rides under <name>-mcp so the restore unit matches. A
			// missing aux file is normal even for a named client (a fresh
			// install never configured MCP): the rewrite creates it.
			if err := BackupScoped(aux, bakDir, c.Name+"-mcp", opts.Scope); err != nil && !errors.Is(err, ErrNoFile) {
				return report, fmt.Errorf("%s mcp backup: %w", c.Name, err)
			}
		}
		survivors = append(survivors, c)
	}
	for _, c := range survivors {
		if err := c.RewriteOpts(cfg, meta, routes, opts); err != nil {
			return report, fmt.Errorf("%s rewrite: %w", c.Name, err)
		}
		logx.Infof("  ✓ %s done", c.Name)
		report.Applied = append(report.Applied, AppliedClient{Name: c.Name, Note: c.Note})
	}
	return report, nil
}

// MetadataWarnings returns one warning per default-sourced model that a
// metadata-writing client (opencode, pi, kimi) in this takeover set will
// emit. claude/codex don't write per-model metadata, so they are skipped to
// avoid noise.
func MetadataWarnings(clients []ClientSpec, cfg *configdomain.Config, facts ModelFacts) []string {
	if !WritesMetadata(clients) || facts.SourceDefault < 0 {
		return nil
	}
	var out []string
	for _, m := range ExposedModels(cfg, facts.Meta, facts.Routes) {
		if facts.Sources[m.Provider] != nil && facts.Sources[m.Provider][m.RealModel] == facts.SourceDefault {
			out = append(out, fmt.Sprintf("model %s at %s: no models.dev metadata — wrote defaults (ctx=%d out=%d text-only)",
				m.RealModel, m.Provider, facts.DefaultContext, facts.DefaultOutput))
		}
	}
	return out
}

// EmitTakeoverWarnings prints MetadataWarnings to stderr, one line each.
func EmitTakeoverWarnings(clients []ClientSpec, cfg *configdomain.Config, meta map[string]map[string]catalog.Model, facts ModelFacts) {
	for _, w := range MetadataWarnings(clients, cfg, facts) {
		fmt.Fprintln(os.Stderr, "warning: "+w)
	}
}

func RunRestore(cfg *configdomain.Config, which, bakDir, templatesDir string) error {
	_, err := RunRestoreReport(cfg, which, bakDir, templatesDir)
	return err
}

// RunRestoreReport is RunRestore plus a structured report for non-CLI
// callers (the Web admin API).
func RunRestoreReport(cfg *configdomain.Config, which, bakDir, templatesDir string) (RestoreReport, error) {
	var report RestoreReport
	clients, batch, err := restoreClients(cfg, which, templatesDir)
	if err != nil {
		return report, err
	}
	for _, c := range clients {
		logx.Infof("restore %s: %s (from %s/)", c.Name, c.File, bakDir)
		if err := Restore(c.File, bakDir, c.Name); err != nil {
			if batch && errors.Is(err, ErrNoFile) {
				logx.Infof("  ~ %s skipped (no backup in %s/)", c.Name, bakDir)
				report.Skipped = append(report.Skipped, c.Name)
				continue
			}
			return report, fmt.Errorf("%s restore: %w", c.Name, err)
		}
		// A separate MCP storage file was backed up as <name>-mcp — restore
		// it as part of the same takeover (missing backup = the aux file was
		// never present before the takeover; try anyway, ErrNoFile is skipped
		// below via the shared path).
		if aux := mcpAuxFile(c.Template); aux != "" {
			if err := Restore(aux, bakDir, c.Name+"-mcp"); err != nil && !errors.Is(err, ErrNoFile) {
				return report, fmt.Errorf("%s mcp restore: %w", c.Name, err)
			}
		}
		// Side artifacts the takeover wrote next to the client config (codex's
		// model-catalog JSON) end with the takeover too.
		if c.Template != nil && c.Template.Models != nil && c.Template.Models.CatalogFile != "" {
			if err := os.Remove(expandHome(c.Template.Models.CatalogFile)); err != nil && !os.IsNotExist(err) {
				logx.Warnf("takeover: restore %s: could not remove model catalog %s: %v", c.Name, c.Template.Models.CatalogFile, err)
			}
		}
		logx.Infof("  ✓ %s restored", c.Name)
		report.Restored = append(report.Restored, c.Name)
	}
	return report, nil
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
	// RewriteOpts applies the same rewrite with full options (scope + MCP/
	// model subset selections). Derived from the template (plus the split
	// model filter); plain Rewrite stays for direct template adapters.
	RewriteOpts func(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget, opts TakeoverOptions) error
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
			Name:        t.Name,
			File:        t.File,
			Template:    t,
			Rewrite:     t.Rewrite,
			RewriteOpts: t.RewriteOpts,
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
