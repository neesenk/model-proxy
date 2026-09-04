package takeover

import (
	"fmt"
	"sort"
	"strings"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
)

// select.go — protocol-aware client resolution. Takeover writes an agent's
// config so the agent talks to the proxy in ONE protocol; when the agent
// supports several (template variants of a family, e.g. pi / pi-openai /
// pi-responses), the right choice is the protocol the route providers serve
// natively — a native match is a byte-level passthrough, anything else pays
// for cross-protocol conversion and its compatibility edge cases
// (docs/architecture/protocol-conversion.md). This file picks that variant.

// ProtocolCoverage tallies native-protocol reach over the route table's
// primary targets (one per exposed model — the target a request actually
// tries first).
type ProtocolCoverage struct {
	// Counts maps protocol → number of exposed models whose primary target
	// natively speaks it (routing.NativeProtocols).
	Counts map[string]int
	// Total is the number of exposed models considered.
	Total int
	// Unknown lists exposed models whose primary target's native set is
	// unknown (provider missing from config, or no static signal) — they
	// abstain from Counts.
	Unknown []string
	// native records each exposed model's native set for conversion notes.
	native map[string]map[string]bool
}

// protocolCoverage computes coverage over primary targets (lowest priority,
// same rule ExposedModels uses for metadata).
func protocolCoverage(cfg *configdomain.Config, routes map[string][]configdomain.RouteTarget) *ProtocolCoverage {
	cov := &ProtocolCoverage{
		Counts: map[string]int{},
		native: map[string]map[string]bool{},
	}
	for exposed, targets := range routes {
		if len(targets) == 0 {
			continue
		}
		cov.Total++
		set := routing.NativeProtocols(cfg, primaryTarget(targets))
		cov.native[exposed] = set
		if len(set) == 0 {
			cov.Unknown = append(cov.Unknown, exposed)
			continue
		}
		for proto := range set {
			cov.Counts[proto]++
		}
	}
	sort.Strings(cov.Unknown)
	return cov
}

// converts reports the exposed models that would need cross-protocol
// conversion when the agent speaks proto (native set known and proto not in
// it). Unknown-native models are excluded — we can't claim they convert.
func (c *ProtocolCoverage) converts(proto string) []string {
	var out []string
	for exposed, set := range c.native {
		if len(set) > 0 && !set[proto] {
			out = append(out, exposed)
		}
	}
	sort.Strings(out)
	return out
}

// ResolveMode selects how a multi-variant client family is written.
type ResolveMode string

const (
	// ModeUnified writes ONE variant per family — the protocol with the best
	// native coverage; the remaining models ride protocol conversion.
	ModeUnified ResolveMode = "unified"
	// ModeSplit writes one config entry per natively-spoken protocol and
	// partitions the exposed models among them, so every model is a
	// byte-level passthrough. Models with unknown native protocol (and
	// multi-native models) land on the family's default variant.
	ModeSplit ResolveMode = "split"
)

// IsProtocol reports whether m pins a concrete wire protocol
// (anthropic|openai|responses): unified mode with a preference — a family
// that HAS a variant for the protocol is unified into it regardless of
// native coverage; a family that doesn't falls back to the normal
// auto-selection ("没有就用默认方式").
func (m ResolveMode) IsProtocol() bool {
	switch m {
	case "anthropic", "openai", "responses":
		return true
	}
	return false
}

// variantForProtocol returns the family variant declaring proto, or nil.
func variantForProtocol(variants []*Template, proto string) *Template {
	for _, v := range variants {
		if v.Protocol == proto {
			return v
		}
	}
	return nil
}

// splitAssignment partitions the exposed models of a family across its
// variants by native protocol. Preference order: the default variant (named
// like the family) first, then the rest by name — a model whose provider
// natively speaks several protocols (or whose native protocol is unknown, or
// matches no variant) always lands on the default. Variants assigned no
// models are dropped by the caller.
func splitAssignment(family string, variants []*Template, cov *ProtocolCoverage) map[string]map[string]bool {
	ordered := make([]*Template, 0, len(variants))
	for _, v := range variants {
		if v.Name == family {
			ordered = append([]*Template{v}, ordered...)
		} else {
			ordered = append(ordered, v)
		}
	}
	def := ordered[0]
	out := map[string]map[string]bool{}
	for exposed, set := range cov.native {
		picked := def
		if len(set) > 0 {
			for _, v := range ordered {
				if set[v.Protocol] {
					picked = v
					break
				}
			}
		}
		if out[picked.Name] == nil {
			out[picked.Name] = map[string]bool{}
		}
		out[picked.Name][exposed] = true
	}
	return out
}

