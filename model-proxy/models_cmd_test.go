package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestFetchModels hits a mock upstream and verifies CQP bearer auth + parsing.
func TestFetchModels(t *testing.T) {
	var gotAuth, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[
			{"id":"glm-5.2","object":"model","owned_by":"MaaS","context_window":1048576},
			{"id":"deepseek-v4-pro","object":"model","owned_by":"MaaS","context_window":1048576}
		]}`))
	}))
	defer up.Close()

	cfg := &Config{
		Auth: AuthCfg{StaticKey: "test-key"},
		Providers: map[string]Provider{"compass": {BaseURL: up.URL, Auth: "cqp"}},
	}
	entries, err := fetchModels(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/models" {
		t.Errorf("path=%q want /models", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("auth=%q", gotAuth)
	}
	if len(entries) != 2 || entries[0].ID != "glm-5.2" || entries[0].ContextWindow != 1048576 {
		t.Errorf("entries=%+v", entries)
	}
}

func TestFetchModels_NoCQPRoute(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{"g": {BaseURL: "http://x", Auth: "static"}}}
	if _, err := fetchModels(cfg); err == nil {
		t.Error("expected error for no cqp route")
	}
}

// TestModelsCache_SaveLoad round-trips the cache file.
func TestModelsCache_SaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	entries := []ModelEntry{{ID: "glm-5.2", OwnedBy: "MaaS", ContextWindow: 1048576, Object: "model"}}
	if err := saveModelsCache(path, entries); err != nil {
		t.Fatal(err)
	}
	// File perms.
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("perm=%o want 0600", fi.Mode().Perm())
	}
	c, err := loadModelsCache(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Data) != 1 || c.Data[0].ID != "glm-5.2" {
		t.Errorf("loaded=%+v", c)
	}
	if c.CachedAt.IsZero() {
		t.Error("cached_at not set")
	}
}

// TestModelsCache_AbsentReturnsNil verifies a missing cache file is not an error.
func TestModelsCache_AbsentReturnsNil(t *testing.T) {
	c, err := loadModelsCache(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c != nil {
		t.Errorf("expected nil, got %+v", c)
	}
}

// TestModelsRefreshInterval verifies parsing + default fallback.
func TestModelsRefreshInterval(t *testing.T) {
	cases := []struct{ in string; want time.Duration }{
		{"", defaultModelsRefreshInterval},
		{"30m", 30 * time.Minute},
		{"2h", 2 * time.Hour},
		{"bad", defaultModelsRefreshInterval},
		{"-1s", defaultModelsRefreshInterval},
	}
	for _, tc := range cases {
		got := modelsRefreshInterval(&Config{ModelsRefreshInterval: tc.in})
		if got != tc.want {
			t.Errorf("modelsRefreshInterval(%q)=%s want %s", tc.in, got, tc.want)
		}
	}
}

// TestModelsCachePath_Default verifies the default lands next to the SSO cookie file.
func TestModelsCachePath_Default(t *testing.T) {
	cfg := &Config{Auth: AuthCfg{SSOCookieFile: "/home/u/.model-proxy/google_oauth_auth.json"}}
	got := modelsCachePath(cfg)
	want := "/home/u/.model-proxy/model-proxy-models.json"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	// Explicit override.
	cfg.ModelsCacheFile = "/var/cache/m.json"
	if got := modelsCachePath(cfg); got != "/var/cache/m.json" {
		t.Errorf("override: got %q", got)
	}
}

// TestPrintModels does a smoke render (just ensure no panic + output).
func TestPrintModels(t *testing.T) {
	// Capture stdout via a pipe is overkill; just call it, it writes to os.Stdout.
	printModels([]ModelEntry{{ID: "glm-5.2", OwnedBy: "MaaS", ContextWindow: 1048576}}, time.Now(), "/tmp/m.json")
}

// TestRefreshModelsCache verifies fetch+save through the cache path.
func TestRefreshModelsCache(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"x","object":"model","owned_by":"MaaS","context_window":1024}]}`))
	}))
	defer up.Close()
	cfg := &Config{Auth: AuthCfg{StaticKey: "k"}, Providers: map[string]Provider{"c": {BaseURL: up.URL, Auth: "cqp"}}}
	path := filepath.Join(t.TempDir(), "m.json")
	entries, err := refreshModelsCache(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "x" {
		t.Errorf("entries=%+v", entries)
	}
	// Cache file written.
	b, _ := os.ReadFile(path)
	var c modelsCacheFile
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Data) != 1 {
		t.Errorf("cache data len=%d", len(c.Data))
	}
}
