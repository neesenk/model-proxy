package guard

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
)

// rules.json is the embedded secret-pattern table. Most rules are a curated
// selection from the pinned gitleaks default rule set (see the upstream and
// attribution fields in the file); the rest (source "model-proxy") are
// hand-written for this proxy's traffic.
//
// Sync process for bumping the gitleaks selection: run
// scripts/sync_guard_rules.sh [tag] (default: the pinned upstream tag). The
// script pulls config/gitleaks.toml from that tag, re-extracts the carried
// gitleaks rules' regex/entropy, and diffs a regenerated candidate against
// rules.json — review the diff, replace rules.json manually, update the rule
// fixtures in scanner_rules_test.go. Selection criteria for adding upstream
// rules: RE2-compatible, value carries a fixed literal prefix usable as a
// prefilter, no file-path/extension context, no generic keywords like
// "api"/"key". No TOML parser in production code.
//
//go:embed rules.json
var rulesJSON []byte

// ruleEntry is one rule as stored in rules.json.
type ruleEntry struct {
	Name     string   `json:"name"`
	Regex    string   `json:"regex"`
	Literals []string `json:"literals"`
	Entropy  *float64 `json:"entropy"`
	Source   string   `json:"source"`
	// SourceRule is the upstream gitleaks rule id (or the rule name itself
	// for model-proxy rules).
	SourceRule string `json:"source_rule"`
}

// rulesFile is the rules.json schema.
type rulesFile struct {
	Upstream    string      `json:"upstream"`
	Attribution string      `json:"attribution"`
	Rules       []ruleEntry `json:"rules"`
}

// rule is one compiled secret-pattern matcher from the embedded table.
type rule struct {
	name string
	re   *regexp.Regexp
	// regex/source keep the rules.json attribution for RuleInfo (the admin
	// security-explain surface shows them as the rule's explanation).
	regex  string
	source string
	// literals are fixed substrings of which every possible match of re
	// contains at least one (refined from the gitleaks keywords; see the
	// required-literal invariant on the old hand-written table: a literal
	// that is not a provable substring of every match silently disables the
	// rule — the per-rule fixtures in scanner_rules_test.go re-run every
	// positive vector through Scan, so a broken literal fails loudly there).
	literals [][]byte
	// entropy, when non-nil, is the minimum Shannon entropy (bits/byte, same
	// semantics as gitleaks) of the matched secret — the first capture group
	// when re has one, the whole match otherwise. Lower-entropy hits are
	// discarded as false positives.
	entropy *float64
	// group reports whether re has at least one capture group. Gitleaks
	// imports capture the secret in group 1 and leave a trailing context
	// byte (quote/whitespace/;) in a non-captured group of the full match;
	// the claimed span must then be the GROUP span — redacting the full
	// match would eat the delimiter and corrupt enclosing JSON (a string
	// losing its closing quote no longer parses, which also downgrades
	// downstream ScanPathsContext strong hits to weak). Rules without a
	// capture group keep claiming the full match: their regex IS the secret
	// shape. Recorded at load so the hot path pays FindSubmatchIndex only
	// for rules that need group coordinates.
	group bool
}

// findIn returns the claim spans of every re match in body that passes the
// entropy post-filter — the group-1 span for rules with a capture group (see
// the group field), the full match otherwise.
func (r rule) findIn(body []byte) [][2]int {
	if !r.group && r.entropy == nil {
		locs := r.re.FindAllIndex(body, -1)
		out := make([][2]int, 0, len(locs))
		for _, loc := range locs {
			out = append(out, [2]int{loc[0], loc[1]})
		}
		return out
	}
	var out [][2]int
	for _, loc := range r.re.FindAllSubmatchIndex(body, -1) {
		gs, ge := loc[0], loc[1]
		if r.group && len(loc) >= 4 && loc[2] >= 0 {
			gs, ge = loc[2], loc[3]
		}
		if r.entropy == nil || shannon(body[gs:ge]) >= *r.entropy {
			out = append(out, [2]int{gs, ge})
		}
	}
	return out
}

// findEach streams the claim spans of every re match in body that passes the
// entropy post-filter to fn, without materializing the match list — scan
// bodies can be adversarially large and FindAllIndex would allocate the full
// span set up front. fn returning false stops iteration. Rescanning from each
// previous match end keeps the total scan linear (same advancement rule as
// FindAllIndex, one byte on empty matches).
//
// The claim span is the group-1 span when re has a capture group (the
// gitleaks trailing-context delimiter belongs to the match but not to the
// secret — claiming it would make Redact eat a closing quote/semicolon and
// corrupt enclosing JSON), the full match otherwise. Iteration still advances
// past the FULL match end, so the trailing context cannot start a second
// overlapping claim.
func (r rule) findEach(body []byte, fn func(start, end int) bool) {
	for pos := 0; pos <= len(body); {
		// ms/me: full match span (drives advancement); gs/ge: claim span.
		var ms, me, gs, ge int
		if r.group {
			loc := r.re.FindSubmatchIndex(body[pos:])
			if loc == nil {
				return
			}
			ms, me, gs, ge = loc[0], loc[1], loc[0], loc[1]
			if len(loc) >= 4 && loc[2] >= 0 {
				gs, ge = loc[2], loc[3]
			}
		} else {
			loc := r.re.FindIndex(body[pos:])
			if loc == nil {
				return
			}
			ms, me, gs, ge = loc[0], loc[1], loc[0], loc[1]
		}
		if r.entropy == nil || shannon(body[pos+gs:pos+ge]) >= *r.entropy {
			if !fn(pos+gs, pos+ge) {
				return
			}
		}
		if next := pos + me; next > pos+ms {
			pos = next
		} else {
			pos = pos + ms + 1
		}
	}
}

// hasLiteral was the per-rule bytes.Contains prefilter; the Scanner now
// prefilters all literals in one automaton pass (see matcher.go).

// mustLoadRules parses and compiles the embedded rule table. Any malformed
// entry is a build-time data bug: fail-closed at package init (the package
// tests also re-validate every entry).
func mustLoadRules() []rule {
	var f rulesFile
	if err := json.Unmarshal(rulesJSON, &f); err != nil {
		panic(fmt.Sprintf("guard: parse embedded rules.json: %v", err))
	}
	if len(f.Rules) == 0 {
		panic("guard: embedded rules.json has no rules")
	}
	rules := make([]rule, 0, len(f.Rules))
	for _, e := range f.Rules {
		if e.Name == "" {
			panic("guard: rule with empty name")
		}
		re, err := regexp.Compile(e.Regex)
		if err != nil {
			panic(fmt.Sprintf("guard: rule %q regex does not compile: %v", e.Name, err))
		}
		if len(e.Literals) == 0 {
			panic(fmt.Sprintf("guard: rule %q has no prefilter literals", e.Name))
		}
		r := rule{name: e.Name, re: re, regex: e.Regex, source: e.Source, entropy: e.Entropy, group: re.NumSubexp() > 0}
		for _, lit := range e.Literals {
			if lit == "" {
				panic(fmt.Sprintf("guard: rule %q has an empty literal", e.Name))
			}
			r.literals = append(r.literals, []byte(lit))
		}
		rules = append(rules, r)
	}
	return rules
}

// embeddedRules is the compiled form of rules.json, shared read-only by every
// Scanner. Order matters: more specific rules come first so they claim
// overlapping byte spans before looser ones.
var embeddedRules = mustLoadRules()
