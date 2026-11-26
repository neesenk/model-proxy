package routing

import (
	"bytes"
	"fmt"
	"strings"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// Profile contains the request-derived facts used by capability, context and
// route-policy (bands) routing. A caller should compute it once and reuse it
// across candidate checks.
type Profile struct {
	HasImage        bool
	HasTools        bool
	EstimatedTokens int64
	// FollowUp reports that the body already carries an assistant turn — the
	// request is a later turn of an ongoing conversation rather than a
	// first-turn prompt. Marker-based like the other facts (no JSON parse on
	// the hot path); see RoutePolicy bands in docs/architecture/request-routing.md.
	FollowUp bool
}

// ProfileRequest derives the routing profile from a raw client request body.
// The three marker flags come from one pass (scanProfileMarkers) instead of a
// full-body search per marker spelling.
func ProfileRequest(body []byte) Profile {
	markers := scanProfileMarkers(body)
	return Profile{
		HasImage:        markers.image,
		HasTools:        markers.tools,
		EstimatedTokens: EstimateInputTokens(body),
		FollowUp:        markers.followUp,
	}
}

// profileMarkers are the literal-existence facts of a body.
type profileMarkers struct{ image, tools, followUp bool }

// scanProfileMarkers reports whether any image marker, any tools marker and
// any assistant-role marker (a follow-up turn) occurs in body — literal
// existence checks in ONE pass: every marker opens with a quote, so the scan
// visits each quote once, tests the marker sets by the byte after it, and
// stops once all three are found.
func scanProfileMarkers(body []byte) profileMarkers {
	var found profileMarkers
	hasAny := func(at int, markers [][]byte) bool {
		for _, marker := range markers {
			if bytes.HasPrefix(body[at:], marker) {
				return true
			}
		}
		return false
	}
	for i := 0; !(found.image && found.tools && found.followUp); {
		q := bytes.IndexByte(body[i:], '"')
		if q < 0 {
			break
		}
		at := i + q
		i = at + 1
		if i >= len(body) {
			break
		}
		switch body[i] {
		case 't':
			found.image = found.image || hasAny(at, imageMarkers)
			found.tools = found.tools || hasAny(at, toolsMarkers)
		case 's':
			found.image = found.image || hasAny(at, imageMarkers)
		case 'r':
			found.followUp = found.followUp || hasAny(at, assistantMarkers)
		}
	}
	return found
}

// RoleCounts tallies the message-role composition of a request body — the
// observability projection behind the request log's context-composition line
// (which slice of a conversation the request carries). Tool counts the
// tool-position forms of all three protocols: the openai tool/function roles
// and anthropic tool_result blocks (anthropic has no tool role; its tool
// results ride user messages as typed content blocks).
type RoleCounts struct {
	System    int `json:"system,omitempty"`
	User      int `json:"user,omitempty"`
	Assistant int `json:"assistant,omitempty"`
	Tool      int `json:"tool,omitempty"`
}

// roleMarker pairs one counted spelling (compact and spaced JSON) with its
// bucket. Marker-based like the other body facts: no JSON parse, counts are
// non-overlapping occurrences of disjoint literals.
type roleMarker struct {
	marker []byte
	add    func(*RoleCounts, int)
}

// countRolesMarkers is the closed marker table; built once, read-only.
var countRolesMarkers = buildRoleMarkers()

func buildRoleMarkers() []roleMarker {
	pair := func(role string, add func(*RoleCounts, int)) []roleMarker {
		return []roleMarker{
			{[]byte(`"role":"` + role + `"`), add},
			{[]byte(`"role": "` + role + `"`), add},
		}
	}
	markers := []roleMarker{}
	markers = append(markers, pair("system", func(r *RoleCounts, n int) { r.System += n })...)
	// The openai developer role is system-phrased guidance; count it as system.
	markers = append(markers, pair("developer", func(r *RoleCounts, n int) { r.System += n })...)
	// Anthropic carries no system role: guidance rides the top-level "system"
	// field (string or content-block array). These spellings would only
	// false-positive on content that embeds a literal `"system":` JSON key —
	// rare enough for an observability count.
	markers = append(markers,
		roleMarker{[]byte(`"system":"`), func(r *RoleCounts, n int) { r.System += n }},
		roleMarker{[]byte(`"system": [`), func(r *RoleCounts, n int) { r.System += n }},
	)
	markers = append(markers, pair("user", func(r *RoleCounts, n int) { r.User += n })...)
	markers = append(markers, pair("assistant", func(r *RoleCounts, n int) { r.Assistant += n })...)
	markers = append(markers, pair("tool", func(r *RoleCounts, n int) { r.Tool += n })...)
	markers = append(markers, pair("function", func(r *RoleCounts, n int) { r.Tool += n })...)
	markers = append(markers,
		roleMarker{[]byte(`"type":"tool_result"`), func(r *RoleCounts, n int) { r.Tool += n }},
		roleMarker{[]byte(`"type": "tool_result"`), func(r *RoleCounts, n int) { r.Tool += n }},
	)
	for _, entry := range markers {
		// CountRoles' single pass visits quote positions only.
		if len(entry.marker) < 2 || entry.marker[0] != '"' {
			panic("routing: role marker must open with a quote: " + string(entry.marker))
		}
	}
	return markers
}

// CountRoles counts the role composition of body in ONE pass. Every marker
// opens with a quote, so the scan visits each quote once and tests only the
// markers whose second byte matches the byte after it. Counting stays
// per-marker and non-overlapping (a marker may not start inside its own
// previous match; different markers may share bytes) — exactly what one
// bytes.Count per marker computed, without sixteen full-body sweeps that all
// key on the most frequent byte in JSON.
func CountRoles(body []byte) RoleCounts {
	var counts RoleCounts
	nextFree := make([]int, len(countRolesMarkers))
	for i := 0; ; {
		q := bytes.IndexByte(body[i:], '"')
		if q < 0 {
			return counts
		}
		at := i + q
		i = at + 1
		if i >= len(body) {
			return counts
		}
		second := body[i]
		for m, entry := range countRolesMarkers {
			if entry.marker[1] != second || at < nextFree[m] || !bytes.HasPrefix(body[at:], entry.marker) {
				continue
			}
			entry.add(&counts, 1)
			nextFree[m] = at + len(entry.marker)
		}
	}
}

// assistantMarkers are the byte forms of an assistant-role message across the
// three client protocols (anthropic/openai messages, responses input). Spacing
// variants are listed explicitly — same style as imageMarkers.
var assistantMarkers = [][]byte{
	[]byte(`"role":"assistant"`),
	[]byte(`"role": "assistant"`),
}

// Fits reports whether model can serve profile according to an authoritative
// per-model capability declaration and the models.dev catalog.
func Fits(cat *catalog.Catalog, capabilities map[string][]string, model string, profile Profile) bool {
	ok, _ := FitVerdict(cat, capabilities, model, profile)
	return ok
}

// FitVerdict is Fits plus the reason for the verdict: declared capabilities
// outrank the catalog, and the context check reports the estimate/window
// ratio even on success. It backs the /debug/route preview; the scheduling
// hot path uses Fits (same semantics, no reason strings).
func FitVerdict(cat *catalog.Catalog, capabilities map[string][]string, model string, profile Profile) (bool, string) {
	if declared, ok := capabilities[model]; ok {
		if profile.HasImage && !hasCapability(declared, "image") {
			return false, "image capability not declared"
		}
		if profile.HasTools && !hasCapability(declared, "tools") {
			return false, "tools capability not declared"
		}
	} else if cat != nil {
		if profile.HasImage {
			modelMeta, ok := cat.Lookup(model)
			if !ok || !supportsImage(modelMeta) {
				return false, "model metadata lacks image modality"
			}
		}
		if profile.HasTools {
			modelMeta, ok := cat.Lookup(model)
			if !ok || !modelMeta.ToolCall {
				return false, "model metadata lacks tool calling"
			}
		}
	}
	if profile.EstimatedTokens > 0 {
		if modelMeta, ok := lookupModelMeta(cat, model); ok && modelMeta.Context > 0 {
			if profile.EstimatedTokens > modelMeta.Context {
				return false, fmt.Sprintf("estimated %d tokens exceed context window %d", profile.EstimatedTokens, modelMeta.Context)
			}
			return true, fmt.Sprintf("estimated %d / context %d tokens", profile.EstimatedTokens, modelMeta.Context)
		}
	}
	return true, ""
}

// CapabilitiesFor returns the provider-level capability declaration for target.
// Credential-pool virtual provider IDs resolve through parentOf.
func CapabilitiesFor(
	cfg *configdomain.Config,
	parentOf map[string]string,
	target configdomain.RouteTarget,
) map[string][]string {
	if cfg == nil {
		return nil
	}
	providerName := target.Provider
	if parent := parentOf[providerName]; parent != "" {
		providerName = parent
	}
	providerConfig, ok := cfg.Providers[providerName]
	if !ok {
		return nil
	}
	return providerConfig.Capabilities
}

// ImageOKForTarget reports whether image content may be preserved when
// converting a request for target. Unlike proactive selection, unknown metadata
// is permissive here because the alternative is silently dropping user content.
func ImageOKForTarget(
	cfg *configdomain.Config,
	parentOf map[string]string,
	cat *catalog.Catalog,
	target configdomain.RouteTarget,
) bool {
	if capabilities := CapabilitiesFor(cfg, parentOf, target); capabilities != nil {
		if declared, ok := capabilities[target.Model]; ok {
			return hasCapability(declared, "image")
		}
	}
	if modelMeta, ok := lookupModelMeta(cat, target.Model); ok {
		return supportsImage(modelMeta)
	}
	return true
}

// toolsMarkers are the spellings of a non-empty-or-empty tools array.
var toolsMarkers = [][]byte{
	[]byte(`"tools":[`),
	[]byte(`"tools": [`),
}

// EstimateInputTokens returns the request-routing prompt-size estimate. CJK
// runes count as one token, other bytes as roughly one token per four bytes,
// and long base64 runs are excluded.
//
// Linear: a base64 run is measured ONCE from the scan position and then
// skipped whole (>= base64RunMin characters: encoded payload) or counted
// whole (shorter: each character is one ASCII byte) — exactly what checking
// the remaining run at every byte decided, without re-scanning it per byte.
// The run is measured from the scan position, not from the run's start, so a
// multi-byte lead byte that consumed the head of a run (invalid UTF-8) leaves
// the remainder judged on its own, as before.
func EstimateInputTokens(body []byte) int64 {
	var cjkRunes, otherBytes int64
	for index := 0; index < len(body); {
		c := body[index]
		if base64Chars[c] {
			end := index + 1
			for end < len(body) && base64Chars[body[end]] {
				end++
			}
			if end-index < base64RunMin {
				otherBytes += int64(end - index)
			}
			index = end
			continue
		}
		if c < 0x80 {
			otherBytes++
			index++
			continue
		}
		decoded, size := decodeRune(body, index)
		if isCJK(rune(decoded)) {
			cjkRunes++
		} else {
			otherBytes += int64(size)
		}
		index += size
	}
	return cjkRunes + otherBytes/4
}

// base64Chars is isBase64Char as a lookup table for the estimator's hot loop.
var base64Chars = func() (table [256]bool) {
	for c := 0; c < 256; c++ {
		table[c] = isBase64Char(byte(c))
	}
	return table
}()

// FilterTargetsByProvider narrows targets to provider, matching pooled virtual
// targets by either their virtual ID or parent provider name.
func FilterTargetsByProvider(
	targets []configdomain.RouteTarget,
	parentOf map[string]string,
	provider string,
) []configdomain.RouteTarget {
	var filtered []configdomain.RouteTarget
	for _, target := range targets {
		if target.Provider == provider || parentOf[target.Provider] == provider {
			filtered = append(filtered, target)
		}
	}
	return filtered
}

// SplitProviderPrefix decomposes a "provider/model" called name into its
// provider prefix and bare model — but only when the prefix is a configured
// provider, so model names that legitimately contain "/" (and have no
// matching provider) pass through untouched. Callers must try the exact
// route key FIRST: an explicit route named "p/m" always wins over the
// provider-prefix reading.
func SplitProviderPrefix(providers map[string]configdomain.Provider, calledModel string) (provider, model string, ok bool) {
	p, m, found := strings.Cut(calledModel, "/")
	if !found || p == "" || m == "" {
		return "", "", false
	}
	if _, isProvider := providers[p]; !isProvider {
		return "", "", false
	}
	return p, m, true
}

// CollectCrossRoute gathers matching concrete targets from all expanded routes.
// Identity ignores Priority; duplicate identities keep their lowest priority.
func CollectCrossRoute(
	expanded map[string][]configdomain.RouteTarget,
	keep func(configdomain.RouteTarget) bool,
) []configdomain.RouteTarget {
	type targetKey struct {
		provider string
		model    string
		protocol string
	}
	best := make(map[targetKey]configdomain.RouteTarget)
	for _, targets := range expanded {
		for _, target := range targets {
			if keep != nil && !keep(target) {
				continue
			}
			key := targetKey{
				provider: target.Provider,
				model:    target.Model,
				protocol: target.Protocol,
			}
			if existing, ok := best[key]; !ok || target.Priority < existing.Priority {
				best[key] = target
			}
		}
	}
	collected := make([]configdomain.RouteTarget, 0, len(best))
	for _, target := range best {
		collected = append(collected, target)
	}
	return collected
}

// Scheduler is the narrow stateful port used to rank a cross-route pool.
// Implementations bind route keys, scheduling settings and runtime generation.
type Scheduler interface {
	Schedule(routeName, sessionKey string, targets []configdomain.RouteTarget) []configdomain.RouteTarget
}

// Planner owns request-aware filtering and context-overflow replacement policy.
// Its inputs are one immutable runtime generation; stateful ordering is
// delegated through Scheduler.
type Planner struct {
	config         *configdomain.Config
	parentOf       map[string]string
	catalog        *catalog.Catalog
	expandedRoutes map[string][]configdomain.RouteTarget
	scheduler      Scheduler
}

// PlannerInput is the immutable-generation projection required to construct a
// Planner. Callers retain ownership of the maps; the proxy runtime contract
// guarantees generation-owned maps are never mutated in place.
type PlannerInput struct {
	Config         *configdomain.Config
	ParentOf       map[string]string
	Catalog        *catalog.Catalog
	ExpandedRoutes map[string][]configdomain.RouteTarget
	Scheduler      Scheduler
}

// NewPlanner freezes a caller's runtime-generation projection behind a
// read-only policy API.
func NewPlanner(input PlannerInput) Planner {
	return Planner{
		config:         input.Config,
		parentOf:       input.ParentOf,
		catalog:        input.Catalog,
		expandedRoutes: input.ExpandedRoutes,
		scheduler:      input.Scheduler,
	}
}

// preferRouteProviders applies the fallback ladder's first rule: targets on
// the original route's providers — a fitting sibling model on a provider the
// route already trusts — come before everything else, regardless of tier.
// Within each group the scheduler's own order (tier plan→pay, then priority,
// then quota surplus) is preserved: a stable partition of the scheduled list,
// no scheduler semantics touched.
func preferRouteProviders(scheduled []configdomain.RouteTarget, routeProviders map[string]bool) []configdomain.RouteTarget {
	if len(scheduled) < 2 || len(routeProviders) == 0 {
		return scheduled
	}
	same := make([]configdomain.RouteTarget, 0, len(scheduled))
	rest := make([]configdomain.RouteTarget, 0, len(scheduled))
	for _, t := range scheduled {
		if routeProviders[t.Provider] {
			same = append(same, t)
		} else {
			rest = append(rest, t)
		}
	}
	if len(same) == 0 || len(rest) == 0 {
		return scheduled
	}
	return append(same, rest...)
}

func routeProviderSet(targets []configdomain.RouteTarget) map[string]bool {
	set := make(map[string]bool, len(targets))
	for _, t := range targets {
		set[t.Provider] = true
	}
	return set
}

// ApplyWithProfile narrows ordered to in-route targets that fit a pre-computed
// request profile: the forward path profiles the (immutable) request body once
// and reuses it across the wait-retry rounds and the context-overflow retry
// instead of re-scanning. If none fit, it schedules a cross-route pool under
// the synthetic "#req" route key. A nil catalog is a complete no-op, and an
// empty scheduled fallback returns ordered.
func (planner Planner) ApplyWithProfile(
	exposed, sessionKey string,
	ordered []configdomain.RouteTarget,
	profile Profile,
) []configdomain.RouteTarget {
	if planner.catalog == nil {
		return ordered
	}
	inRoute := make([]configdomain.RouteTarget, 0, len(ordered))
	for _, target := range ordered {
		if Fits(
			planner.catalog,
			CapabilitiesFor(planner.config, planner.parentOf, target),
			target.Model,
			profile,
		) {
			inRoute = append(inRoute, target)
		}
	}
	if len(inRoute) == len(ordered) {
		return ordered
	}
	if len(inRoute) > 0 {
		return inRoute
	}
	pool := CollectCrossRoute(
		planner.expandedRoutes,
		func(target configdomain.RouteTarget) bool {
			return Fits(
				planner.catalog,
				CapabilitiesFor(planner.config, planner.parentOf, target),
				target.Model,
				profile,
			)
		},
	)
	scheduled := planner.schedule(exposed+"#req", sessionKey, pool)
	if len(scheduled) == 0 {
		return ordered
	}
	return preferRouteProviders(scheduled, routeProviderSet(ordered))
}

// DescribeFitReroute reports whether request-aware routing changed the
// target set and, if so, builds the trace for the request log: the fit verdict
// of every dropped in-route target, plus the cross-route replacement head when
// the whole chain was replaced. ("", false) when the set survived unchanged —
// the overwhelmingly common case pays one slice compare, no verdicts.
func (planner Planner) DescribeFitReroute(before, after []configdomain.RouteTarget, profile Profile) (string, bool) {
	if len(after) == len(before) {
		same := true
		for i := range before {
			if before[i].Provider != after[i].Provider || before[i].Model != after[i].Model {
				same = false
				break
			}
		}
		if same {
			return "", false
		}
	}
	kept := make(map[string]bool, len(after))
	for _, t := range after {
		kept[t.Provider+"/"+t.Model] = true
	}
	var reasons []string
	for _, t := range before {
		if kept[t.Provider+"/"+t.Model] {
			continue
		}
		_, why := FitVerdict(
			planner.catalog,
			CapabilitiesFor(planner.config, planner.parentOf, t),
			t.Model,
			profile,
		)
		if why == "" {
			why = "unfit"
		}
		reasons = append(reasons, t.Provider+"/"+t.Model+": "+why)
	}
	if len(reasons) == 0 {
		return "", false // reordering only: same target set, no fit verdicts to trace
	}
	note := "request-aware: dropped " + strings.Join(reasons, "; ")
	if !kept[before[0].Provider+"/"+before[0].Model] && len(after) > 0 {
		note += fmt.Sprintf(" → cross-route %s/%s", after[0].Provider, after[0].Model)
	}
	return note, true
}

// ContextOverflowRetryWithProfile schedules, over a shared pre-computed
// profile (see ApplyWithProfile), cross-route targets whose known context is
// strictly larger than the largest known context among tried and whose
// capability/context profile still fits. It uses the synthetic "#ctx" key.
func (planner Planner) ContextOverflowRetryWithProfile(
	exposed, sessionKey string,
	tried []configdomain.RouteTarget,
	profile Profile,
) []configdomain.RouteTarget {
	if planner.catalog == nil {
		return nil
	}
	var maxContext int64
	for _, target := range tried {
		if modelMeta, ok := lookupModelMeta(planner.catalog, target.Model); ok &&
			modelMeta.Context > maxContext {
			maxContext = modelMeta.Context
		}
	}
	if maxContext == 0 {
		return nil
	}
	pool := CollectCrossRoute(
		planner.expandedRoutes,
		func(target configdomain.RouteTarget) bool {
			modelMeta, ok := lookupModelMeta(planner.catalog, target.Model)
			if !ok || modelMeta.Context <= maxContext {
				return false
			}
			return Fits(
				planner.catalog,
				CapabilitiesFor(planner.config, planner.parentOf, target),
				target.Model,
				profile,
			)
		},
	)
	return preferRouteProviders(planner.schedule(exposed+"#ctx", sessionKey, pool), routeProviderSet(tried))
}

func (planner Planner) schedule(
	routeName, sessionKey string,
	targets []configdomain.RouteTarget,
) []configdomain.RouteTarget {
	if len(targets) == 0 || planner.scheduler == nil {
		return nil
	}
	return planner.scheduler.Schedule(routeName, sessionKey, targets)
}

func lookupModelMeta(cat *catalog.Catalog, model string) (catalog.Model, bool) {
	return cat.Lookup(model)
}

func supportsImage(model catalog.Model) bool {
	for _, input := range model.Modalities.Input {
		if input == "image" {
			return true
		}
	}
	return false
}

func hasCapability(capabilities []string, want string) bool {
	for _, capability := range capabilities {
		if capability == want {
			return true
		}
	}
	return false
}

var imageMarkers = [][]byte{
	[]byte(`"type":"image"`),
	[]byte(`"type": "image"`),
	[]byte(`"type":"image_url"`),
	[]byte(`"type": "image_url"`),
	[]byte(`"type":"input_image"`),
	[]byte(`"type": "input_image"`),
	[]byte(`"source":{"type":"base64"`),
}

func decodeRune(body []byte, index int) (int32, int) {
	first := body[index]
	if first&0x80 == 0 {
		return int32(first), 1
	}
	if first&0xE0 == 0xC0 && index+1 < len(body) {
		return int32(first&0x1F)<<6 | int32(body[index+1]&0x3F), 2
	}
	if first&0xF0 == 0xE0 && index+2 < len(body) {
		return int32(first&0x0F)<<12 |
			int32(body[index+1]&0x3F)<<6 |
			int32(body[index+2]&0x3F), 3
	}
	if first&0xF8 == 0xF0 && index+3 < len(body) {
		return int32(first&0x07)<<18 |
			int32(body[index+1]&0x3F)<<12 |
			int32(body[index+2]&0x3F)<<6 |
			int32(body[index+3]&0x3F), 4
	}
	return int32(first), 1
}

func isCJK(value rune) bool {
	return (value >= 0x4E00 && value <= 0x9FFF) ||
		(value >= 0x3040 && value <= 0x30FF) ||
		(value >= 0xAC00 && value <= 0xD7AF)
}

func isBase64Char(value byte) bool {
	return (value >= 'A' && value <= 'Z') ||
		(value >= 'a' && value <= 'z') ||
		(value >= '0' && value <= '9') ||
		value == '+' ||
		value == '/' ||
		value == '='
}

// base64RunMin is the length from which a run of base64 characters counts as
// encoded payload (an inline image, a blob) rather than prose, and is left
// out of the token estimate.
const base64RunMin = 100
