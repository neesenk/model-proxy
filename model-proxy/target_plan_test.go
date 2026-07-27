package main

import (
	"encoding/json"
	"testing"
)

func TestTargetPlanOwnsWirePreparation(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{
			"up": {
				Provider:         "static",
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

	body := plan.rewriteModel([]byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`), "alias")
	body, err = plan.convertBody(body)
	if err != nil {
		t.Fatal(err)
	}
	var converted map[string]any
	if err := json.Unmarshal(body, &converted); err != nil {
		t.Fatal(err)
	}
	if converted["model"] != "claude" || converted["max_tokens"] == nil {
		t.Fatalf("converted target body = %s", body)
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
