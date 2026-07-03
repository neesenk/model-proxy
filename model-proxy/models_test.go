package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeModels_ListsExposedModels verifies /v1/models lists the exposed model
// names from routes (not upstream forwarding — the proxy lists what it exposes).
func TestServeModels_ListsExposedModels(t *testing.T) {
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Auth:   AuthCfg{StaticKey: "test-cqp-key"},
		Providers: map[string]Provider{
			"compass": {BaseURL: "http://x", Provider: "compass",
				Models: map[string]ProviderModel{"glm-5.2": {Context: 1048576}}},
		},
		Routes: map[string]ProtocolRoute{
			"anthropic": {Models: map[string]string{
				"claude-opus-4-7": "compass/glm-5.2",
				"claude-haiku-4-5": "compass/glm-5.2",
			}},
		},
	}
	p := NewProxy(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if list.Object != "list" || len(list.Data) != 2 {
		t.Errorf("expected 2 exposed models, got %+v", list)
	}
	// Check both exposed names are present.
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	if !ids["claude-opus-4-7"] || !ids["claude-haiku-4-5"] {
		t.Errorf("expected claude-opus-4-7 + claude-haiku-4-5, got %v", ids)
	}
}

// TestServeModels_NoCQPRouteFallsBackToEmpty verifies that with no cqp route,
// /v1/models returns an empty list when no routes have models.
func TestServeModels_NoCQPRouteFallsBackToEmpty(t *testing.T) {
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"other": {BaseURL: "http://x", Provider: "static"},
		},
		Routes: map[string]ProtocolRoute{
			"anthropic": {Models: map[string]string{}},
		},
	}
	p := NewProxy(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	var list struct {
		Object string `json:"object"`
		Data   []any  `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	if list.Object != "list" || len(list.Data) != 0 {
		t.Errorf("expected empty list, got %+v", list)
	}
}
