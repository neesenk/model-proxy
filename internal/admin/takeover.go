package admin

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	"model-proxy/internal/configedit"
	"model-proxy/internal/takeover"
)

// takeover.go — the Web admin surface for client takeover: the daemon twins
// of `model-proxy takeover|restore [client] [--mode]` plus takeover template
// management (view/save/delete user overrides). The mechanics (backup,
// rewrite, drift probe, protocol auto-selection) stay owned by
// internal/takeover; this file only resolves directories, projects state and
// classifies errors for the HTTP transport.

// takeoverDirs resolves the takeover directory set from one HomeDir port
// read: templatesDir (user overrides, same path as
// takeover.DefaultTemplatesDir but rooted at the port's home so tests stay
// hermetic) and bakDir (backup markers under the config file's directory).
func (s *Service) takeoverDirs() (homeDir, templatesDir, bakDir string) {
	homeDir = accounts.HomeDir()
	if s.ports.HomeDir != nil {
		if h := s.ports.HomeDir(); h != "" {
			homeDir = h
		}
	}
	templatesDir = filepath.Join(homeDir, ".model-proxy", "takeover-templates")
	bakDir = takeover.BackupDir(s.currentConfigFile())
	return homeDir, templatesDir, bakDir
}

// TakeoverSurface projects every takeover template with its install /
// takeover / drift state for GET /api/takeover. mode selects which variant
// the auto_selected marker previews ("" = unified); invalid modes are a 400.
func (s *Service) TakeoverSurface(mode string) (appapi.TakeoverSurface, error) {
	cfg := s.ports.Config()
	_, templatesDir, bakDir := s.takeoverDirs()
	resolvedMode, err := resolveTakeoverMode(mode)
	if err != nil {
		return appapi.TakeoverSurface{}, err
	}
	surface := appapi.TakeoverSurface{
		Clients:      []appapi.TakeoverClient{},
		TemplatesDir: templatesDir,
		BackupDir:    bakDir,
	}
	clients, err := takeover.ListClients(cfg, "", templatesDir)
	if err != nil {
		return surface, err
	}
	drift, err := takeover.CheckDrift(cfg, bakDir, templatesDir)
	if err != nil {
		return surface, err
	}
	driftByName := make(map[string]takeover.ClientDrift, len(drift))
	for _, d := range drift {
		driftByName[d.Client] = d
	}
	// auto_selected marks the variant(s) the requested mode would write per
	// family — meaningful only for multi-variant families (pi, opencode);
	// single-variant families are always trivially "selected" and stay
	// unmarked to keep the marker a signal.
	familySize := map[string]int{}
	for _, c := range clients {
		familySize[c.Template.ClientFamily()]++
	}
	selected := map[string]bool{}
	resolvedClients, err := takeover.ResolveClientsMode(cfg, "", templatesDir, resolvedMode)
	if err != nil {
		return surface, err
	}
	for _, c := range resolvedClients {
		if familySize[c.Template.ClientFamily()] > 1 {
			selected[c.Name] = true
		}
	}
	// SplitWouldChange is per family; resolve once per family, not per variant.
	splitChanges := map[string]bool{}
	for _, c := range clients {
		family := c.Template.ClientFamily()
		if _, ok := splitChanges[family]; !ok {
			splitChanges[family] = takeover.SplitWouldChange(cfg, family, templatesDir)
		}
		d := driftByName[c.Name]
		entry := appapi.TakeoverClient{
			Name:         c.Name,
			Family:       family,
			Protocol:     c.Template.Protocol,
			Format:       c.Template.Format,
			Source:       takeoverTemplateSource(c.Template.Source),
			Description:  c.Template.Description,
			File:         c.File,
			Installed:    fileExists(c.File),
			TakenOver:    d.Taken,
			DriftOK:      !d.Taken || d.OK,
			AutoSelected: selected[c.Name],
			SplitChanges: splitChanges[family],
		}
		if d.Taken && !d.OK {
			entry.Current = d.Current
			entry.Expected = d.Expected
		}
		surface.Clients = append(surface.Clients, entry)
	}
	return surface, nil
}

// takeoverTemplateSource normalizes Template.Source ("preset" or the user
// templates directory path) to the wire values preset|user.
func takeoverTemplateSource(source string) string {
	if source == "preset" {
		return "preset"
	}
	return "user"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RunTakeover executes a takeover run (the daemon twin of the CLI command).
// mode "" defaults to unified — the Web transport has no interactive prompt.
func (s *Service) RunTakeover(client, mode string) (appapi.TakeoverRunResult, error) {
	result := appapi.TakeoverRunResult{
		Status:   "ok",
		Applied:  []appapi.TakeoverApplied{},
		Skipped:  []string{},
		Warnings: []string{},
	}
	resolved, err := resolveTakeoverMode(mode)
	if err != nil {
		return result, err
	}
	cfg := s.ports.Config()
	homeDir, templatesDir, bakDir := s.takeoverDirs()
	report, err := takeover.RunTakeoverReport(
		cfg, client, bakDir,
		takeover.ModelFactsFor(cfg, client, homeDir, templatesDir, resolved),
		templatesDir, resolved,
	)
	if err != nil {
		return result, err
	}
	for _, a := range report.Applied {
		result.Applied = append(result.Applied, appapi.TakeoverApplied{Name: a.Name, Note: a.Note})
	}
	result.Skipped = append(result.Skipped, report.Skipped...)
	result.Warnings = append(result.Warnings, report.Warnings...)
	return result, nil
}

// resolveTakeoverMode validates the wire mode value; unknown values are a
// client error (400), matching the CLI's usage failure.
func resolveTakeoverMode(mode string) (takeover.ResolveMode, error) {
	switch m := takeover.ResolveMode(mode); m {
	case "", takeover.ModeUnified:
		return takeover.ModeUnified, nil
	case takeover.ModeSplit:
		return m, nil
	default:
		if m.IsProtocol() {
			return m, nil
		}
		return "", appapi.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("unknown takeover mode %q (want unified|split|anthropic|openai|responses)", mode))
	}
}

