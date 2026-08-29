package routing

import (
	"bytes"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

var benchmarkBody1K = bytes.Repeat(
	[]byte("The quick brown fox jumps over the lazy dog. "),
	25,
)

// passthroughScheduler returns the candidate pool unchanged.
type passthroughScheduler struct{}

func (passthroughScheduler) Schedule(
	_ string,
	_ string,
	targets []configdomain.RouteTarget,
) []configdomain.RouteTarget {
	return targets
}

func BenchmarkEstimateInputTokens(b *testing.B) {
	b.SetBytes(int64(len(benchmarkBody1K)))
	for b.Loop() {
		_ = EstimateInputTokens(benchmarkBody1K)
	}
}

func BenchmarkProfileRequest(b *testing.B) {
	body := []byte(`{"messages":[{"content":"` +
		strings.Repeat("中英文 prompt ", 2048) +
		`"},{"type":"image","source":{"type":"base64","data":"` +
		strings.Repeat("A", 256*1024) +
		`"}}],"tools":[{"name":"lookup"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		_ = ProfileRequest(body)
	}
}

func BenchmarkPlannerApply(b *testing.B) {
	models := make(map[string]catalog.Model, 32)
	expanded := make(map[string][]configdomain.RouteTarget, 32)
	for index := 0; index < 32; index++ {
		name := "model-" + string(rune('a'+index))
		models[name] = catalog.Model{
			Context: 32000,
			Modalities: catalog.Modalities{
				Input: []string{"text", "image"},
			},
			ToolCall: true,
		}
		expanded[name] = []configdomain.RouteTarget{{
			Provider: "provider-" + name,
			Model:    name,
			Priority: index,
		}}
	}
	cat := catalog.New(models)
	ordered := expanded["model-a"]
	planner := NewPlanner(PlannerInput{
		Catalog:        cat,
		ExpandedRoutes: expanded,
		Scheduler:      passthroughScheduler{},
	})
	body := []byte(`{"messages":[{"content":[{"type":"image"}]}],"tools":[{"name":"lookup"}]}`)
	b.ReportAllocs()
	for b.Loop() {
		_ = planner.ApplyWithProfile("public", "session", ordered, ProfileRequest(body))
	}
}
