package app

import "testing"

func TestTargetPlanOwnsWirePreparation(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {
				Provider:         testProviderID,
				OpenAIBaseURL:    "https://chat.example/v1",
				AnthropicBaseURL: "https://anthropic.example",
			},
		},
	})
	plan, err := p.planTarget(targetPlanInput{
		runtime:     p.SnapshotRuntime(),
		target:      RouteTarget{Provider: "up", Model: "claude", Protocol: "anthropic"},
		clientProto: "openai",
		clientPath:  "/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.BackendProtocol() != "anthropic" ||
		plan.BaseURL() != "https://anthropic.example" ||
		plan.UpstreamPath() != "/v1/messages" ||
		plan.Provider() == nil {
		t.Fatalf("unexpected target plan: %+v", plan)
	}

	if plan.Target() != (RouteTarget{Provider: "up", Model: "claude", Protocol: "anthropic"}) ||
		plan.ProviderID() != testProviderID {
		t.Fatalf("root target facts not frozen in plan: %+v", plan)
	}
}

func TestTargetPlanRejectsUnknownProvider(t *testing.T) {
	p := newTestProxy(t, &Config{})
	if _, err := p.planTarget(targetPlanInput{
		runtime: p.SnapshotRuntime(),
		target:  RouteTarget{Provider: "missing", Model: "m"},
	}); err == nil {
		t.Fatal("unknown provider unexpectedly produced a target plan")
	}
}