// splitNote summarizes the partition for the log, one segment per variant.
func splitNote(family string, assignment map[string]map[string]bool) string {
	names := make([]string, 0, len(assignment))
	for name := range assignment {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		models := make([]string, 0, len(assignment[name]))
		for m := range assignment[name] {
			models = append(models, m)
		}
		sort.Strings(models)
		parts = append(parts, fmt.Sprintf("%s: [%s]", name, strings.Join(models, ", ")))
	}
	return fmt.Sprintf("client %s: split by native protocol — %s", family, strings.Join(parts, "; "))
}

// selectVariant picks one template from a family: the variant whose declared
// protocol has the best native coverage. Ties (including "no signal at all")
// break to the default variant — the one named exactly like the family — then
// to the alphabetically first (variants arrive sorted by name).
func selectVariant(family string, variants []*Template, cov *ProtocolCoverage) *Template {
	best := variants[0]
	bestScore := -1
	for _, v := range variants {
		score := cov.Counts[v.Protocol]
		if score > bestScore ||
			(score == bestScore && best.Name != family && v.Name == family) {
			best, bestScore = v, score
		}
	}
	return best
}

// selectionNote explains an auto-selection in one log line: why this variant,
// and which exposed models will still ride the converter.
func selectionNote(family string, picked *Template, cov *ProtocolCoverage) string {
	if cov.Total == 0 {
		return fmt.Sprintf("client %s: using %s (no routes configured — default variant)", family, picked.Name)
	}
	score := cov.Counts[picked.Protocol]
	note := fmt.Sprintf("client %s: using %s — protocol %s natively covers %d/%d exposed models",
		family, picked.Name, picked.Protocol, score, cov.Total)
	if conv := cov.converts(picked.Protocol); len(conv) > 0 {
		note += fmt.Sprintf("; conversion needed for: %s", strings.Join(conv, ", "))
	}
	if len(cov.Unknown) > 0 {
		note += fmt.Sprintf("; native protocol unknown for: %s", strings.Join(cov.Unknown, ", "))
	}
	return note
}

// groupFamilies buckets templates by client family, preserving the
// sorted-by-name template order inside each family and returning family ids
// sorted as well.
func groupFamilies(templates []*Template) (families []string, byFamily map[string][]*Template) {
	byFamily = map[string][]*Template{}
	for _, t := range templates {
		f := t.ClientFamily()
		if _, ok := byFamily[f]; !ok {
			families = append(families, f)
		}
		byFamily[f] = append(byFamily[f], t)
	}
	sort.Strings(families)
	return families, byFamily
}

// AutoSelectedNames returns the template names ResolveClients would pick for
// ""/"all" — the `takeover list` marker set. Resolution failures degrade to
// an empty set (list still prints the templates, just without markers).
func AutoSelectedNames(cfg *configdomain.Config, templatesDir string) map[string]bool {
	out := map[string]bool{}
	clients, err := ResolveClients(cfg, "", templatesDir)
	if err != nil {
		return out
	}
	for _, c := range clients {
		out[c.Name] = true
	}
	return out
}

// ResolveClients resolves the client set for a takeover run in unified mode
// (see ResolveClientsMode).
func ResolveClients(cfg *configdomain.Config, which, templatesDir string) ([]ClientSpec, error) {
	return ResolveClientsMode(cfg, which, templatesDir, ModeUnified)
}

