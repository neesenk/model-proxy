package runtime

import (
	"reflect"
	"testing"
	"time"
)

func disabledTestInput(targets ...Target) ScheduleInput {
	return ScheduleInput{
		Exposed:   "m",
		Targets:   targets,
		RouteKeys: map[string]bool{"m": true},
		Dwell:     time.Minute,
		Now:       time.Now(),
	}
}

// TestSetModelDisabledLifecycle pins the override's set/clear/projection
// contract: exact-key lookups, the detached sorted DisabledModels
// projection, and idempotent enable of a never-disabled pair.
func TestSetModelDisabledLifecycle(t *testing.T) {
	m := &Manager{}
	if m.ModelDisabled("zhipu", "glm") {
		t.Fatal("fresh Manager reports a disabled model")
	}
	m.SetModelDisabled("zhipu", "glm-4.7", true)
	m.SetModelDisabled("zhipu", "glm-4.6", true)
	m.SetModelDisabled("kimi", "k3", true)
	if !m.ModelDisabled("zhipu", "glm-4.7") || !m.ModelDisabled("kimi", "k3") {
		t.Fatal("exact-key lookup missed a disabled pair")
	}
	if got := m.DisabledModels(); !reflect.DeepEqual(got, map[string][]string{
		"kimi":  {"k3"},
		"zhipu": {"glm-4.6", "glm-4.7"},
	}) {
		t.Fatalf("DisabledModels() = %v, want sorted provider→models map", got)
	}
	// Empty provider/model are refused (no junk keys).
	m.SetModelDisabled("", "m", true)
	m.SetModelDisabled("p", "", true)
	if _, ok := m.DisabledModels()[""]; ok {
		t.Fatal("empty provider key was installed")
	}

	m.SetModelDisabled("zhipu", "glm-4.7", false)
	if m.ModelDisabled("zhipu", "glm-4.7") {
		t.Fatal("cleared pair still disabled")
	}
	m.SetModelDisabled("gone", "never", false) // idempotent enable
	if _, ok := m.DisabledModels()["gone"]; ok {
		t.Fatal("idempotent enable installed a key")
	}
}

// TestDecideOrderExcludesDisabledTargets pins the scheduling contract: exact
// provider keys and pool PARENT keys both drop the target, facts keep their
// original indices, and ReplaceGeneration preserves the override (the pin
// contract) while clearing generation-scoped state.
func TestDecideOrderExcludesDisabledTargets(t *testing.T) {
	m := &Manager{}
	input := disabledTestInput(
		Target{Provider: "zhipu#a", Parent: "zhipu", Model: "glm", Priority: 1},
		Target{Provider: "zhipu#b", Parent: "zhipu", Model: "glm", Priority: 2},
		Target{Provider: "other", Model: "glm", Priority: 3},
	)

	// Parent-keyed disable covers every pooled virtual without enumeration.
	m.SetModelDisabled("zhipu", "glm", true)
	result := m.DecideOrder(input)
	if len(result.Order) != 1 || input.Targets[result.Order[0]].Provider != "other" {
		t.Fatalf("parent-keyed disable order = %v, want only [other]", result.Order)
	}
	// Facts keep input indices (the survivor is index 2, not renumbered).
	if len(result.Facts) != 3 {
		t.Fatalf("facts len = %d, want 3 (input-aligned projection)", len(result.Facts))
	}

	// Exact virtual-id key also works.
	m.SetModelDisabled("zhipu", "glm", false)
	m.SetModelDisabled("zhipu#b", "glm", true)
	result = m.DecideOrder(input)
	if len(result.Order) != 2 {
		t.Fatalf("virtual-keyed disable order = %v, want the two untouched targets", result.Order)
	}

	// A different model on the same provider is NOT affected.
	if m.ModelDisabled("zhipu#a", "other-model") {
		t.Fatal("disable leaked across models")
	}

	// ReplaceGeneration clears generation-scoped state but keeps the override
	// (reload preservation — same for pins).
	m.ReplaceGeneration(9)
	if !m.ModelDisabled("zhipu#b", "glm") {
		t.Fatal("ReplaceGeneration cleared the operator disabled-model override (reload keeps it)")
	}
	m.SetModelDisabled("zhipu", "glm", true)
	m.ReplaceGeneration(10)
	if !m.ModelDisabled("zhipu", "glm") {
		t.Fatal("ReplaceGeneration cleared the operator disabled-model override (reload keeps it)")
	}
}

