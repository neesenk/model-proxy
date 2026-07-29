package fusion

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
)

type enginePorts struct {
	mu            sync.Mutex
	supportsTools bool
	legs          map[string]LegResult
	calls         []LegCall
	synthTarget   configdomain.RouteTarget
	synthBody     []byte
	synthesis     SynthesisResult
}

func (ports *enginePorts) SupportsTools(configdomain.RouteTarget) bool {
	return ports.supportsTools
}

func (ports *enginePorts) CallLeg(_ context.Context, call LegCall) LegResult {
	ports.mu.Lock()
	ports.calls = append(ports.calls, call)
	result := ports.legs[call.Target.Provider]
	ports.mu.Unlock()
	result.Index = call.Index
	if result.Provider == "" {
		result.Provider = call.Target.Provider
	}
	if result.Model == "" {
		result.Model = call.Target.Model
	}
	return result
}

func (ports *enginePorts) Synthesize(target configdomain.RouteTarget, body []byte) SynthesisResult {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.synthTarget = target
	ports.synthBody = append([]byte(nil), body...)
	return ports.synthesis
}

func TestEngineOrchestratesJudgeAndRecordsSynthesis(t *testing.T) {
	registry := NewRegistry()
	ports := &enginePorts{
		supportsTools: true,
		legs: map[string]LegResult{
			"a":     {Text: "draft A", Status: 200, Usage: Usage{Input: 2, Output: 1}},
			"b":     {Text: "draft B", Status: 200, Usage: Usage{Input: 3, Output: 1}},
			"judge": {Text: "judge report", Status: 200, Usage: Usage{Input: 4, Output: 2}},
		},
		synthesis: SynthesisResult{Committed: true, Status: 200, LatencyMs: 17, Input: 8, Output: 5},
	}
	judge := configdomain.RouteTarget{Provider: "judge", Model: "review"}
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), Request{
		Workflow:     "quality",
		RunID:        "request-1",
		Route:        "hard",
		Agent:        "test",
		Protocol:     "anthropic",
		OriginalBody: []byte(`{"messages":[{"role":"user","content":"solve"}]}`),
		Recipe: configdomain.FusionConfig{
			Panel: []configdomain.RouteTarget{
				{Provider: "a", Model: "ma"},
				{Provider: "b", Model: "mb"},
			},
			Synthesizer: configdomain.RouteTarget{Provider: "synth", Model: "final"},
			Judge:       &judge,
		},
	}, ports)
	if !result.Committed || !result.Run.SynthCommitted || result.Run.SynthStatus != 200 ||
		result.Run.SynthLatencyMs != 17 || result.Run.SynthInput != 8 || result.Run.SynthOutput != 5 {
		t.Fatalf("synthesis result = %+v", result)
	}
	if result.Run.DraftsUsed != 2 || result.Run.Quorum != 2 || !result.Run.JudgeUsed || len(result.Run.Legs) != 3 {
		t.Fatalf("run observation = %+v", result.Run)
	}
	for _, text := range [][]byte{[]byte("draft A"), []byte("draft B"), []byte("judge report")} {
		if !bytes.Contains(ports.synthBody, text) {
			t.Errorf("synthesis body missing %q: %s", text, ports.synthBody)
		}
	}
	if ports.synthTarget.Provider != "synth" || ports.synthTarget.Model != "final" {
		t.Errorf("synthesis target = %+v", ports.synthTarget)
	}
	stats, runs := registry.Snapshot("quality", time.Now())
	if stats["quality"].Runs != 1 || stats["quality"].QuorumMet != 1 || len(runs) != 1 ||
		runs[0].RunID != "request-1" {
		t.Fatalf("registry state = %+v / %+v", stats, runs)
	}
}

