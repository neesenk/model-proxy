package takeover

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	configdomain "model-proxy/internal/config"
)

// preview.go — dry-run rendering for the Web takeover confirmation dialog.
// PreviewWrites executes the exact resolution + rewrite path RunTakeover
// would (same ResolveClientsMode selection, same template rendering, same
// sequential merge order for split variants sharing one file), but against
// private copies of the client configs in a temp dir: no real file is read
// after the initial copy, and none is ever written. The returned content is
// byte-identical to what a real takeover would leave behind.

// PreviewWrite is one client config file a takeover would write. Split mode
// may have several variants merging into the same file; Templates lists them
// in application order.
type PreviewWrite struct {
	Templates []string
	Notes     []string
	File      string // the real client config path
	Exists    bool   // the file exists pre-takeover (false = takeover creates it)
	Content   string // the merged result a takeover would leave behind
	format    string // json|toml|env: drives format-aware path replacement
}

// PreviewWrites renders the takeover result for which/mode without touching
// any real file. Errors mirror RunTakeover's resolution errors (unknown
// client, protocol mismatch, template parse failures); a client whose config
// file is absent is NOT an error here — the preview shows the fresh file a
// takeover would create (run mode would skip it in batch, error on a single
// name; preview deliberately stays permissive so the dialog can show what
// WOULD happen once the client is installed).
//
// managedOnly selects the scope: false renders over a private copy of the
// REAL client file (the merged final document a run leaves behind — the
// takeover confirmation dialog); true renders from an EMPTY file (just the
// entries the template itself writes — the template editor's "what this
// template writes" view). Exists still reports the real file's presence in
// both scopes.
func PreviewWrites(cfg *configdomain.Config, which, templatesDir string, mode ResolveMode, facts ModelFacts, managedOnly bool, scope RewriteScope) ([]PreviewWrite, error) {
	return PreviewWritesOpts(cfg, which, templatesDir, TakeoverOptions{Mode: mode, Scope: scope}, facts, managedOnly)
}

// PreviewWritesDraft previews an UNSAVED template draft (the template
// editor's live preview): the draft YAML is written to a private temp
// templates dir under the client's name — overriding that one template
// while everything else (resolution, rendering, redirect safety) is the
// exact production path — and rendered via PreviewWritesOpts. The client
// name must match what the editor edits (the template file name, so
// variant families like pi.yaml carry their whole variants doc).
func PreviewWritesDraft(cfg *configdomain.Config, which, draft string, opts TakeoverOptions, facts ModelFacts, managedOnly bool) ([]PreviewWrite, error) {
	if which == "" || which == "all" {
		return nil, fmt.Errorf("draft preview needs one template name, got %q", which)
	}
	if strings.TrimSpace(draft) == "" {
		return nil, fmt.Errorf("draft preview: empty template body")
	}
	tmp, err := os.MkdirTemp("", "takeover-draft-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, which+".yaml"), []byte(draft), 0o600); err != nil {
		return nil, err
	}
	return PreviewWritesOpts(cfg, which, tmp, opts, facts, managedOnly)
}

