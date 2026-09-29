package routing

import (
	configdomain "model-proxy/internal/config"
)

// policy.go owns the route-tier policy decision: the declarative band match
// (request profile → preferred target) and the application of that preference.
// Both are pure functions over one request's profile — no state, no I/O, no
// model call — so band-only routes keep exact response-cache semantics.
// See docs/architecture/request-routing.md.

// PickBand returns the first band whose conditions all hold for profile. Bands
// are evaluated in order; no match means "keep the scheduled order". A band
// whose target the route does not serve simply never applies (PreferTarget
// returns the order unchanged) — startup surfaces that shape through
// ConfigRoutingWarnings.
func PickBand(policy configdomain.RoutePolicy, profile Profile) (configdomain.RouteTarget, bool) {
	for _, band := range policy.Bands {
		if bandMatches(band.When, profile) {
			return band.Target, true
		}
	}
	return configdomain.RouteTarget{}, false
}

// bandMatches reports whether every non-nil condition holds (AND). An empty
// when would match everything; validate rejects that shape.
func bandMatches(w configdomain.BandWhen, p Profile) bool {
	if w.EstimatedTokensMin != nil && p.EstimatedTokens < *w.EstimatedTokensMin {
		return false
	}
	if w.EstimatedTokensMax != nil && p.EstimatedTokens > *w.EstimatedTokensMax {
		return false
	}
	if w.FollowUp != nil && p.FollowUp != *w.FollowUp {
		return false
	}
	if w.HasTools != nil && p.HasTools != *w.HasTools {
		return false
	}
	if w.HasImage != nil && p.HasImage != *w.HasImage {
		return false
	}
	return true
}

// SelectGrade returns the grade selected by precedence: active latch → selector
// choice → first matching band. The empty string choice means "no selector
// decision". The returned bool is true when latch/choice/band explicitly picks a
// grade; false means the caller should fall back to the route's natural order.
// A latch always wins, even if the grade is empty after filtering; bands and
// selector choices are only honored when they name a declared grade.
func SelectGrade(policy configdomain.RoutePolicy, profile Profile, latched string, choice string) (string, bool) {
	if latched != "" {
		if _, ok := policy.Grades[latched]; ok {
			return latched, true
		}
	}
	if choice != "" {
		if _, ok := policy.Grades[choice]; ok {
			return choice, true
		}
	}
	for _, band := range policy.Bands {
		if !bandMatches(band.When, profile) {
			continue
		}
		if band.Grade != "" {
			if _, ok := policy.Grades[band.Grade]; ok {
				return band.Grade, true
			}
			continue
		}
		grade, ambiguous := GradeForTarget(band.Target, policy.Grades)
		if !ambiguous && grade != "" {
			return grade, true
		}
	}
	return "", false
}

// GradeForTarget returns the unique grade containing target, and whether it
// appears in more than one grade. Validation rejects ambiguous band targets, so
// the ambiguous case is a defensive no-match at runtime.
func GradeForTarget(target configdomain.RouteTarget, grades map[string][]configdomain.RouteTarget) (string, bool) {
	var found string
	for name, targets := range grades {
		for _, t := range targets {
			if t.Provider == target.Provider && t.Model == target.Model {
				if found != "" && found != name {
					return "", true
				}
				found = name
				break
			}
		}
	}
	return found, false
}

// TargetIndex returns the index of the first target matching want in ordered,
// or -1 when want is absent. Matching follows the same model+provider rule as
// PreferTarget (a parent provider name also matches a pooled virtual target),
// so callers can distinguish "no reorder" from "target not present".
func TargetIndex(
	ordered []configdomain.RouteTarget,
	want configdomain.RouteTarget,
	parentOf map[string]string,
) int {
	for i, t := range ordered {
		if t.Model != want.Model {
			continue
		}
		if t.Provider != want.Provider && parentOf[t.Provider] != want.Provider {
			continue
		}
		return i
	}
	return -1
}

// PreferTarget moves the first target matching want to the front of ordered.
// Matching is by model plus provider and follows FilterTargetsByProvider's
// parent rule: a band naming the parent provider also matches a pooled virtual
// target. ordered is returned unchanged when nothing matches — a band must
// never leave the pipeline with zero targets (capability filtering may already
// have removed the preferred one).
func PreferTarget(
	ordered []configdomain.RouteTarget,
	want configdomain.RouteTarget,
	parentOf map[string]string,
) []configdomain.RouteTarget {
	idx := TargetIndex(ordered, want, parentOf)
	if idx <= 0 {
		return ordered
	}
	out := make([]configdomain.RouteTarget, 0, len(ordered))
	out = append(out, ordered[idx])
	out = append(out, ordered[:idx]...)
	out = append(out, ordered[idx+1:]...)
	return out
}