// RestoreTakeover restores client configs from their takeover backups.
func (s *Service) RestoreTakeover(client string) (appapi.TakeoverRestoreResult, error) {
	result := appapi.TakeoverRestoreResult{Status: "restored", Restored: []string{}, Skipped: []string{}}
	cfg := s.ports.Config()
	_, templatesDir, bakDir := s.takeoverDirs()
	report, err := takeover.RunRestoreReport(cfg, client, bakDir, templatesDir)
	if err != nil {
		return result, err
	}
	result.Restored = append(result.Restored, report.Restored...)
	result.Skipped = append(result.Skipped, report.Skipped...)
	return result, nil
}

// validTemplateName reports whether name is a safe template id: the file base
// name (without .yaml), no separators, no traversal, no leading dot.
func validTemplateName(name string) bool {
	if name == "" || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.ContainsAny(name, `/\`) && !strings.Contains(name, "..")
}

// TakeoverTemplate returns one template's raw YAML: the user override when
// present, else the embedded preset document.
func (s *Service) TakeoverTemplate(name string) (appapi.TakeoverTemplateDoc, error) {
	if !validTemplateName(name) {
		return appapi.TakeoverTemplateDoc{}, appapi.NewHTTPError(http.StatusBadRequest, "invalid template name: "+name)
	}
	_, templatesDir, _ := s.takeoverDirs()
	userPath := filepath.Join(templatesDir, name+".yaml")
	data, err := os.ReadFile(userPath)
	if err == nil {
		return appapi.TakeoverTemplateDoc{Name: name, Source: "user", YAML: string(data), Path: userPath}, nil
	}
	if !os.IsNotExist(err) {
		return appapi.TakeoverTemplateDoc{}, err
	}
	preset, err := takeover.PresetTemplateYAML(name)
	if err != nil {
		return appapi.TakeoverTemplateDoc{}, appapi.NewHTTPError(http.StatusNotFound, "unknown takeover template: "+name)
	}
	return appapi.TakeoverTemplateDoc{Name: name, Source: "preset", YAML: string(preset)}, nil
}

// SaveTakeoverTemplate validates the candidate against the FULL post-save
// template set (parse + multi-variant family protocol constraints) by loading
// an overlay copy — presets plus current user templates with the candidate
// substituted — and only then writes it. Fail-closed: an invalid candidate
// never reaches the user templates directory.
func (s *Service) SaveTakeoverTemplate(name string, data []byte) error {
	if !validTemplateName(name) {
		return appapi.NewHTTPError(http.StatusBadRequest, "invalid template name: "+name)
	}
	_, templatesDir, _ := s.takeoverDirs()
	if err := validateTemplateCandidate(templatesDir, name, data); err != nil {
		return appapi.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if err := os.MkdirAll(templatesDir, 0o700); err != nil {
		return err
	}
	// Same atomic-write discipline as config edits: a crash mid-write must not
	// leave a truncated template that breaks every later LoadTemplates.
	return configedit.AtomicWrite(filepath.Join(templatesDir, name+".yaml"), data)
}

// validateTemplateCandidate loads presets + the current user templates with
// the candidate substituted for <name>.yaml, so parse errors and family
// protocol violations (duplicate/missing protocol in a multi-variant family)
// reject the save before anything is persisted.
func validateTemplateCandidate(templatesDir, name string, data []byte) error {
	tmp, err := os.MkdirTemp("", "takeover-templates-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	entries, err := os.ReadDir(templatesDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		if strings.TrimSuffix(e.Name(), ".yaml") == name {
			continue // replaced by the candidate below
		}
		b, err := os.ReadFile(filepath.Join(templatesDir, e.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(tmp, e.Name()), b, 0o600); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(tmp, name+".yaml"), data, 0o600); err != nil {
		return err
	}
	_, err = takeover.LoadTemplates(tmp)
	return err
}

// DeleteTakeoverTemplate removes a user template. Deleting a preset itself is
// impossible (presets are embedded); deleting the user override that shadows
// one restores the preset.
func (s *Service) DeleteTakeoverTemplate(name string) error {
	if !validTemplateName(name) {
		return appapi.NewHTTPError(http.StatusBadRequest, "invalid template name: "+name)
	}
	_, templatesDir, _ := s.takeoverDirs()
	path := filepath.Join(templatesDir, name+".yaml")
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if _, perr := takeover.PresetTemplateYAML(name); perr == nil {
			return appapi.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("template %q is a built-in preset with no user override — nothing to delete", name))
		}
		return appapi.NewHTTPError(http.StatusNotFound, "unknown takeover template: "+name)
	}
	return os.Remove(path)
}
