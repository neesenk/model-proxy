package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// --- refreshProviderModels: pool must fetch ONCE (not per-account) ---

// TestRefreshProviderModelsOnceForPool verifies that refreshing a pooled parent
// hits the upstream /models exactly once. The model list is per-upstream, not
// per-account, so fanning out across the pool is wasted work. The first virtual
// (id-sorted) is sufficient.
func TestRefreshProviderModelsOnceForPool(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "K1", "K2", "K3")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5","object":"model"}]}`))
	}))
	defer srv.Close()
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: srv.URL + "/v1", Provider: "zhipu"}}}
	entries, err := refreshProviderModels(cfg, "zhipu")
	if err != nil {
		t.Fatalf("refreshProviderModels: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/models hit %d times, want 1 (pool must not fan out)", got)
	}
	if len(entries) == 0 || entries[0] != "glm-5" {
		t.Fatalf("entries = %v, want [glm-5]", entries)
	}
}

// TestRefreshProviderModels_SingleAccount verifies the non-pooled path: a
// single-account (or legacy singular-file) provider fetches exactly once via
// its plain name.
func TestRefreshProviderModels_SingleAccount(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Write the legacy singular file (1 account → not pooled).
	credDir := dir + "/.model-proxy"
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credDir+"/zhipu_apikey.json", []byte(`{"api_key":"SOLE-KEY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","object":"model"}]}`))
	}))
	defer srv.Close()
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: srv.URL + "/v1", Provider: "zhipu"}}}
	entries, err := refreshProviderModels(cfg, "zhipu")
	if err != nil {
		t.Fatalf("refreshProviderModels: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/models hit %d times, want 1", got)
	}
	if len(entries) != 1 || entries[0] != "glm-5.2" {
		t.Fatalf("entries = %v, want [glm-5.2]", entries)
	}
}

// TestRefreshProviderModels_Unknown asserts the unknown-provider error path.
func TestRefreshProviderModels_Unknown(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	cfg := &Config{Providers: map[string]Provider{"a": {Provider: "static"}}}
	_, err := refreshProviderModels(cfg, "nope")
	if err == nil || !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("unknown provider: err=%v want 'unknown provider'", err)
	}
}

// TestPoolVirtuals verifies poolVirtuals resolves a multi-account parent to its
// sorted virtual ids, and returns false for single-account / unknown providers.
func TestPoolVirtuals(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-C", "KEY-B")
	cfg := &Config{Providers: map[string]Provider{"zhipu": {Provider: "zhipu"}}}

	vids, pooled := poolVirtuals(cfg, "zhipu")
	if !pooled {
		t.Fatal("pooled=true want true for 3-account pool")
	}
	if len(vids) != 3 {
		t.Fatalf("len(vids)=%d want 3", len(vids))
	}
	// vids must be sorted (id-derived suffixes, not the raw keys).
	for i := 1; i < len(vids); i++ {
		if vids[i-1] >= vids[i] {
			t.Errorf("vids not sorted: %v", vids)
		}
	}
	// Each virtual id carries the parent prefix.
	for _, v := range vids {
		if !strings.HasPrefix(v, "zhipu#") {
			t.Errorf("virtual %q missing zhipu# prefix", v)
		}
	}

	// Single account → not pooled.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	os.MkdirAll(home2+"/.model-proxy", 0o700)
	os.WriteFile(home2+"/.model-proxy/zhipu_apikey.json", []byte(`{"api_key":"K"}`), 0o600)
	if _, pooled := poolVirtuals(cfg, "zhipu"); pooled {
		t.Error("single account: pooled=true want false")
	}

	// Unknown provider → not pooled.
	if _, pooled := poolVirtuals(cfg, "nope"); pooled {
		t.Error("unknown provider: pooled=true want false")
	}
}
