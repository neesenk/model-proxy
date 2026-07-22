package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeModels_ListsExposedModels verifies /v1/models lists exposed model
// names (routes' keys) plus claude_mapping aliases.
func TestServeModels_ListsExposedModels(t *testing.T) {
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp",
				Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
		ClaudeMapping: map[string]string{
			"claude-opus-4-7":  "glm-5.2",
			"claude-haiku-4-5": "glm-5.2",
		},
	}
	p := newTestProxy(t, cfg)
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
	if list.Object != "list" || len(list.Data) != 3 {
		t.Errorf("expected 3 models (1 route + 2 claude aliases), got %+v", list)
	}
	ids := map[string]bool{}
	for _, m := range list.Data {
		ids[m.ID] = true
	}
	for _, want := range []string{"glm-5.2", "claude-opus-4-7", "claude-haiku-4-5"} {
		if !ids[want] {
			t.Errorf("expected %s in %v", want, ids)
		}
	}
}

// TestServeModels_NoRoutesReturnsEmpty verifies /v1/models returns an empty list
// when there are no routes.
func TestServeModels_NoRoutesReturnsEmpty(t *testing.T) {
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"other": {OpenAIBaseURL: "http://x", Provider: "static"},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
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
