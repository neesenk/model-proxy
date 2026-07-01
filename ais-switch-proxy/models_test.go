package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestServeModels_ForwardsToUpstream verifies /v1/models forwards to the cqp
// route's upstream at /models with CQP bearer auth, and returns the upstream's
// model list verbatim.
func TestServeModels_ForwardsToUpstream(t *testing.T) {
	var gotAuth string
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model","owned_by":"MaaS","context_window":1048576}]}`))
	}))
	defer up.Close()

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Auth:   AuthCfg{StaticKey: "test-cqp-key"},
		Routes: []Route{{
			Name:         "claude",
			PathPrefixes: []string{"/v1/messages"},
			Upstream:     up.URL,
			Auth:         "cqp",
			ModelMap:     map[string]string{"claude-opus-4-7": "glm-5.2"},
		}},
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

	if gotPath != "/models" {
		t.Errorf("upstream path=%q want /models", gotPath)
	}
	if gotAuth != "Bearer test-cqp-key" {
		t.Errorf("auth=%q want Bearer test-cqp-key", gotAuth)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &list); err != nil {
		t.Fatalf("parse: %v body=%s", err, string(body))
	}
	if list.Object != "list" || len(list.Data) != 1 || list.Data[0].ID != "glm-5.2" {
		t.Errorf("unexpected list: %+v", list)
	}
}

// TestServeModels_NoCQPRouteFallsBackToEmpty verifies that with no cqp route,
// /v1/models returns an empty list rather than erroring.
func TestServeModels_NoCQPRouteFallsBackToEmpty(t *testing.T) {
	cfg := &Config{
		Listen: "127.0.0.1:0",
		Routes: []Route{{
			Name: "other", PathPrefixes: []string{"/v1beta"},
			Upstream: "http://x", Auth: "static",
		}},
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