// TestRestoreDisabledModelsSeedsOverride pins the construction-time seed:
// the persisted projection (provider→models) becomes live override entries
// (exact + parent-keyed lookups), merges with in-memory toggles, skips junk
// keys, and a nil/empty map is a no-op (first run).
func TestRestoreDisabledModelsSeedsOverride(t *testing.T) {
	m := &Manager{}
	m.SetModelDisabled("kimi", "k3", true) // pre-existing in-memory toggle
	m.RestoreDisabledModels(map[string][]string{
		"zhipu": {"glm-4.7", "glm-4.6"},
		"junk":  {""},
		"":      {"orphan"},
	})
	for _, pair := range []struct{ provider, model string }{
		{"zhipu", "glm-4.7"}, {"zhipu", "glm-4.6"}, {"kimi", "k3"},
	} {
		if !m.ModelDisabled(pair.provider, pair.model) {
			t.Fatalf("restored pair %v not disabled", pair)
		}
	}
	// Parent-keyed restore covers pooled virtuals in scheduling.
	result := m.DecideOrder(disabledTestInput(
		Target{Provider: "zhipu#a", Parent: "zhipu", Model: "glm-4.7", Priority: 1},
		Target{Provider: "other", Model: "glm-4.7", Priority: 2},
	))
	if len(result.Order) != 1 || result.Order[0] != 1 {
		t.Fatalf("restored parent-key disable order = %v, want only [other]", result.Order)
	}
	// Junk keys were skipped; projection stays clean.
	if got := m.DisabledModels(); !reflect.DeepEqual(got, map[string][]string{
		"kimi":  {"k3"},
		"zhipu": {"glm-4.6", "glm-4.7"},
	}) {
		t.Fatalf("DisabledModels() after restore = %v", got)
	}
	// nil and empty maps are no-ops.
	m.RestoreDisabledModels(nil)
	m.RestoreDisabledModels(map[string][]string{})
	if !m.ModelDisabled("zhipu", "glm-4.7") {
		t.Fatal("no-op restore cleared an entry")
	}
}

// TestPreviewOrderExcludesDisabledTargets pins the detached-preview parity:
// Dashboard() carries the override into the snapshot and PreviewOrder applies
// the same candidate filter the live DecideOrder does.
func TestPreviewOrderExcludesDisabledTargets(t *testing.T) {
	m := &Manager{}
	m.SetModelDisabled("zhipu", "glm", true)
	now := time.Now()
	snapshot := m.Dashboard(now)
	result := snapshot.PreviewOrder(disabledTestInput(
		Target{Provider: "zhipu#a", Parent: "zhipu", Model: "glm", Priority: 1},
		Target{Provider: "other", Model: "glm", Priority: 2},
	))
	if len(result.Order) != 1 {
		t.Fatalf("preview order = %v, want only the non-disabled target", result.Order)
	}

	// A snapshot captured BEFORE the disable keeps its detached view (the
	// override applies from the next Dashboard capture).
	fresh := &Manager{}
	stale := fresh.Dashboard(now)
	fresh.SetModelDisabled("zhipu", "glm", true)
	if got := stale.PreviewOrder(disabledTestInput(Target{Provider: "zhipu", Model: "glm"})); len(got.Order) != 1 {
		t.Fatalf("stale snapshot order = %v, want the target (detachment)", got.Order)
	}
	if got := fresh.Dashboard(now).PreviewOrder(disabledTestInput(Target{Provider: "zhipu", Model: "glm"})); len(got.Order) != 0 {
		t.Fatalf("fresh snapshot order = %v, want empty (disabled)", got.Order)
	}
}