// PreviewWritesOpts is PreviewWrites with full options (scope + subset
// selections) — the dialog's partial-takeover preview.
func PreviewWritesOpts(cfg *configdomain.Config, which, templatesDir string, opts TakeoverOptions, facts ModelFacts, managedOnly bool) ([]PreviewWrite, error) {
	opts, err := opts.validated()
	if err != nil {
		return nil, err
	}
	specs, err := ResolveClientsMode(cfg, which, templatesDir, opts.Mode)
	if err != nil {
		return nil, err
	}
	if len(specs) == 0 {
		return []PreviewWrite{}, nil
	}
	tmp, err := os.MkdirTemp("", "takeover-preview-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	// Redirect every unique client file to a private copy. The redirect MUST
	// precede any Rewrite call — a spec writing its real File would mutate the
	// user's config, which is exactly what a preview must never do.
	redirect := map[string]string{}
	var auxReal []struct{ real, name string }
	redirectAux := func(real string) (string, error) {
		if real == "" {
			return "", nil
		}
		if dst, ok := redirect[real]; ok {
			return dst, nil
		}
		dst := filepath.Join(tmp, fmt.Sprintf("%02d-%s", len(redirect), filepath.Base(real)))
		redirect[real] = dst
		return dst, nil
	}
	for _, sp := range specs {
		if _, ok := redirect[sp.File]; ok {
			continue
		}
		dst := filepath.Join(tmp, fmt.Sprintf("%02d-%s", len(redirect), filepath.Base(sp.File)))
		if !managedOnly {
			data, err := os.ReadFile(sp.File)
			switch {
			case err == nil:
				if err := os.WriteFile(dst, data, 0o600); err != nil {
					return nil, err
				}
			case os.IsNotExist(err):
				// absent file: rewrite creates it, same as a real takeover
			default:
				return nil, fmt.Errorf("preview %s: read: %w", sp.File, err)
			}
		}
		// managedOnly: dst stays absent — the rewrite below creates it, so
		// Content carries only the template's own entries.
		redirect[sp.File] = dst
	}
	// Side files the rewrite writes (codex model catalog, claude's separate
	// MCP storage) are redirected the same way: a preview must never touch
	// the real artifacts. MCP aux files are MERGE targets, so a merged-scope
	// preview seeds the private copy from the real file first.
	for _, sp := range specs {
		if sp.Template == nil {
			continue
		}
		if sp.Template.Models != nil && sp.Template.Models.CatalogFile != "" {
			dst, err := redirectAux(expandHome(sp.Template.Models.CatalogFile))
			if err != nil {
				return nil, err
			}
			sp.Template.Models.CatalogFile = dst
		}
		if aux := mcpAuxFile(sp.Template); aux != "" {
			dst, err := redirectAux(aux)
			if err != nil {
				return nil, err
			}
			auxReal = append(auxReal, struct{ real, name string }{aux, sp.Name})
			if !managedOnly {
				if data, err := os.ReadFile(aux); err == nil {
					if err := os.WriteFile(dst, data, 0o600); err != nil {
						return nil, err
					}
				} else if !os.IsNotExist(err) {
					return nil, err
				}
			}
			sp.Template.MCP.File = dst
		}
	}
	// Apply the rewrites in resolution order — split variants sharing one
	// file must merge in the same sequence RunTakeover uses.
	for _, sp := range specs {
		sp.Template.File = redirect[sp.File]
		if err := sp.RewriteOpts(cfg, facts.Meta, facts.Routes, opts); err != nil {
			return nil, fmt.Errorf("preview %s: render: %w", sp.Name, err)
		}
	}
	// Report per FILE (not per spec): variants sharing a file produce one
	// merged document; duplicating it per spec would misrepresent the result.
	byFile := map[string]*PreviewWrite{}
	var order []string
	// The separate MCP storage file is part of what the takeover writes —
	// register it for the report under its REDIRECTED key (that is where the
	// rewrite lands), carrying the REAL path for display.
	for _, aux := range auxReal {
		w := &PreviewWrite{File: aux.real, Templates: []string{aux.name + " (mcp)"}, Notes: []string{}}
		byFile[redirect[aux.real]] = w
		order = append(order, redirect[aux.real])
	}
	for _, sp := range specs {
		key := redirect[sp.File]
		w, ok := byFile[key]
		if !ok {
			w = &PreviewWrite{File: sp.File, Templates: []string{}, Notes: []string{}, format: sp.Template.Format}
			byFile[key] = w
			order = append(order, key)
		}
		w.Templates = append(w.Templates, sp.Name)
		if sp.Note != "" {
			w.Notes = append(w.Notes, sp.Note)
		}
	}
	out := make([]PreviewWrite, 0, len(order))
	for _, key := range order {
		w := byFile[key]
		if _, err := os.Stat(w.File); err == nil {
			w.Exists = true
		}
		data, err := os.ReadFile(key)
		if err != nil {
			// managedOnly previews never create the private copy for a file
			// the scope does not write (scope=mcp skips the main config,
			// scope=model skips a separate MCP aux) — such files carry no
			// report entry at all.
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		w.Content = string(data)
		out = append(out, *w)
	}
	// The redirected side-file paths (codex model catalog) appear verbatim in
	// the rendered client config (model_catalog_json = "<tmp>/…"); show the
	// REAL path a takeover would write instead of the preview's temp path.
	// Replacement is format-aware so backslashes or quotes in the real path do
	// not break JSON/TOML string escaping.
	pathRedirect := map[string]string{}
	for real, tmp := range redirect {
		if real != "" && filepath.Dir(real) != tmp {
			pathRedirect[real] = tmp
		}
	}
	for i := range out {
		out[i].Content = replaceRedirectedPaths(out[i].Content, out[i].format, pathRedirect)
	}
	return out, nil
}

// replaceRedirectedPaths substitutes preview temp paths with the real side-file
// paths they stand in for, respecting the config file format so escaping stays
// valid. The redirect map keys are real paths and values are temp paths.
func replaceRedirectedPaths(content, format string, redirect map[string]string) string {
	switch format {
	case "json":
		return replaceRedirectedPathsJSON(content, redirect)
	case "toml":
		return replaceRedirectedPathsTOML(content, redirect)
	case "env":
		return replaceRedirectedPathsEnv(content, redirect)
	default:
		return replaceRedirectedPathsRaw(content, redirect)
	}
}

func replaceRedirectedPathsJSON(content string, redirect map[string]string) string {
	for real, tmp := range redirect {
		quotedTmp, _ := json.Marshal(tmp)
		quotedReal, _ := json.Marshal(real)
		content = strings.ReplaceAll(content, string(quotedTmp), string(quotedReal))
	}
	return content
}

func replaceRedirectedPathsTOML(content string, redirect map[string]string) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		eq := strings.IndexByte(l, '=')
		if eq < 0 {
			continue
		}
		val := strings.TrimSpace(l[eq+1:])
		for real, tmp := range redirect {
			if val == strconv.Quote(tmp) {
				lines[i] = l[:eq] + "= " + strconv.Quote(real)
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func replaceRedirectedPathsEnv(content string, redirect map[string]string) string {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		eq := strings.IndexByte(l, '=')
		if eq < 0 {
			continue
		}
		val := strings.TrimSpace(l[eq+1:])
		for real, tmp := range redirect {
			if val == tmp {
				lines[i] = l[:eq] + "=" + real
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func replaceRedirectedPathsRaw(content string, redirect map[string]string) string {
	for real, tmp := range redirect {
		content = strings.ReplaceAll(content, tmp, real)
	}
	return content
}
