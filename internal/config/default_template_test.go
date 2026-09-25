package config

import (
	"testing"
)

// TestDefaultTemplateSelfContained guards the shipped template as an
// INDEPENDENTLY curated artifact (new-user defaults + the embedded preset
// catalog): it must load + validate, and every provider block must carry a
// non-empty models list (an empty preset would render as a dead catalog
// entry in `presets list` and the Web Add-provider wizard).
//
// It deliberately does NOT compare against the repo's config.yaml. That file
// is the operator's LIVE config — the daemon itself rewrites
// providers.<name>.models at runtime (Web Status→Models "Refresh" /
// `models refresh`), and the operator hand-edits it — so any equality gate
// between the two failed CI on ordinary usage (observed twice in one day:
// a UI refresh and a manual edit each desynced the lists). Template content
// freshness is a curation decision, not a mechanical sync; the catalog's
// structural integrity is guarded here (valid + non-empty) and in
// internal/presets (every listed preset has a registered implementation).
//
// Loading via LoadConfigFromBytes runs validate(): provider_id must be a
// known id, base URLs must be absolute http(s), billing values legal — a
// template regression of any of those turns this test red.
func TestDefaultTemplateSelfContained(t *testing.T) {
	tpl, err := LoadConfigFromBytes("config.yaml", []byte(DefaultConfigYAML))
	if err != nil {
		t.Fatalf("DefaultConfigYAML no longer parses/validates: %v", err)
	}
	if len(tpl.Providers) == 0 {
		t.Fatal("template has no providers")
	}
	for name, p := range tpl.Providers {
		if len(p.Models) == 0 {
			t.Errorf("template provider %q has an empty models list — every preset must expose models", name)
		}
		if p.Provider == "" {
			t.Errorf("template provider %q: provider_id is empty", name)
		}
	}
}
