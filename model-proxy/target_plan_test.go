package main

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
		runtime:     p.snapshotRuntime(),
		target:      RouteTarget{Provider: "up", Model: "claude", Protocol: "anthropic"},
		clientProto: "openai",
		clientPath:  "/chat/completions",
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.backendProto != "anthropic" ||
		plan.baseURL != "https://anthropic.example" ||
		plan.upPath != "/v1/messages" ||
		plan.providerImpl == nil {
		t.Fatalf("unexpected target plan: %+v", plan)
	}

	if plan.wire.BaseURL() != plan.baseURL || plan.wire.UpstreamPath() != plan.upPath {
		t.Fatalf("root target plan did not preserve wire plan: %+v", plan)
	}
}

func TestTargetPlanRejectsUnknownProvider(t *testing.T) {
	p := newTestProxy(t, &Config{})
	if _, err := p.planTarget(targetPlanInput{
		runtime: p.snapshotRuntime(),
		target:  RouteTarget{Provider: "missing", Model: "m"},
	}); err == nil {
		t.Fatal("unknown provider unexpectedly produced a target plan")
	}
}