func TestEngineToolGateSkipsPanelAndBudget(t *testing.T) {
	registry := NewRegistry()
	ports := &enginePorts{
		supportsTools: false,
		legs:          map[string]LegResult{},
		synthesis:     SynthesisResult{Committed: true, Status: 200},
	}
	original := []byte(`{"messages":[],"tools":[{"name":"x"}]}`)
	result := (Engine{Registry: registry}).Run(t.Context(), Request{
		Workflow:     "quality",
		RunID:        "request-2",
		Route:        "hard",
		Protocol:     "openai",
		OriginalBody: original,
		HasTools:     true,
		Recipe: configdomain.FusionConfig{
			Panel: []configdomain.RouteTarget{
				{Provider: "a", Model: "ma"},
				{Provider: "b", Model: "mb"},
			},
			Synthesizer:   configdomain.RouteTarget{Provider: "synth", Model: "final"},
			MaxRunsPerDay: 1,
		},
	}, ports)
	if result.Run.Degraded != DegradedToolsUnsupported || !bytes.Equal(ports.synthBody, original) {
		t.Fatalf("tool-gated run = %+v body=%s", result.Run, ports.synthBody)
	}
	if len(ports.calls) != 0 {
		t.Fatalf("tool gate executed panel calls: %+v", ports.calls)
	}
	stats, _ := registry.Snapshot("quality", time.Now())
	if stats["quality"].RunsToday != 0 {
		t.Fatalf("tool gate consumed daily budget: %+v", stats["quality"])
	}
}

func TestEngineQuorumFailureCancelsAndSynthesizesOriginal(t *testing.T) {
	registry := NewRegistry()
	cancelSeen := make(chan struct{}, 2)
	ports := &blockingEnginePorts{
		cancelSeen: cancelSeen,
		synthesis:  SynthesisResult{Committed: true, Status: 200},
	}
	original := []byte(`{"messages":[]}`)
	result := (Engine{Registry: registry, GracePeriod: time.Second}).Run(t.Context(), Request{
		Workflow:     "quality",
		RunID:        "request-3",
		Route:        "hard",
		Protocol:     "openai",
		OriginalBody: original,
		Recipe: configdomain.FusionConfig{
			Panel: []configdomain.RouteTarget{
				{Provider: "fail", Model: "m0"},
				{Provider: "wait-a", Model: "m1"},
				{Provider: "wait-b", Model: "m2"},
			},
			MinPanel:    3,
			Synthesizer: configdomain.RouteTarget{Provider: "synth", Model: "final"},
		},
	}, ports)
	if result.Run.Degraded != DegradedInsufficientProposers || !bytes.Equal(ports.synthBody, original) {
		t.Fatalf("quorum-failed run = %+v body=%s", result.Run, ports.synthBody)
	}
	for i := 0; i < 2; i++ {
		select {
		case <-cancelSeen:
		case <-time.After(time.Second):
			t.Fatal("quorum failure did not cancel an in-flight panel leg")
		}
	}
}

type blockingEnginePorts struct {
	mu         sync.Mutex
	cancelSeen chan<- struct{}
	synthBody  []byte
	synthesis  SynthesisResult
}

func (*blockingEnginePorts) SupportsTools(configdomain.RouteTarget) bool { return true }

func (ports *blockingEnginePorts) CallLeg(ctx context.Context, call LegCall) LegResult {
	if call.Target.Provider == "fail" {
		return LegResult{
			Index:    call.Index,
			Provider: call.Target.Provider,
			Model:    call.Target.Model,
			Err:      errors.New("failed"),
		}
	}
	<-ctx.Done()
	ports.cancelSeen <- struct{}{}
	return LegResult{
		Index:    call.Index,
		Provider: call.Target.Provider,
		Model:    call.Target.Model,
		Err:      ctx.Err(),
	}
}

func (ports *blockingEnginePorts) Synthesize(_ configdomain.RouteTarget, body []byte) SynthesisResult {
	ports.mu.Lock()
	defer ports.mu.Unlock()
	ports.synthBody = append([]byte(nil), body...)
	return ports.synthesis
}
