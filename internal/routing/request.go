package routing

import (
	"bytes"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// Profile contains the request-derived facts used by capability and context
// routing. A caller should compute it once and reuse it across candidate checks.
type Profile struct {
	HasImage        bool
	HasTools        bool
	EstimatedTokens int64
}

// ProfileRequest derives the routing profile from a raw client request body.
func ProfileRequest(body []byte) Profile {
	return Profile{
		HasImage:        requestHasImage(body),
		HasTools:        RequestHasTools(body),
		EstimatedTokens: EstimateInputTokens(body),
	}
}

// Fits reports whether model can serve profile according to an authoritative
// per-model capability declaration and the models.dev catalog.
func Fits(cat *catalog.Catalog, capabilities map[string][]string, model string, profile Profile) bool {
	if declared, ok := capabilities[model]; ok {
		if profile.HasImage && !hasCapability(declared, "image") {
			return false
		}
		if profile.HasTools && !hasCapability(declared, "tools") {
			return false
		}
	} else if cat != nil {
		if profile.HasImage {
			modelMeta, ok := cat.Lookup(model)
			if !ok || !supportsImage(modelMeta) {
				return false
			}
		}
		if profile.HasTools {
			modelMeta, ok := cat.Lookup(model)
			if !ok || !modelMeta.ToolCall {
				return false
			}
		}
	}
	if profile.EstimatedTokens > 0 {
		if modelMeta, ok := lookupModelMeta(cat, model); ok &&
			modelMeta.Context > 0 &&
			profile.EstimatedTokens > modelMeta.Context {
			return false
		}
	}
	return true
}

// FitsRequest profiles body and applies Fits without a manual capability
// override.
func FitsRequest(cat *catalog.Catalog, model string, body []byte) bool {
	return Fits(cat, nil, model, ProfileRequest(body))
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

// RequestHasTools reports whether body contains a top-level tools definition.
func RequestHasTools(body []byte) bool {
	return bytes.Contains(body, []byte(`"tools":[`)) ||
		bytes.Contains(body, []byte(`"tools": [`))
}

// EstimateInputTokens returns the request-routing prompt-size estimate. CJK
// runes count as one token, other bytes as roughly one token per four bytes,
// and long base64 runs are excluded.
func EstimateInputTokens(body []byte) int64 {
	if len(body) == 0 {
		return 0
	}
	var cjkRunes, otherBytes int64
	for index := 0; index < len(body); {
		if isBase64Run(body, index) {
			for index < len(body) && isBase64Char(body[index]) {
				index++
			}
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

// CollectCrossRoute gathers matching concrete targets from all expanded routes.
// Identity ignores Priority; duplicate identities keep their lowest priority.
// Fusion recipes are deliberately route-local and are never collected.
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
			if target.Provider == "fusion" || (keep != nil && !keep(target)) {
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

// SchedulerFunc adapts a function to Scheduler.
type SchedulerFunc func(routeName, sessionKey string, targets []configdomain.RouteTarget) []configdomain.RouteTarget

// Schedule implements Scheduler.
func (schedule SchedulerFunc) Schedule(
	routeName, sessionKey string,
	targets []configdomain.RouteTarget,
) []configdomain.RouteTarget {
	return schedule(routeName, sessionKey, targets)
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

// Apply narrows ordered to in-route targets that fit the request. If none fit,
// it schedules a cross-route pool under the synthetic "#req" route key. A nil
// catalog is a complete no-op, and an empty scheduled fallback returns ordered.
func (planner Planner) Apply(
	exposed, sessionKey string,
	ordered []configdomain.RouteTarget,
	body []byte,
) []configdomain.RouteTarget {
	if planner.catalog == nil {
		return ordered
	}
	profile := ProfileRequest(body)
	inRoute := make([]configdomain.RouteTarget, 0, len(ordered))
	for _, target := range ordered {
		if target.Provider == "fusion" ||
			Fits(
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
	return scheduled
}

// ContextOverflowRetry schedules cross-route targets whose known context is
// strictly larger than the largest known context among tried and whose
// capability/context profile still fits. It uses the synthetic "#ctx" key.
func (planner Planner) ContextOverflowRetry(
	exposed, sessionKey string,
	tried []configdomain.RouteTarget,
	body []byte,
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
	profile := ProfileRequest(body)
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
	return planner.schedule(exposed+"#ctx", sessionKey, pool)
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

func requestHasImage(body []byte) bool {
	for _, marker := range imageMarkers {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
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

func isBase64Run(body []byte, index int) bool {
	end := index
	for end < len(body) && end-index < 120 && isBase64Char(body[end]) {
		end++
	}
	return end-index >= 100
}