// ResolveClientsMode resolves the client set for a takeover run. A
// client-family name with several template variants (pi, opencode) — or
// ""/"all" — resolves per family by native-protocol coverage over the route
// table:
//
//   - ModeUnified picks ONE variant per family (best native coverage, ties
//     break to the default variant), so a multi-protocol agent is configured
//     with the protocol its providers serve natively instead of every variant
//     at once (which wrote several provider entries into the same client
//     file). Models outside that protocol ride protocol conversion.
//   - ModeSplit emits one spec per natively-spoken protocol, each carrying
//     only its assigned exposed models (splitAssignment) — every model is a
//     passthrough. Variants assigned no models are skipped.
//   - A protocol value (anthropic|openai|responses) is unified with a
//     pinned preference: families with a variant for that protocol use it
//     regardless of coverage; families without one fall back to
//     ModeUnified auto-selection.
//
// An exact template name that is not a multi-variant family (pi-openai,
// claude) pins that template regardless of mode. The selection rationale
// rides on ClientSpec.Note for RunTakeover to log.
func ResolveClientsMode(cfg *configdomain.Config, which, templatesDir string, mode ResolveMode) ([]ClientSpec, error) {
	if templatesDir == "" {
		templatesDir = DefaultTemplatesDir()
	}
	templates, err := LoadTemplates(templatesDir)
	if err != nil {
		return nil, err
	}
	switch mode {
	case ModeUnified, ModeSplit:
	default:
		if !mode.IsProtocol() {
			return nil, fmt.Errorf("unknown takeover mode %q (want unified|split|anthropic|openai|responses)", mode)
		}
	}
	families, byFamily := groupFamilies(templates)
	if which != "" && which != "all" {
		if variants, ok := byFamily[which]; ok && len(variants) > 1 {
			// Multi-variant family name (which is also the default variant's
			// template name — pi, opencode): auto-select below.
			families = []string{which}
		} else {
			for _, t := range templates {
				if t.Name == which {
					if mode.IsProtocol() && t.Protocol != "" && t.Protocol != string(mode) {
						return nil, fmt.Errorf("template %s speaks protocol %q, not %q — drop --mode or name the %s variant", t.Name, t.Protocol, mode, mode)
					}
					return []ClientSpec{{Name: t.Name, File: t.File, Template: t, Rewrite: t.Rewrite}}, nil
				}
			}
			if _, ok := byFamily[which]; !ok {
				names := make([]string, 0, len(templates))
				for _, t := range templates {
					names = append(names, t.Name)
				}
				return nil, fmt.Errorf("unknown takeover client %q — available templates: %s; client families: %s",
					which, strings.Join(names, ", "), strings.Join(families, ", "))
			}
			// Single-variant family whose name differs from its template.
			families = []string{which}
		}
	}
	cov := protocolCoverage(cfg, routing.RouteTable(cfg))
	out := make([]ClientSpec, 0, len(families))
	for _, family := range families {
		variants := byFamily[family]
		if mode.IsProtocol() {
			picked := variantForProtocol(variants, string(mode))
			note := ""
			switch {
			case picked != nil:
				note = fmt.Sprintf("client %s: using %s — protocol %s pinned by --mode", family, picked.Name, mode)
				if conv := cov.converts(string(mode)); len(conv) > 0 {
					note += fmt.Sprintf("; conversion needed for: %s", strings.Join(conv, ", "))
				}
			case len(variants) > 1:
				picked = selectVariant(family, variants, cov)
				note = selectionNote(family, picked, cov) + fmt.Sprintf(" (no %s variant — auto-selected)", mode)
			default:
				picked = variants[0]
			}
			out = append(out, ClientSpec{
				Name:     picked.Name,
				File:     picked.File,
				Template: picked,
				Rewrite:  picked.Rewrite,
				Note:     note,
			})
			continue
		}
		if len(variants) > 1 && mode == ModeSplit && cov.Total > 0 {
			assignment := splitAssignment(family, variants, cov)
			note := splitNote(family, assignment)
			noted := false
			for _, v := range variants {
				models := assignment[v.Name]
				if len(models) == 0 {
					continue // no empty provider entries
				}
				v := v
				spec := ClientSpec{
					Name:     v.Name,
					File:     v.File,
					Template: v,
					Rewrite: func(cfg *configdomain.Config, meta map[string]map[string]catalog.Model, routes map[string][]configdomain.RouteTarget) error {
						return v.RewriteFiltered(cfg, meta, routes, models)
					},
				}
				if !noted {
					spec.Note = note
					noted = true
				}
				out = append(out, spec)
			}
			continue
		}
		picked := variants[0]
		note := ""
		if len(variants) > 1 {
			picked = selectVariant(family, variants, cov)
			note = selectionNote(family, picked, cov)
		}
		out = append(out, ClientSpec{
			Name:     picked.Name,
			File:     picked.File,
			Template: picked,
			Rewrite:  picked.Rewrite,
			Note:     note,
		})
	}
	return out, nil
}

// SplitWouldChange reports whether split mode would resolve a different
// client set than unified for this config — the condition under which the
// CLI asks the user to choose. Resolution failures degrade to false (no
// prompt; unified stays the default).
func SplitWouldChange(cfg *configdomain.Config, which, templatesDir string) bool {
	unified, err := ResolveClientsMode(cfg, which, templatesDir, ModeUnified)
	if err != nil {
		return false
	}
	split, err := ResolveClientsMode(cfg, which, templatesDir, ModeSplit)
	if err != nil {
		return false
	}
	if len(split) != len(unified) {
		return true
	}
	for i := range unified {
		if unified[i].Name != split[i].Name {
			return true
		}
	}
	return false
}
