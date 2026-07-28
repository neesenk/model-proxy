package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestForward_Converted4xxUsesClientErrorEnvelope(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"unsupported field","type":"invalid_request_error","code":"bad_request"},"request_id":"req_up"}`)
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x", Protocol: "openai"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "error" || strOf(asMap(out["error"])["message"]) != "unsupported field" {
		t.Fatalf("body is not an Anthropic error envelope: %s", body)
	}
}

func TestConvertBody_CodexSameProtocolResponsesPassthrough(t *testing.T) {
	in := []byte(`{"model":"gpt-x","input":"hi","max_output_tokens":10,"temperature":0.2,"top_p":0.9,"store":true}`)
	plan := targetPlan{
		providerCfg:  Provider{Provider: "codex"},
		clientProto:  "responses",
		backendProto: "responses",
		imageOK:      true,
	}
	got, err := plan.convertBody(in)
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("same-protocol codex request must stay byte-identical: %s, %v", got, err)
	}

	plain := targetPlan{
		providerCfg:  Provider{Provider: testProviderID},
		clientProto:  "responses",
		backendProto: "responses",
		imageOK:      true,
	}
	got, err = plain.convertBody(in)
	if err != nil || !bytes.Equal(got, in) {
		t.Fatalf("non-codex same-protocol request must stay byte-identical: %s, %v", got, err)
	}
}
