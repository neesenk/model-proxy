package takeover_test

// codex_compat_test.go pins the Codex compatibility invariants harvested
// from cc-switch's 3.20.x–4.0 changelog (each was a real breakage they had
// to fix against a Codex upgrade). These are template-level contracts: the
// embedded presets must keep holding them as Codex evolves.

import (
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/takeover"
)

// auth.json is Codex's own credential file — newer Codex versions reject
// third-party credential material there, so the takeover must never write
// it: credentials stay proxy-side (PROXY_MANAGED placeholder) and only
// config.toml (+ the model catalog JSON) are touched.
func TestPresets_NeverTouchAuthJSON(t *testing.T) {
	templates, err := takeover.LoadTemplates("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range templates {
		if base := filepath.Base(tmpl.File); base == "auth.json" {
			t.Errorf("template %s targets %s — auth.json is not a credential carrier", tmpl.Name, tmpl.File)
		}
		if tmpl.MCP != nil && tmpl.MCP.File != "" {
			if base := filepath.Base(tmpl.MCP.File); base == "auth.json" {
				t.Errorf("template %s mcp file targets auth.json", tmpl.Name)
			}
		}
		if tmpl.Models != nil && tmpl.Models.CatalogFile != "" {
			if base := filepath.Base(tmpl.Models.CatalogFile); base == "auth.json" {
				t.Errorf("template %s catalog file targets auth.json", tmpl.Name)
			}
		}
	}
}

// Codex template coherence: the top-level model_provider selector and the
// provider section must derive from the SAME provider_id placeholder — if
// they ever drift apart the takeover points Codex at a nonexistent provider
// section. requires_openai_auth marks the section as third-party-auth
// routing (Codex changed this semantics; dropping the flag silently changes
// how Codex authenticates). mcp_servers entries must NOT carry a `type`
// key — newer Codex rejects unknown keys there.
func TestCodexPreset_CompatInvariants(t *testing.T) {
	templates, err := takeover.LoadTemplates("")
	if err != nil {
		t.Fatal(err)
	}
	var codex *takeover.Template
	for _, tmpl := range templates {
		if tmpl.Name == "codex" {
			codex = tmpl
		}
	}
	if codex == nil {
		t.Fatal("codex preset missing")
	}
	if codex.TOML == nil {
		t.Fatal("codex preset is not toml")
	}
	sel, ok := codex.TOML.TopKeys["model_provider"]
	if !ok || !strings.Contains(sel, "{{provider_id}}") {
		t.Errorf("codex model_provider top key = %q (present=%v), want a {{provider_id}} reference", sel, ok)
	}
	sectionFound := false
	for _, s := range codex.TOML.Sections {
		if strings.Contains(s.Name, "{{provider_id}}") {
			sectionFound = true
			if !strings.Contains(s.Body, "requires_openai_auth = true") {
				t.Errorf("codex provider section missing requires_openai_auth = true:\n%s", s.Body)
			}
		}
	}
	if !sectionFound {
		t.Error("codex provider section does not derive from {{provider_id}} — selector and section can drift apart")
	}
	if codex.MCP == nil || codex.MCP.TOMLSection == "" {
		t.Fatal("codex preset lost its mcp block")
	}
	for _, line := range strings.Split(codex.MCP.TOMLBody, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "type") {
			t.Errorf("codex mcp_servers entry declares %q — newer Codex rejects unknown keys in mcp_servers", strings.TrimSpace(line))
		}
	}
}

// Step Code's unified TOML config shares the mcp_servers shape with Codex —
// the same no-`type` invariant applies to every preset's TOML MCP body.
func TestPresets_MCTomlBodiesHaveNoTypeKey(t *testing.T) {
	templates, err := takeover.LoadTemplates("")
	if err != nil {
		t.Fatal(err)
	}
	for _, tmpl := range templates {
		if tmpl.MCP == nil || tmpl.MCP.TOMLBody == "" {
			continue
		}
		for _, line := range strings.Split(tmpl.MCP.TOMLBody, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "type") {
				t.Errorf("template %s mcp toml_body declares %q — strict MCP TOML parsers reject unknown keys", tmpl.Name, strings.TrimSpace(line))
			}
		}
	}
}
