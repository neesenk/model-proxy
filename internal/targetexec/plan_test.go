package targetexec

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

func TestPlanOwnsWirePreparation(t *testing.T) {
	plan := NewPlan(PlanInput{
		Target: configdomain.RouteTarget{Model: "claude"},
		ProviderConfig: configdomain.Provider{
			Provider:         "test",
			OpenAIBaseURL:    "https://chat.example/v1",
			AnthropicBaseURL: "https://anthropic.example",
		},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.Anthropic,
		ClientPath:      "/chat/completions",
		ImageOK:         true,
	})
	if plan.BaseURL() != "https://anthropic.example" || plan.UpstreamPath() != "/v1/messages" {
		t.Fatalf("wire endpoint = %q %q", plan.BaseURL(), plan.UpstreamPath())
	}
	body := plan.RewriteModel([]byte(`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`), "alias")
	body, err := plan.ConvertBody(body)
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

func TestPlanAppliesConfiguredHeadersWithoutExposingConfig(t *testing.T) {
	providerConfig := configdomain.Provider{
		Provider: "test",
		Headers:  map[string]string{"x-upstream-feature": "enabled"},
	}
	plan := NewPlan(PlanInput{
		ProviderConfig: providerConfig,
	})
	providerConfig.Headers["x-upstream-feature"] = "mutated"
	header := http.Header{}
	plan.ApplyConfiguredHeaders(header)
	if got := header.Get("x-upstream-feature"); got != "enabled" {
		t.Fatalf("configured header = %q, want enabled", got)
	}
	if plan.ProviderID() != "test" {
		t.Fatalf("provider id = %q, want test", plan.ProviderID())
	}
}

func TestPlanPreservesSameProtocolBytesAndEndpoint(t *testing.T) {
	in := []byte("{  \"model\" : \"same\", \"messages\" : [] }\n")
	plan := NewPlan(PlanInput{
		Target:         configdomain.RouteTarget{Model: "same"},
		ProviderConfig: configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://chat.example/v1"},
		ClientProtocol: protocol.OpenAI, BackendProtocol: protocol.OpenAI,
		ClientPath: "/chat/completions", ImageOK: true,
	})
	rewritten := plan.RewriteModel(in, "same")
	got, err := plan.ConvertBody(rewritten)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, in) {
		t.Fatalf("same-protocol plan changed bytes:\n got: %q\nwant: %q", got, in)
	}
	if plan.BaseURL() != "https://chat.example/v1" || plan.UpstreamPath() != "/chat/completions" {
		t.Fatalf("wire endpoint = %q %q", plan.BaseURL(), plan.UpstreamPath())
	}
}

func TestPlanPreservesBodyWhenTargetModelEmptyOrBodyMalformed(t *testing.T) {
	in := []byte(`{"model":"called","messages":[]}`)
	emptyTarget := NewPlan(PlanInput{ClientProtocol: protocol.OpenAI, BackendProtocol: protocol.OpenAI})
	if got := emptyTarget.RewriteModel(in, "called"); !bytes.Equal(got, in) {
		t.Fatalf("empty target changed body: %s", got)
	}

	malformed := []byte(`not-json`)
	rewriting := NewPlan(PlanInput{
		Target:         configdomain.RouteTarget{Model: "upstream"},
		ClientProtocol: protocol.OpenAI, BackendProtocol: protocol.OpenAI,
	})
	if got := rewriting.RewriteModel(malformed, "called"); !bytes.Equal(got, malformed) {
		t.Fatalf("malformed body changed: %s", got)
	}
}

func TestPlanExtractResponseTextByBackendProtocol(t *testing.T) {
	tests := []struct {
		name    string
		backend protocol.Protocol
		body    string
		want    string
	}{
		{name: "anthropic", backend: protocol.Anthropic, body: `{"content":[{"type":"text","text":"alpha"}]}`, want: "alpha"},
		{name: "chat", backend: protocol.OpenAI, body: `{"choices":[{"message":{"content":"beta"}}]}`, want: "beta"},
		{name: "responses", backend: protocol.Responses, body: `{"output":[{"type":"message","content":[{"type":"output_text","text":"gamma"}]}]}`, want: "gamma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := NewPlan(PlanInput{BackendProtocol: tt.backend})
			if got := plan.ExtractResponseText([]byte(tt.body)); got != tt.want {
				t.Fatalf("ExtractResponseText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanSameProtocolCodexPassthrough(t *testing.T) {
	in := []byte(`{"model":"gpt-x","input":"hi","max_output_tokens":10,"temperature":0.2,"top_p":0.9}`)
	plan := NewPlan(PlanInput{
		ProviderConfig:  configdomain.Provider{Provider: "codex"},
		ClientProtocol:  protocol.Responses,
		BackendProtocol: protocol.Responses,
		ClientPath:      "/v1/responses",
		ImageOK:         true,
	})
	got, err := plan.ConvertBody(in)
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("same-protocol body = %s, err = %v", got, err)
	}
}
